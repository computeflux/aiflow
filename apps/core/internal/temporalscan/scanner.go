// Package temporalscan 扫描运行中 workflow 的 history（S3 干预舱，docs/plan/S3-干预舱.md §3.2）：
// 事件数、最近错误、同一 activity 连续失败重试次数。
// watchdog 死循环检测与 board 活跃 workflow 表共用。
//
// 注意：Temporal SDK v1.45.0 的 ListWorkflow 返回 WorkflowExecutionInfo（含 status / start_time /
// execution.id+run_id）；activity 名需经 ScheduledEventId 关联 scheduled 事件
// （completed/failed 事件本身不带 ActivityType）。
package temporalscan

import (
	"context"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
)

// WorkflowScan 是单个运行中 workflow 的扫描结果。
// JSON 标签为 snake_case（board 干预舱页 ActiveWorkflow 接口约定；
// 缺标签时 Go 默认导出 PascalCase 字段名，前端全列渲染为空）。
type WorkflowScan struct {
	WorkflowID                     string `json:"workflow_id"`
	Namespace                      string `json:"namespace"`
	Status                         string `json:"status"`
	StartTime                      string `json:"start_time"` // RFC3339
	EventCount                     int    `json:"event_count"`
	LastError                      string `json:"last_error"`                        // 最近一次 ActivityTaskFailed 的 message
	MaxConsecutiveActivityFailures int    `json:"max_consecutive_activity_failures"` // 尾部连续段中，同一 activity 的最大连续失败次数
}

const maxEventsPerWorkflow = 2000 // 单 workflow 事件扫描上限（控制成本）

const recentStartWindow = time.Hour // 只扫最近 1h 内启动的 workflow（见 ScanRunning 注释）

// ScanRunning 扫描 namespace 中运行中的 workflow（最多 limit 个，默认 30，上限 50）。
// namespace 显式传入（client.Client v1.45.0 不暴露 Namespace()）。
func ScanRunning(ctx context.Context, c client.Client, namespace string, limit int) ([]WorkflowScan, error) {
	if limit <= 0 || limit > 50 {
		limit = 30
	}
	// 本版本 API 的 ListWorkflow 无 Sort 字段，默认按开始时间升序：存量老 workflow 会占满
	// 第一页，新启动的死循环 workflow 不可见。用 CQL StartTime 过滤只看最近 1h 内
	// 启动的 workflow（死循环检测与 board 活跃表都只关心新近 workflow）。
	since := time.Now().Add(-recentStartWindow).UTC().Format(time.RFC3339)
	resp, err := c.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		Query:    fmt.Sprintf("ExecutionStatus = 'Running' AND StartTime > '%s'", since),
		PageSize: int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("list running workflows: %w", err)
	}
	scans := make([]WorkflowScan, 0, len(resp.Executions))
	for _, info := range resp.Executions {
		scan, err := scanOne(ctx, c, namespace, info)
		if err != nil {
			// 单个 workflow 扫描失败不阻断整体
			continue
		}
		scans = append(scans, scan)
	}
	return scans, nil
}

// scanOne 拉取单个 workflow 的 history 并统计。
//
// activity 名关联：ACTIVITY_TASK_SCHEDULED 事件带 ActivityType，其 EventId 即 completed/
// failed 事件里引用的 ScheduledEventId；据此把失败/完成归到对应 activity 名。
//
// 事件按时间正序：failStreak[activity] = 该 activity 自上次完成以来的连续失败数，
// 扫描结束时各值即"当前尾部连续段"的失败次数。
func scanOne(ctx context.Context, c client.Client, namespace string, info *workflowpb.WorkflowExecutionInfo) (WorkflowScan, error) {
	scan := WorkflowScan{
		WorkflowID: info.Execution.WorkflowId,
		Namespace:  namespace,
		Status:     info.Status.String(),
	}
	if info.StartTime != nil {
		scan.StartTime = info.StartTime.AsTime().Format("2006-01-02T15:04:05Z07:00")
	}

	iter := c.GetWorkflowHistory(ctx, info.Execution.WorkflowId, info.Execution.RunId, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	activityName := map[int64]string{} // scheduled 事件 id → activity 名
	failStreak := map[string]int{}
	var lastError string
	count := 0
	for iter.HasNext() {
		ev, err := iter.Next()
		if err != nil {
			return scan, err
		}
		count++
		if count > maxEventsPerWorkflow {
			break
		}
		switch ev.EventType {
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED:
			attr := ev.GetActivityTaskScheduledEventAttributes()
			if attr.ActivityType != nil {
				activityName[ev.EventId] = attr.ActivityType.Name
			}
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_FAILED:
			attr := ev.GetActivityTaskFailedEventAttributes()
			if name := activityName[attr.ScheduledEventId]; name != "" {
				failStreak[name]++
			}
			if attr.Failure != nil && attr.Failure.Message != "" {
				lastError = attr.Failure.Message
			}
		case enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED:
			attr := ev.GetActivityTaskCompletedEventAttributes()
			if name := activityName[attr.ScheduledEventId]; name != "" {
				failStreak[name] = 0
			}
		}
	}
	for _, n := range failStreak {
		if n > scan.MaxConsecutiveActivityFailures {
			scan.MaxConsecutiveActivityFailures = n
		}
	}
	scan.EventCount = count
	scan.LastError = lastError
	return scan, nil
}
