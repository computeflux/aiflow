package activities

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.temporal.io/sdk/temporal"

	"github.com/wetee/ai-workflow/internal/db"
	internalotel "github.com/wetee/ai-workflow/internal/otel"
)

// FlexActivity 调用 flex 柔性子图（S2 混合编排，docs/plan/S2-混合编排.md §3.3）。
//
// POST {FLEX_ENDPOINT}/runs（默认 http://flex-server:8124，仅内部网络）。
// 入参 JSON Schema 校验与硬约束（节点 ≤ 20 / token ≤ 50k / 超时 10 分钟，
// 超限直接拒绝）由 flex-server 侧负责；这里做输出结构轻校验
// （ok=true + output 对象），并把执行记录写入 skill_runs
// （skill_name=flex:<graph>），W3C traceparent 透传贯穿链路。
//
// 注意：agent-prod namespace 默认 Activity 超时为 2 分钟（硬配置 §3.1）；
// workflow 调用本 activity 时须显式设置 StartToCloseTimeout: 11 * time.Minute
// （server 最大 10 分钟 + 余量）。
//
// 失败时记录 skill_runs 后返回 error；workflow 侧按 §7 降级（跳过脑暴节点）。
func (a *Activities) FlexActivity(ctx context.Context, graph string, input map[string]any) (map[string]any, error) {
	base := strings.TrimSuffix(os.Getenv("FLEX_ENDPOINT"), "/")
	if base == "" {
		base = "http://flex-server:8124"
	}

	body, err := json.Marshal(map[string]any{"graph": graph, "input": input})
	if err != nil {
		return nil, fmt.Errorf("marshal flex input: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/runs", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build flex request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// 透传 W3C traceparent：子图执行与 workflow 串成一条链路
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))

	// server 侧硬超时 10 分钟，HTTP 超时留 1 分钟余量
	client := &http.Client{Timeout: 11 * time.Minute}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		a.recordFlexRun(ctx, graph, input, "fail", time.Since(start), err.Error())
		return nil, fmt.Errorf("call flex-server /runs: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		a.recordFlexRun(ctx, graph, input, "fail", time.Since(start), fmt.Sprintf("read response: %v", err))
		return nil, fmt.Errorf("read flex-server response: %w", err)
	}

	status, runErr := "ok", ""
	switch {
	case resp.StatusCode == http.StatusGatewayTimeout:
		status, runErr = "timeout", "flex run exceeded 10min hard timeout"
	case resp.StatusCode != http.StatusOK:
		var e struct {
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Detail == "" {
			e.Detail = string(data)
		}
		status, runErr = "fail", fmt.Sprintf("HTTP %d: %s", resp.StatusCode, e.Detail)
	}

	var out struct {
		OK     bool           `json:"ok"`
		Output map[string]any `json:"output"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &out); err != nil {
			status, runErr = "fail", fmt.Sprintf("decode response: %v", err)
		} else if !out.OK || out.Output == nil {
			status, runErr = "fail", "unexpected response shape (ok=true + output object expected)"
		}
	}

	latency := time.Since(start)
	a.recordFlexRun(ctx, graph, input, status, latency, runErr)
	if status != "ok" {
		err := fmt.Errorf("flex graph %s: %s", graph, runErr)
		// 4xx（schema 校验/超限/未知图）是确定性客户端错误，重试无意义；5xx/超时可重试
		// （本 SDK 版本无 NonRetryableError，用 NewNonRetryableApplicationError 等效）
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			err = temporal.NewNonRetryableApplicationError(err.Error(), "flexClientError", err)
		}
		return nil, err
	}
	return out.Output, nil
}

// recordFlexRun 把柔性子图执行记录写入 skill_runs（skill_name=flex:<graph>）。
// 写库失败仅记日志，不影响 activity 结果。
func (a *Activities) recordFlexRun(ctx context.Context, graph string, input map[string]any, status string, latency time.Duration, runErr string) {
	if a.DB == nil {
		return
	}
	inputJSON, _ := json.Marshal(input)
	sum := sha256.Sum256(inputJSON)
	latencyMs := int64(latency.Milliseconds())
	// 红线 6（S8 审计）：skill_runs 必带 trace——OTel 链路缺失时随机 32 位 hex 兜底（与 skillgateway 同语义）
	traceID := internalotel.TraceIDFrom(ctx)
	if traceID == "" {
		traceID = randomHex32()
	}
	run := &db.SkillRun{
		SkillName: "flex:" + graph,
		Version:   "0.1.0",
		TraceID:   traceID,
		InputHash: hex.EncodeToString(sum[:]),
		Status:    status,
		LatencyMs: &latencyMs,
		Error:     runErr,
	}
	// activity.GetInfo 在非 activity 上下文（如直调冒烟）会 panic，recover 兜底
	wid, rid := safeActivityInfo(ctx)
	if wid != "" {
		run.WorkflowID = wid
		run.RunID = rid
	}
	if err := a.DB.WithContext(ctx).Create(run).Error; err != nil {
		log.Printf("WARN: record flex run flex:%s: %v", graph, err)
	}
}

// randomHex32 随机 32 位 hex（trace 兜底，与 skillgateway.runner 的 randomHex32 同语义；
// 两包独立不互相 import，此处镜像实现）。
func randomHex32() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", 32)
	}
	return hex.EncodeToString(b)
}
