package workflows

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ScheduledFetchWorkflowInput holds the config for a recurring ingest.
type ScheduledFetchWorkflowInput struct {
	Source        string `json:"source"`
	Keyword       string `json:"keyword"`
	Limit         int    `json:"limit"`
	Mode          string `json:"mode"`
	ParallelCount int    `json:"parallel_count"`
}

// ScheduledFetchWorkflow is a cron workflow that periodically fetches and indexes content.
func ScheduledFetchWorkflow(ctx workflow.Context, input ScheduledFetchWorkflowInput) (activities.ProcessCacheResult, error) {
	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 3,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	pythonAO := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		TaskQueue:           pythonMCPTaskQueue,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
		},
	}

	workflowID := workflow.GetInfo(ctx).WorkflowExecution.ID

	jitter := time.Duration(rand.Intn(1800)) * time.Second
	_ = workflow.Sleep(ctx, jitter)

	parallelCount := input.ParallelCount
	if parallelCount <= 0 {
		parallelCount = 1
	}

	var futures []workflow.Future
	for i := range parallelCount {
		fi := FetchInput{
			Source:     input.Source,
			Keyword:    input.Keyword,
			Limit:      input.Limit,
			Mode:       input.Mode,
			WorkflowID: fmt.Sprintf("%s-p%d", workflowID, i),
		}
		future := workflow.ExecuteActivity(
			workflow.WithActivityOptions(ctx, pythonAO),
			"FetchActivities.fetch", fi,
		)
		futures = append(futures, future)
	}

	anyData := false
	for _, f := range futures {
		var fr FetchResult
		if err := f.Get(ctx, &fr); err != nil {
			return activities.ProcessCacheResult{}, err
		}
		if fr.Total > 0 {
			anyData = true
		}
	}
	if !anyData {
		return activities.ProcessCacheResult{}, nil
	}

	var merged activities.ProcessCacheResult
	for i := range parallelCount {
		pid := fmt.Sprintf("%s-p%d", workflowID, i)
		var pr activities.ProcessCacheResult
		err := workflow.ExecuteActivity(ctx, "ProcessFetchCache",
			activities.ProcessCacheInput{WorkflowID: pid},
		).Get(ctx, &pr)
		if err != nil {
			return merged, fmt.Errorf("process cache %s: %w", pid, err)
		}
		merged.Saved += pr.Saved
		merged.Skipped += pr.Skipped
		merged.Chunks += pr.Chunks
	}
	return merged, nil
}
