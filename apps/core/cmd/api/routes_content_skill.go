package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/workflows"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

// registerContentSkillRoutes 内容生产域·生成（social-generate / like / skill / weekly-content
// + S1 审批单）。逻辑自 registerContentRoutes 原样搬迁（批次 F 拆分，行为无变更）。
func registerContentSkillRoutes(
	r *gin.Engine,
	deps *appDeps,
	agentC client.Client,
	requireAuth func() gin.HandlerFunc,
	requireRole func(roles ...string) gin.HandlerFunc,
) {
	database := deps.database
	c := deps.temporal
	cfg := deps.cfg

	// P1 Skill 框架：注册表 + 调用网关（docs/plan/transform-plan.md §6）
	r.POST("/social-generate", func(ctx *gin.Context) {
		var input workflows.AutoTweetInput
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: cfg.TemporalGoTaskQueue, ID: fmt.Sprintf("social-x-%s", uuid.NewString())}, workflows.AutoTweetWorkflow, input)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var result workflows.AutoTweetResult
		if err := wr.Get(ctx, &result); err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, result)
	})

	// 点赞能力（M3-2）：唯一的 like 入口，执行链只有一条 ——
	// LikeSkillWorkflow → InvokeSkill → skillgateway → pyreplay skill 'like'。
	// 旧的 pyreplay worker 直连分支与 LIKE_VIA_SKILL 开关一并删除：点赞的节奏、账号池、
	// 限流与熔断全部由 skill 框架负责，API 层不再关心执行端跑在哪个进程。
	r.POST("/like", func(ctx *gin.Context) {
		var input workflows.LikeSkillInput
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: cfg.TemporalGoTaskQueue, ID: fmt.Sprintf("like-%s", uuid.NewString())}, workflows.LikeSkillWorkflow, input)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var result workflows.LikeSkillResult
		if err := wr.Get(ctx, &result); err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, result)
	})

	// P1：通用 skill 调用端点（docs/plan/transform-plan.md §6）。
	// 任何已注册 skill 均可经 Temporal 调用：
	// body {"skill_name": "generate", "input": {...}, "via_skill": true}
	// → GenericSkillWorkflow → ExecuteActivity("InvokeSkill", ...) → skillgateway
	// agent 也经同一个 skillgateway（POST /api/skills/:name/invoke）调用同一 skill。
	r.POST("/skill", func(ctx *gin.Context) {
		var input workflows.GenericSkillInput
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		wr, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{TaskQueue: cfg.TemporalGoTaskQueue, ID: fmt.Sprintf("skill-%s", uuid.NewString())}, workflows.GenericSkillWorkflow, input)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		var result map[string]any
		if err := wr.Get(ctx, &result); err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		ctx.JSON(http.StatusOK, result)
	})

	// S2 §3.2：周内容 workflow（审批卡点示范）——flex-queue（agent-prod）起 workflow
	// + 写 S1 审批单（kind=workflow_signal）；board approve/reject → Temporal Signal。
	r.POST("/weekly-content", requireAuth(), func(ctx *gin.Context) {
		var input workflows.WeeklyContentInput
		if err := ctx.ShouldBindJSON(&input); err != nil {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if input.Week == "" || input.Source == "" || input.Platform == "" {
			ctx.JSON(http.StatusBadRequest, gin.H{"error": "week/source/platform required"})
			return
		}
		if input.ApprovalWaitHours <= 0 {
			input.ApprovalWaitHours = float64(cfg.WeeklyApprovalWaitHours)
		}
		wid := fmt.Sprintf("weekly-content-%s", input.Week)
		wr, err := agentC.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			TaskQueue:                cfg.FlexTaskQueue,
			ID:                       wid,
			WorkflowIDReusePolicy:    enums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
			WorkflowExecutionTimeout: 24 * time.Hour,
		}, workflows.WeeklyContentWorkflow, input)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		// S1 审批单（单号 APR-WC-<week>[-n]，与 evolve-driver 同约定避免撞唯一索引）
		approvalNo := fmt.Sprintf("APR-WC-%s", input.Week)
		var existing int64
		if err := database.Model(&db.Approval{}).
			Where("approval_no = ? OR approval_no LIKE ?", approvalNo, approvalNo+"-%").
			Count(&existing).Error; err == nil && existing > 0 {
			approvalNo = fmt.Sprintf("%s-%d", approvalNo, existing+1)
		}
		now := time.Now()
		expiresAt := now.Add(time.Duration(cfg.WeeklyApprovalWaitHours) * time.Hour) // 审批单过期与 workflow 审批等待同步（默认 24h）
		payload, _ := json.Marshal(map[string]any{
			"workflow_id": wr.GetID(), "run_id": wr.GetRunID(),
			"week": input.Week, "platform": input.Platform,
		})
		apr := db.Approval{
			ApprovalNo: approvalNo, Kind: "workflow_signal",
			Subject: fmt.Sprintf("周内容 %s（平台 %s）", input.Week, input.Platform),
			Payload: string(payload), Status: "pending", Proposer: "weekly-content",
			ExpiresAt: &expiresAt, CreatedAt: now,
		}
		if err := database.Create(&apr).Error; err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{
				"error":       "workflow started but approval create failed: " + err.Error(),
				"workflow_id": wr.GetID(),
			})
			return
		}
		deps.apvStore.Log(ctx, "weekly-content", "approval_created", apr.ApprovalNo, map[string]any{
			"workflow_id": wr.GetID(), "week": input.Week, "platform": input.Platform,
		})
		ctx.JSON(http.StatusCreated, gin.H{
			"workflow_id": wr.GetID(), "run_id": wr.GetRunID(),
			"approval_id": apr.ID, "approval_no": apr.ApprovalNo,
		})
	})
}
