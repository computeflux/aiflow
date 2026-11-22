package workflows

import (
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// WeeklyContentInput 是周内容 workflow 的输入（S2 §3.2 审批卡点示范）。
type WeeklyContentInput struct {
	// Week 是周标识（如 "2026-W40"），workflow ID 的一部分（同一周重复触发走 reuse policy）
	Week string `json:"week"`
	// Source 素材来源：x / github / reddit / xiaohongshu
	Source string `json:"source"`
	// Keyword 素材搜索关键词
	Keyword string `json:"keyword"`
	// Limit 抓取素材条数（<=0 默认 20）
	Limit int `json:"limit"`
	// Platform 发布平台（x / xiaohongshu）
	Platform string `json:"platform"`
	// AccountID 发布账号
	AccountID string `json:"account_id"`
	// ApprovalWaitHours 审批等待超时（小时，<=0 默认 24；支持小数便于验收超时路径）：
	// 超时自动重跑一次生成并重新等待；二次超时以 timeout 结束（无默认通过）。
	ApprovalWaitHours float64 `json:"approval_wait_hours"`
	// SkipFetch 跳过素材抓取（素材由外部提供 / 审批卡点演示 / 抓取链路故障时）
	SkipFetch bool `json:"skip_fetch"`
}

// approvalDecision 是审批门的结果（Signal 到达 / 超时）。
// Signal payload 由审批桥发送（map[string]any：approver / reason / approval_no，见 internal/approval/cd.go）。
type approvalDecision struct {
	Approved bool
	Approver string
	Reason   string
}

// WeeklyContentResult 是 workflow 终态。
// status：published / rejected / timeout / skipped（无素材）。
type WeeklyContentResult struct {
	Status      string `json:"status"`
	Content     string `json:"content"`
	Approver    string `json:"approver"`
	Reason      string `json:"reason"`
	Regenerated bool   `json:"regenerated"` // 审批超时后是否重跑过生成
}

// WeeklyContentWorkflow 是 S2 刚性主干 + 审批卡点示范（docs/plan/S2-混合编排.md §3.2）：
//
//	fetch 素材 → generate 内容 → Signal 等人工审批（24h 超时自动重跑一次）→ publish
//
// 人工审批由 S1 approvals 统一处理：board 操作 → POST /approvals/:id/approve|reject
// → approval.RunCD / reject 路由 → Temporal Signal（approve/reject）。
// Agent 不参与主干（无 ExtractParams/ReportStatus 卡点，保持主干确定性）。
func WeeklyContentWorkflow(ctx workflow.Context, input WeeklyContentInput) (WeeklyContentResult, error) {
	logger := workflow.GetLogger(ctx)

	waitDur := 24 * time.Hour
	if input.ApprovalWaitHours > 0 {
		waitDur = time.Duration(input.ApprovalWaitHours * float64(time.Hour))
	}
	if input.Limit <= 0 {
		input.Limit = 20
	}

	// 1. fetch 素材（Python MCP 抓取 + Go 缓存落库/索引，复用既有链路；SkipFetch 时跳过）
	if !input.SkipFetch {
		workflowID := workflow.GetInfo(ctx).WorkflowExecution.ID
		var fr FetchResult
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 30 * time.Minute,
			TaskQueue:           pythonMCPTaskQueue,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		}), "FetchActivities.fetch", FetchInput{
			Source: input.Source, Keyword: input.Keyword, Limit: input.Limit, WorkflowID: workflowID,
		}).Get(ctx, &fr); err != nil {
			return WeeklyContentResult{}, fmt.Errorf("fetch material: %w", err)
		}
		var pr activities.ProcessCacheResult
		if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 60 * time.Minute,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		}), "ProcessFetchCache", activities.ProcessCacheInput{WorkflowID: workflowID + "-p0"}).Get(ctx, &pr); err != nil {
			return WeeklyContentResult{}, fmt.Errorf("process cache: %w", err)
		}
		if pr.Saved == 0 && fr.Total == 0 {
			logger.Info("no material fetched, skip")
			return WeeklyContentResult{Status: "skipped", Reason: "no material"}, nil
		}
	}

	// 2. generate 内容（Go LLM activity）
	generateContent := func() (string, error) {
		var gen activities.GenerateResult
		err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
			StartToCloseTimeout: 5 * time.Minute,
			RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
		}), "Generate", activities.GenerateInput{
			Topic:    fmt.Sprintf("weekly content %s", input.Week),
			Platform: input.Platform,
			Query:    input.Keyword,
		}).Get(ctx, &gen)
		if err != nil {
			return "", fmt.Errorf("generate: %w", err)
		}
		return gen.Content, nil
	}
	content, err := generateContent()
	if err != nil {
		return WeeklyContentResult{}, err
	}

	// 3. 人工审批卡点：Signal（approve/reject）+ 超时自动重跑一次。
	// 本 SDK 版本无 SetSignalHandler：Signal 经 GetSignalChannel 接收，
	// 与超时 Timer 的并发等待用 Selector（Future 不支持原生 select）。
	approveCh := workflow.GetSignalChannel(ctx, "approve")
	rejectCh := workflow.GetSignalChannel(ctx, "reject")
	signalDecision := func(approved bool, v interface{}) approvalDecision {
		d := approvalDecision{Approved: approved}
		if m, ok := v.(map[string]any); ok {
			if s, ok := m["approver"].(string); ok {
				d.Approver = s
			}
			if s, ok := m["reason"].(string); ok {
				d.Reason = s
			}
		}
		return d
	}
	waitApproval := func() (approvalDecision, bool) {
		timer := workflow.NewTimer(ctx, waitDur)
		var d approvalDecision
		var decided bool
		selector := workflow.NewSelector(ctx)
		selector.AddReceive(approveCh, func(ch workflow.ReceiveChannel, _ bool) {
			var v interface{}
			ch.Receive(ctx, &v)
			d, decided = signalDecision(true, v), true
		})
		selector.AddReceive(rejectCh, func(ch workflow.ReceiveChannel, _ bool) {
			var v interface{}
			ch.Receive(ctx, &v)
			d, decided = signalDecision(false, v), true
		})
		selector.AddFuture(timer, func(_ workflow.Future) { /* 超时：decided 保持 false */ })
		selector.Select(ctx)
		return d, decided
	}

	d, decided := waitApproval()
	regenerated := false
	if !decided {
		// 24h 超时 → 自动重跑一次生成（换一批新内容）→ 重新等待审批
		logger.Info("approval timeout, regenerating once", "wait", waitDur)
		content, err = generateContent()
		if err != nil {
			return WeeklyContentResult{}, err
		}
		regenerated = true
		d, decided = waitApproval()
	}
	if !decided {
		// 二次超时 → 结束（无默认通过）
		logger.Info("second approval timeout, ending without default pass")
		return WeeklyContentResult{Status: "timeout", Content: content, Regenerated: true}, nil
	}
	if !d.Approved {
		logger.Info("content rejected", "approver", d.Approver)
		return WeeklyContentResult{Status: "rejected", Content: content, Approver: d.Approver, Reason: d.Reason, Regenerated: regenerated}, nil
	}

	// 4. publish（Python MCP 发布；失败即 workflow 失败，无自主重试权）
	var pubRes map[string]any
	if err := workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		TaskQueue:           pythonMCPTaskQueue,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}), "PublishActivities.publish", PublishInput{
		Source: input.Platform, AccountID: input.AccountID, Content: content,
	}).Get(ctx, &pubRes); err != nil {
		return WeeklyContentResult{Status: "publish_failed", Content: content}, fmt.Errorf("publish: %w", err)
	}
	logger.Info("published", "platform", input.Platform)
	return WeeklyContentResult{Status: "published", Content: content, Approver: d.Approver, Reason: d.Reason, Regenerated: regenerated}, nil
}
