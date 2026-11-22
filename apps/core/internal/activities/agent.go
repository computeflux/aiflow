package activities

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"go.temporal.io/sdk/activity"

	"github.com/wetee/ai-workflow/internal/flex"
	"github.com/wetee/ai-workflow/internal/otel"
	internalotel "github.com/wetee/ai-workflow/internal/otel"
)

// ExtractParamsActivity / ReportStatusActivity 是 Agent 参与主干的唯一两个 Activity
// （S2 混合编排，docs/plan/S2-混合编排.md §3.4）：
// Agent 提取参数后调用 ExtractParamsActivity（输出 schema 校验）；
// Agent 汇报状态调用 ReportStatusActivity。
// Agent 无权改分支、无权重试、无权跳过节点。

// ExtractParamsActivity 校验 Agent 提取的参数（JSON Schema），通过则原样返回。
// schema 为空（nil/空字节）时不做校验。
func (a *Activities) ExtractParamsActivity(ctx context.Context, params map[string]any, schema json.RawMessage) (map[string]any, error) {
	if params == nil {
		return nil, fmt.Errorf("extract params: nil params")
	}
	if err := flex.ValidateSchema(schema, params); err != nil {
		return nil, fmt.Errorf("extract params schema validation failed: %w", err)
	}
	return params, nil
}

// failingStatuses 是 ReportStatusActivity 落 error_logs 的状态值（其余仅结构化日志）。
var failingStatuses = map[string]bool{
	"error": true, "failed": true, "failure": true, "blocked": true, "timeout": true,
}

// ReportStatusActivity 接收 Agent 状态汇报：
// 异常状态（error/failed/blocked/timeout 等）落 error_logs 留痕，正常状态仅结构化日志。
func (a *Activities) ReportStatusActivity(ctx context.Context, status string, detail string) (map[string]any, error) {
	if status == "" {
		return nil, fmt.Errorf("report status: empty status")
	}
	wid, rid := safeActivityInfo(ctx)
	log.Printf("agent status report: workflow=%s status=%q detail=%s", wid, status, detail)
	if failingStatuses[status] && a.DB != nil {
		otel.Record(a.DB, otel.Entry{
			System:     "go-worker",
			Level:      "warning",
			Source:     "ReportStatusActivity",
			WorkflowID: wid,
			RunID:      rid,
			TraceID:    internalotel.TraceIDFrom(ctx),
			Message:    fmt.Sprintf("agent reported status %q: %s", status, detail),
		})
	}
	return map[string]any{"status": status, "recorded": true}, nil
}

// safeActivityInfo 取 Temporal workflow 元信息；
// activity.GetInfo 在非 activity 上下文（如直调单测/冒烟）会 panic，recover 兜底。
func safeActivityInfo(ctx context.Context) (workflowID, runID string) {
	func() {
		defer func() { _ = recover() }()
		info := activity.GetInfo(ctx)
		workflowID, runID = info.WorkflowExecution.ID, info.WorkflowExecution.RunID
	}()
	return
}
