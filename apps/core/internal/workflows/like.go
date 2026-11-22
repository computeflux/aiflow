package workflows

import (
	"os"
	"time"

	"github.com/wetee/ai-workflow/internal/activities"
	"github.com/wetee/ai-workflow/internal/db"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// LikeWorkflowInput selects which pending raw items to like.
type LikeWorkflowInput struct {
	Source            string  `json:"source"`
	Keyword           string  `json:"keyword"`
	Limit             int     `json:"limit"`
	DelayBetween      float64 `json:"delay_between"`
	PyreplayTaskQueue string  `json:"pyreplay_task_queue"`
}

// LikeWorkflowResult reports how many tweets were liked.
type LikeWorkflowResult struct {
	Total   int `json:"total"`
	Success int `json:"success"`
	Failed  int `json:"failed"`
}

// LikeWorkflow selects unliked raw items from the DB, likes them via the
// pyreplay Python worker, then marks the successfully-liked ones in the DB.
func LikeWorkflow(ctx workflow.Context, input LikeWorkflowInput) (LikeWorkflowResult, error) {
	logger := workflow.GetLogger(ctx)

	source := input.Source
	if source == "" {
		source = db.PlatformX
	}
	delayBetween := input.DelayBetween
	if delayBetween <= 0 {
		delayBetween = 3.0
	}
	taskQueue := input.PyreplayTaskQueue
	if taskQueue == "" {
		taskQueue = "pyreplay-task-queue"
	}

	goAO := workflow.ActivityOptions{
		StartToCloseTimeout: 1 * time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}
	ctx = workflow.WithActivityOptions(ctx, goAO)

	var pending activities.GetPendingLikesResult
	err := workflow.ExecuteActivity(ctx, "GetPendingLikes", activities.GetPendingLikesInput{
		Source:  source,
		Keyword: input.Keyword,
		Limit:   input.Limit,
	}).Get(ctx, &pending)
	if err != nil {
		return LikeWorkflowResult{}, err
	}
	if len(pending.Items) == 0 {
		return LikeWorkflowResult{}, nil
	}

	// P1：LIKE_VIA_SKILL=true 时走 Skill 网关（skill "like"）而非 Python worker。
	// 默认未设置，保持旧链路无回归（docs/plan/transform-plan.md §6.7）。
	if os.Getenv("LIKE_VIA_SKILL") == "true" {
		return likeViaSkill(ctx, pending, source)
	}

	targets := make([]LikeTarget, 0, len(pending.Items))
	for _, item := range pending.Items {
		targets = append(targets, LikeTarget{
			TweetID:  item.ExternalID,
			TweetURL: item.URL,
			Content:  item.Content,
		})
	}

	pythonAO := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		TaskQueue:           taskQueue,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}

	var batchResult LikeBatchResult
	err = workflow.ExecuteActivity(workflow.WithActivityOptions(ctx, pythonAO), "LikeActivities.like_batch", LikeBatchInput{
		Targets:      targets,
		DelayBetween: delayBetween,
	}).Get(ctx, &batchResult)
	if err != nil {
		return LikeWorkflowResult{}, err
	}

	likedIDs := make([]string, 0, len(batchResult.Results))
	for _, r := range batchResult.Results {
		if r.Success {
			likedIDs = append(likedIDs, r.TweetID)
		}
	}

	if len(likedIDs) > 0 {
		err = workflow.ExecuteActivity(ctx, "MarkRawItemsLiked", activities.MarkRawItemsLikedInput{
			Source:      source,
			ExternalIDs: likedIDs,
		}).Get(ctx, nil)
		if err != nil {
			logger.Error("MarkRawItemsLiked failed", "error", err)
			return LikeWorkflowResult{}, err
		}
	}

	return LikeWorkflowResult{
		Total:   batchResult.Total,
		Success: batchResult.Success,
		Failed:  batchResult.Failed,
	}, nil
}

// likeViaSkill 走 P1 Skill 网关点赞（LIKE_VIA_SKILL=true 时启用）：
// 逐条推文调用 skill "like"，成功收集 external_id，最后统一标记已赞。
// 每条之间 Sleep 3s，失败后 Sleep 5s，避免对同一 skill 连环轰炸。
func likeViaSkill(ctx workflow.Context, pending activities.GetPendingLikesResult, source string) (LikeWorkflowResult, error) {
	logger := workflow.GetLogger(ctx)

	// like skill 最长 180s，activity 超时留 5 分钟；失败重试最多 3 次
	skillAO := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    3,
			InitialInterval:    5 * time.Second,
			BackoffCoefficient: 2.0,
		},
	}
	skillCtx := workflow.WithActivityOptions(ctx, skillAO)

	result := LikeWorkflowResult{Total: len(pending.Items)}
	likedIDs := make([]string, 0, len(pending.Items))

	for i, item := range pending.Items {
		if item.URL == "" {
			result.Failed++
			continue
		}
		var out map[string]any
		err := workflow.ExecuteActivity(skillCtx, "InvokeSkill", "like", map[string]any{
			"post_url":   item.URL,
			"like_count": 1,
		}).Get(ctx, &out)
		if err != nil {
			logger.Error("skill like failed", "url", item.URL, "error", err)
			result.Failed++
			_ = workflow.Sleep(ctx, 5*time.Second)
			continue
		}
		if ok, _ := out["ok"].(bool); ok {
			likedIDs = append(likedIDs, item.ExternalID)
			result.Success++
		} else {
			result.Failed++
		}
		if i < len(pending.Items)-1 {
			_ = workflow.Sleep(ctx, 3*time.Second)
		}
	}

	if len(likedIDs) > 0 {
		err := workflow.ExecuteActivity(ctx, "MarkRawItemsLiked", activities.MarkRawItemsLikedInput{
			Source:      source,
			ExternalIDs: likedIDs,
		}).Get(ctx, nil)
		if err != nil {
			logger.Error("MarkRawItemsLiked failed", "error", err)
			return result, err
		}
	}
	return result, nil
}
