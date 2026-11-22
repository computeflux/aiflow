package workflows

import (
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// EmailWorkflowInput 是邮件批量生成 workflow 的输入。
type EmailWorkflowInput struct {
	OrgName string `json:"org_name"`
}

// EmailWorkflowResult 是邮件批量生成 workflow 的结果：批次 id、总数与发送统计。
type EmailWorkflowResult struct {
	BatchID uint `json:"batch_id"`
	Total   int  `json:"total"`
}

// EmailWorkflow 邮件批量生成 workflow：AI 分析 → 逐收件人生成 → 批次收尾（Temporal activity 链）。
func EmailWorkflow(ctx workflow.Context, input EmailWorkflowInput) (EmailWorkflowResult, error) {
	if input.OrgName == "" {
		return EmailWorkflowResult{}, fmt.Errorf("org_name is required")
	}

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var batchID uint
	var total int

	defer func() {
		dCtx, _ := workflow.NewDisconnectedContext(ctx)
		finalStatus := "failed"
		errMsg := "workflow terminated or failed"
		if total > 0 {
			finalStatus = "pending"
			errMsg = ""
		}
		_ = workflow.ExecuteActivity(
			workflow.WithActivityOptions(dCtx, workflow.ActivityOptions{
				StartToCloseTimeout: 30 * time.Second,
				RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
			}),
			"FinalizeEmailBatch",
			activities.FinalizeEmailBatchInput{BatchID: batchID, Status: finalStatus, Total: total, ErrorMsg: errMsg},
		).Get(dCtx, nil)
	}()

	var result activities.EmailAnalysisOutput
	err := workflow.ExecuteActivity(ctx, "RunEmailAnalysis",
		activities.EmailAnalysisInput{OrgName: input.OrgName},
	).Get(ctx, &result)
	if err != nil {
		return EmailWorkflowResult{}, fmt.Errorf("email analysis: %w", err)
	}

	batchID = result.BatchID
	total = result.Total
	return EmailWorkflowResult{BatchID: batchID, Total: total}, nil
}
