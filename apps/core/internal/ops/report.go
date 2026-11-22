package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"go.temporal.io/sdk/client"
)

// GenerateDaily 生成当日日报（type=daily）：聚合近 24h 数据 → 数据表 Markdown →
// LLM 中文归纳（不可用时降级为纯数据表，不阻塞）→ 幂等 upsert。
func GenerateDaily(ctx context.Context, store *Store, database *db.DB, llmClient *llm.Client, temporal client.Client, namespace string) (*OpsReport, error) {
	m, err := Aggregate(ctx, database, temporal, namespace, 1)
	if err != nil {
		return nil, err
	}
	content := buildDailyMarkdown(ctx, m, llmClient)
	return upsert(ctx, store, time.Now(), "daily", content, m)
}

// GenerateWeekly 生成周报（type=weekly）：聚合近 7 天数据（结构与日报一致，窗口 7 天）。
// 升级建议（type=upgrade_suggestion）单独走 GenerateSuggestions。
func GenerateWeekly(ctx context.Context, store *Store, database *db.DB, llmClient *llm.Client, temporal client.Client, namespace string) (*OpsReport, error) {
	m, err := Aggregate(ctx, database, temporal, namespace, 7)
	if err != nil {
		return nil, err
	}
	content := buildWeeklyMarkdown(ctx, m, llmClient)
	return upsert(ctx, store, time.Now(), "weekly", content, m)
}

// upsert 把 metrics 序列化为 JSON 并幂等写入 ops_reports。
func upsert(ctx context.Context, store *Store, when time.Time, rtype, content string, m *Metrics) (*OpsReport, error) {
	metricsJSON, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal metrics: %w", err)
	}
	r := &OpsReport{
		ReportDate: when,
		Type:       rtype,
		Content:    content,
		Metrics:    string(metricsJSON),
	}
	if err := store.Upsert(ctx, r); err != nil {
		return nil, fmt.Errorf("upsert ops_report %s: %w", rtype, err)
	}
	return r, nil
}

// llmSummary 用 LLM 把聚合数据归纳成 1-3 句中文；不可用/失败时返回空串（降级）。
func llmSummary(ctx context.Context, llmClient *llm.Client, m *Metrics) string {
	if llmClient == nil {
		return ""
	}
	b, _ := json.Marshal(m)
	ctxL, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	summary, err := llmClient.ChatCompletion(ctxL,
		"你是运维周报/日报助手。根据下面的运维聚合数据（JSON），用 1-3 句中文总结：整体健康度、最值得关注的问题、需要跟进的事项。只输出总结文字，不要 markdown 标题。",
		string(b))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(summary)
}

// buildDailyMarkdown 生成日报 Markdown：LLM 归纳（可选）+ 各维度数据表。
func buildDailyMarkdown(ctx context.Context, m *Metrics, llmClient *llm.Client) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 运维日报 · %s（近 %d 天）\n\n", m.GeneratedAt[:10], m.WindowDays))
	if s := llmSummary(ctx, llmClient, m); s != "" {
		b.WriteString("## 摘要\n\n" + s + "\n\n")
	}
	b.WriteString(buildMetricsTables(m))
	return b.String()
}

// buildWeeklyMarkdown 生成周报 Markdown（窗口 7 天，结构同日报）。
func buildWeeklyMarkdown(ctx context.Context, m *Metrics, llmClient *llm.Client) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 运维周报 · 截至 %s（近 7 天）\n\n", m.GeneratedAt[:10]))
	if s := llmSummary(ctx, llmClient, m); s != "" {
		b.WriteString("## 摘要\n\n" + s + "\n\n")
	}
	b.WriteString(buildMetricsTables(m))
	return b.String()
}

// buildMetricsTables 把 Metrics 渲染成 Markdown 数据表（日报/周报/建议共用）。
func buildMetricsTables(m *Metrics) string {
	var b strings.Builder

	// 错误日志
	b.WriteString(fmt.Sprintf("## 错误日志（新增 %d / 未处理 %d）\n\n", m.Errors.TotalInWindow, m.Errors.Unresolved))
	b.WriteString("| 维度 | 值 |\n|---|---|\n")
	for _, s := range m.Errors.BySystem {
		b.WriteString(fmt.Sprintf("| 系统 %s | %d |\n", s.Key, s.Count))
	}
	for _, l := range m.Errors.ByLevel {
		b.WriteString(fmt.Sprintf("| 级别 %s | %d |\n", l.Key, l.Count))
	}
	if len(m.Errors.Top10) > 0 {
		b.WriteString("\n**Top 10 错误聚类**\n\n| 系统 | 来源 | 次数 | 示例 |\n|---|---|---|---|\n")
		for _, t := range m.Errors.Top10 {
			b.WriteString(fmt.Sprintf("| %s | %s | %d | %s |\n", t.System, t.Source, t.Count, truncate(t.Sample, 80)))
		}
	}
	b.WriteString("\n")

	// 技能
	if len(m.Skills) > 0 {
		b.WriteString("## 技能（成功率 / p99 / timeout / 熔断）\n\n| 技能 | 次数 | 成功率 | p99(ms) | timeout | 熔断态 |\n|---|---|---|---|---|---|\n")
		for _, s := range m.Skills {
			circuit := s.CircuitState
			if circuit == "" {
				circuit = "-"
			}
			b.WriteString(fmt.Sprintf("| %s | %d | %.1f%% | %d | %d | %s |\n",
				s.Name, s.Total, s.SuccessRate*100, s.P99Ms, s.Timeouts, circuit))
		}
		b.WriteString("\n")
	}

	// Temporal
	b.WriteString("## Temporal\n\n| 活跃 workflow | 窗口内失败 |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| %d | %d |\n\n", m.Temporal.Active, m.Temporal.Failed))
	if m.Temporal.Note != "" {
		b.WriteString("> " + m.Temporal.Note + "\n\n")
	}

	// 干预
	b.WriteString(fmt.Sprintf("## 干预（新增 %d / 未解决 %d）\n\n", m.Interventions.NewInWindow, m.Interventions.Unresolved))
	b.WriteString("（详见「干预舱」页）\n")

	return b.String()
}

// truncate 截断长文本（用于表格示例列，避免 Markdown 表格过宽）。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
