package workflows

import (
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"github.com/wetee/ai-workflow/internal/db"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// PublishWorkflowInput targets a pending/approved job.
type PublishWorkflowInput struct {
	JobID uint `json:"job_id"`
}

// PublishWorkflowInput targets a pending/approved job.
// PublishWorkflow verifies approval and publishes the content to the target platform.
func PublishWorkflow(ctx workflow.Context, input PublishWorkflowInput) error {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	pythonAO := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		TaskQueue:           pythonMCPTaskQueue,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
		},
	}

	var job activities.GetJobResult
	err := workflow.ExecuteActivity(ctx, "GetJob", activities.GetJobInput{ID: input.JobID}).Get(ctx, &job)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job.Job.Status != db.StatusApproved {
		return fmt.Errorf("job %d not approved (status=%s)", input.JobID, job.Job.Status)
	}

	pi := PublishInput{
		Source:    job.Job.Platform,
		AccountID: job.Job.AccountID,
		Content:   job.Job.Content,
	}
	var publishResult map[string]any
	err = workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, pythonAO), "PublishActivities.publish", pi).Get(ctx, &publishResult)
	if err != nil {
		_ = workflow.ExecuteActivity(ctx, "UpdateJobStatus", activities.UpdateJobStatusInput{
			ID:     input.JobID,
			Status: db.StatusFailed,
			Error:  err.Error(),
		}).Get(ctx, nil)
		return fmt.Errorf("publish: %w", err)
	}

	return workflow.ExecuteActivity(ctx, "UpdateJobStatus", activities.UpdateJobStatusInput{
		ID:     input.JobID,
		Status: db.StatusPublished,
	}).Get(ctx, nil)
}
