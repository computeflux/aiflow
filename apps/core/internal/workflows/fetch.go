package workflows

import (
	"fmt"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// FetchWorkflowInput triggers content fetching and indexing.
// Set ResumeWorkflowID to skip the fetch step and process leftover
// rows from a previously failed workflow.
//
// ParallelCount controls how many agents execute the same fetch concurrently
// (each produces its own results; all are stored in the shared DB).
// Use > 1 for sources like X keyword search where multiple agents
// return different results.
type FetchWorkflowInput struct {
	Source           string `json:"source"`
	Keyword          string `json:"keyword"`
	Limit            int    `json:"limit"`
	Mode             string `json:"mode"`
	ParallelCount    int    `json:"parallel_count"`
	ResumeWorkflowID string `json:"resume_workflow_id"`
}

// FetchWorkflow fetches raw data from MCP sources, persists it to the DB
// and indexes it into Qdrant.
func FetchWorkflow(ctx workflow.Context, input FetchWorkflowInput) (activities.ProcessCacheResult, error) {
	if input.Source == "" {
		return activities.ProcessCacheResult{}, fmt.Errorf("missing source: please specify one of: x, github, reddit, xiaohongshu")
	}

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 60 * time.Minute,
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

	workflowID := input.ResumeWorkflowID
	if workflowID == "" {
		workflowID = workflow.GetInfo(ctx).WorkflowExecution.ID

		parallelCount := input.ParallelCount
		if parallelCount <= 0 {
			parallelCount = 1
		}

		// Launch parallel fetch activities — each executed by an available agent.
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

		// Collect results — if all are empty, return early.
		anyData := false
		for _, f := range futures {
			var fr FetchResult
			if err := f.Get(ctx, &fr); err != nil {
				return activities.ProcessCacheResult{}, fmt.Errorf("fetch: %w", err)
			}
			if fr.Total > 0 {
				anyData = true
			}
		}
		if !anyData {
			return activities.ProcessCacheResult{}, nil
		}
	}

	// Drain cache for all parallel workflow IDs.
	procCtx := ctx
	if input.ParallelCount > 1 {
		// Use the base workflow ID when processing, so all parallel
		// caches get drained.  The Go drainer uses LIKE matching or
		// we just process each parallel ID separately.
		var err error
		var merged activities.ProcessCacheResult

		for i := range input.ParallelCount {
			pid := fmt.Sprintf("%s-p%d", workflowID, i)
			var pr activities.ProcessCacheResult
			err = workflow.ExecuteActivity(ctx, "ProcessFetchCache",
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

	var procResult activities.ProcessCacheResult
	err := workflow.ExecuteActivity(procCtx, "ProcessFetchCache",
		activities.ProcessCacheInput{WorkflowID: fmt.Sprintf("%s-p0", workflowID)},
	).Get(ctx, &procResult)
	if err != nil {
		return procResult, fmt.Errorf("process cache: %w", err)
	}

	return procResult, nil
}
