// Package workflows 定义 Temporal 工作流与输入输出契约（跨语言调度的状态机定义层）。
package workflows

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// AutoTweetWorkflow 是 X 自动推文 workflow：抓取素材 → LLM 生成 → 存待审推文。
func AutoTweetWorkflow(ctx workflow.Context, input AutoTweetInput) (*AutoTweetResult, error) {
	if input.Hours <= 0 {
		input.Hours = 24
	}
	if input.Count <= 0 {
		input.Count = 5
	}
	if input.Source == "" {
		input.Source = "x"
	}

	logger := workflow.GetLogger(ctx)
	goTaskQueue := workflow.GetInfo(ctx).TaskQueueName

	// Step 1: generate tweets (Python activity)
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
		return nil, err
	}

	logger.Info("SocialGenerateActivity.generate completed", "tweets", len(result.Tweets))

	// Step 2: save to DB (Go activity, schedule_id=0 for ad-hoc generation)
	saveAO := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		TaskQueue:           goTaskQueue,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 2},
	}
	ctx = workflow.WithActivityOptions(ctx, saveAO)

	for i, t := range result.Tweets {
		err := workflow.ExecuteActivity(ctx, "SaveSocialTweet", map[string]interface{}{
			"schedule_id": uint(0),
			"content":     t.Tweet,
			"source_url":  t.SourceURL,
			"keyword":     t.Keyword,
			"score":       t.Score,
			"reasoning":   t.Reasoning,
		}).Get(ctx, nil)
		if err != nil {
			// 保存失败时让 workflow 失败（Temporal 会按 RetryPolicy 自动重试），
			// 而不是只打日志后假装成功，避免推文静默丢失。
			logger.Error("SaveSocialTweet failed", "index", i, "error", err)
			return nil, fmt.Errorf("save tweet %d: %w", i, err)
		}
		logger.Info("SaveSocialTweet saved", "index", i, "source_url", t.SourceURL)
	}

	return &result, nil
}
