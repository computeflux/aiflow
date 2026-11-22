package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.temporal.io/sdk/activity"
)

// InvokeSkill 通过 Skill Gateway 调用一个已注册的 skill
// （P1 桥接 activity，docs/plan/transform-plan.md §6.7）。
//
// 纯函数 activity：不依赖 Activities 结构体，在 worker main 中单独注册。
// 网关负责 schema 校验、限流、执行与 skill_runs 落库；这里只做 HTTP 转发，
// 并把 W3C traceparent 与 Temporal workflow 元信息带上，实现链路贯穿
// workflow → API → skill。
//
// 环境变量：
//   - SKILL_GATEWAY_URL：workflow API 地址（默认 http://127.0.0.1:8001）
func InvokeSkill(ctx context.Context, name string, input map[string]any) (map[string]any, error) {
	base := strings.TrimSuffix(os.Getenv("SKILL_GATEWAY_URL"), "/")
	if base == "" {
		base = "http://127.0.0.1:8001"
	}

	body, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal skill input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/skills/"+name+"/invoke", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build skill request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 传播 W3C traceparent：skill 执行与 workflow 串成一条链路
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	// Temporal workflow 元信息 → 网关写入 skill_runs
	if info := activity.GetInfo(ctx); info.WorkflowExecution.ID != "" {
		req.Header.Set("X-Workflow-ID", info.WorkflowExecution.ID)
		req.Header.Set("X-Run-ID", info.WorkflowExecution.RunID)
	}

	// skill 最长执行 limits.timeout_sec（300s），HTTP 超时留 5 分钟余量
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call skill gateway: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read skill response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = string(data)
		}
		return nil, fmt.Errorf("invoke skill %s: HTTP %d: %s", name, resp.StatusCode, e.Error)
	}

	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode skill response: %w", err)
	}
	return out, nil
}
