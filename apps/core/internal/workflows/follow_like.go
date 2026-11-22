package workflows

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// FollowLikeDispatchInput 触发订阅目标点赞分发。
// TargetID 为 0 时扫描所有启用的 follow_targets，否则只分发指定目标。
type FollowLikeDispatchInput struct {
	TargetID          uint   `json:"target_id"`
	PyreplayTaskQueue string `json:"pyreplay_task_queue"`
}

// FollowLikeDispatchResult 报告分发结果。
type FollowLikeDispatchResult struct {
	Targets     int `json:"targets"`
	Scanned     int `json:"scanned"`
	JobsCreated int `json:"jobs_created"`
}

// FollowLikeDispatchWorkflow 调用 pyreplay 的 FollowActivities.dispatch_likes：
// 扫描订阅目标的推文，为每台机器生成 scheduled_at 随机分布的 like_jobs 任务，
// 各机器后台轮询到期任务完成点赞。
func FollowLikeDispatchWorkflow(ctx workflow.Context, input FollowLikeDispatchInput) (FollowLikeDispatchResult, error) {
	taskQueue := input.PyreplayTaskQueue
	if taskQueue == "" {
		taskQueue = "pyreplay-task-queue"
	}

	ao := workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Minute,
		TaskQueue:           taskQueue,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
	ctx = workflow.WithActivityOptions(ctx, ao)

	var result FollowLikeDispatchResult
	err := workflow.ExecuteActivity(ctx, "FollowActivities.dispatch_likes", input.TargetID).Get(ctx, &result)
	return result, err
}
