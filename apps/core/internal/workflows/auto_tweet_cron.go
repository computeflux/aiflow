package workflows

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// ScheduledAutoTweetWorkflow is a cron workflow that generates and saves tweets.
func ScheduledAutoTweetWorkflow(ctx workflow.Context, input AutoTweetInput) error {
	logger := workflow.GetLogger(ctx)
	goTaskQueue := workflow.GetInfo(ctx).TaskQueueName

	genAO := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		TaskQueue:           pythonMCPTaskQueue,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, genAO)

	var result AutoTweetResult
	err := workflow.ExecuteActivity(ctx, "SocialGenerateActivity.generate", map[string]interface{}{
		"hours":  input.Hours,
		"count":  input.Count,
		"source": input.Source,
	}).Get(ctx, &result)
	if err != nil {
		return err
	}

	logger.Info("SocialGenerateActivity.generate completed", "tweets", len(result.Tweets))

	saveAO := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		TaskQueue:           goTaskQueue,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	ctx = workflow.WithActivityOptions(ctx, saveAO)

	wid := workflow.GetInfo(ctx).WorkflowExecution.ID
	var schedID uint
	fmt.Sscanf(wid, "social-sched-%d", &schedID)

	for i, t := range result.Tweets {
		err := workflow.ExecuteActivity(ctx, "SaveSocialTweet", map[string]interface{}{
			"schedule_id": schedID,
			"content":     t.Tweet,
			"source_url":  t.SourceURL,
			"keyword":     t.Keyword,
			"score":       t.Score,
			"reasoning":   t.Reasoning,
		}).Get(ctx, nil)
		if err != nil {
			logger.Error("SaveSocialTweet failed", "index", i, "error", err)
		} else {
			logger.Info("SaveSocialTweet saved", "index", i, "source_url", t.SourceURL)
		}
	}

	return nil
}
