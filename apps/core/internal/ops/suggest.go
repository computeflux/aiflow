package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/llm"
	"go.temporal.io/sdk/client"
)

// SuggestedCard 是「一键转任务卡」要建的看板卡（S6 §3.2）。
// priority 对齐 BoardCard：low|medium|high|urgent；checklist 为待办文本列表。
type SuggestedCard struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Priority    string   `json:"priority"`
	Checklist   []string `json:"checklist"`
}

// Suggestion 是一条升级建议（S6 §3.2 输出）。
// ManualPROnly=true 时只出「人工 PR 建议单」，不生成自动进化任务卡（红线，§3.2）。
type Suggestion struct {
	Title           string         `json:"title"`
	Reason          string         `json:"reason"`
	SuggestedChange string         `json:"suggested_change"`
	RiskLevel       string         `json:"risk_level"` // low | medium | high
	ManualPROnly    bool           `json:"manual_pr_only"`
	SuggestedCard   *SuggestedCard `json:"suggested_card,omitempty"`
}

// SuggestionInput 是建议生成的输入快照（写入 metrics.inputs，供对账/回溯）。
type SuggestionInput struct {
	WindowDays     int      `json:"window_days"`
	HighFreqErrors []string `json:"high_freq_errors"`
	GitHotspots    []string `json:"git_hotspots"`
	TODOCount      int      `json:"todo_count"`
	TODOSamples    []string `json:"todo_samples"`
	SlowCalls      []string `json:"slow_calls"`
}

// SuggestionMetrics 是 upgrade_suggestion 报告的 metrics 结构
// （card 端点从 metrics.suggestions[idx].suggested_card 读卡模板）。
type SuggestionMetrics struct {
	Inputs      SuggestionInput `json:"inputs"`
	Suggestions []Suggestion    `json:"suggestions"`
}

// GenerateSuggestions 生成升级建议（type=upgrade_suggestion）：
// 近 7 天高频错误 + git 变更热点 + TODO/FIXME + 技能慢调用 Top → LLM 归纳 →
// 建议数组（high 风险 → manual_pr_only，不生成自动任务卡）→ 幂等 upsert。
// 触发：每周一 08:00（ticker）+ 按需 POST /ops-reports/suggest（api 侧）。
func GenerateSuggestions(ctx context.Context, store *Store, database *db.DB, llmClient *llm.Client, temporal client.Client, namespace, repoRoot string) (*OpsReport, error) {
	m, err := Aggregate(ctx, database, temporal, namespace, 7)
	if err != nil {
		return nil, err
	}

	gitCounts, _ := GitHotspots(ctx, repoRoot) // 降级：空热点
	todo := ScanTODO(ctx, repoRoot, []string{"apps", "internal"})

	input := SuggestionInput{
		WindowDays:     7,
		HighFreqErrors: topErrorStrings(m.Errors.Top10),
		GitHotspots:    TopGitHotspots(gitCounts, 10),
		TODOCount:      todo.Total,
		TODOSamples:    todo.Samples,
		SlowCalls:      topSlowSkills(m.Skills, 5),
	}

	var suggestions []Suggestion
	if llmClient != nil {
		suggestions = llmSuggestions(ctx, llmClient, input)
	}
	applyRiskRedLine(&suggestions)

	content := buildSuggestionMarkdown(m, input, suggestions)
	sm := SuggestionMetrics{Inputs: input, Suggestions: suggestions}
	metricsJSON, err := json.Marshal(sm)
	if err != nil {
		return nil, fmt.Errorf("marshal suggestion metrics: %w", err)
	}
	r := &OpsReport{
		ReportDate: time.Now(),
		Type:       "upgrade_suggestion",
		Content:    content,
		Metrics:    string(metricsJSON),
	}
	if err := store.Upsert(ctx, r); err != nil {
		return nil, fmt.Errorf("upsert upgrade_suggestion: %w", err)
	}
	return r, nil
}

// applyRiskRedLine 红线（S6 §3.2）：high 风险（涉及 internal/cmd 核心代码）
// 只生成「人工 PR 建议单」，不生成自动进化任务卡 → 清空 suggested_card 并标记 manual_pr_only。
func applyRiskRedLine(suggestions *[]Suggestion) {
	for i := range *suggestions {
		s := &(*suggestions)[i]
		switch strings.ToLower(s.RiskLevel) {
		case "high":
			s.RiskLevel = "high"
			s.ManualPROnly = true
			s.SuggestedCard = nil
		default:
			s.ManualPROnly = false
		}
	}
}

// llmSuggestions 用 LLM 把输入归纳成建议数组；不可用/失败/解析失败时返回空（降级，报告仍生成）。
func llmSuggestions(ctx context.Context, llmClient *llm.Client, input SuggestionInput) []Suggestion {
	b, _ := json.Marshal(input)
	ctxL, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	out, err := llmClient.ChatCompletion(ctxL,
		"你是资深运维 / 架构评审。基于下面的近 7 天运维数据，提出 3-6 条可落地的升级/优化建议。"+
			"对每条建议判断风险等级：low=只影响 apps/* 某个独立子项目且改动小；medium=跨模块或影响技能行为；"+
			"high=涉及 internal/ 或 cmd/ 核心代码、数据库 schema、部署编排、鉴权（这类只出人工 PR 建议单，不生成自动任务卡）。"+
			"严格只输出一个 JSON 数组，不要 markdown 代码块、不要任何解释文字。每个元素字段："+
			`{"title":"...","reason":"...","suggested_change":"...","risk_level":"low|medium|high","suggested_card":{"title":"...","description":"...","priority":"low|medium|high|urgent","checklist":["...","..."]}}。`+
			"high 风险的元素把 suggested_card 置为 null。",
		string(b))
	if err != nil {
		return nil
	}
	return parseSuggestions(out)
}

// parseSuggestions 从 LLM 输出中抽取 JSON 数组并解析；失败返回空。
func parseSuggestions(raw string) []Suggestion {
	raw = strings.TrimSpace(raw)
	// 去掉可能的 markdown 代码块围栏
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start < 0 || end < 0 || end < start {
		return nil
	}
	var out []Suggestion
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return nil
	}
	return out
}

// buildSuggestionMarkdown 生成建议报告 Markdown：数据概览 + 建议列表（含红线标注）。
func buildSuggestionMarkdown(m *Metrics, input SuggestionInput, suggestions []Suggestion) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# 升级建议 · 截至 %s（近 7 天）\n\n", m.GeneratedAt[:10]))

	b.WriteString("## 输入概览\n\n")
	b.WriteString(fmt.Sprintf("- 高频错误：%d 类\n", len(input.HighFreqErrors)))
	b.WriteString(fmt.Sprintf("- git 变更热点：%d 个文件\n", len(input.GitHotspots)))
	b.WriteString(fmt.Sprintf("- TODO/FIXME：%d 处\n", input.TODOCount))
	b.WriteString(fmt.Sprintf("- 慢调用 Top：%d 个技能\n", len(input.SlowCalls)))

	if len(suggestions) == 0 {
		b.WriteString("\n> 本次未生成建议（LLM 不可用或数据不足）。可稍后重试 `POST /ops-reports/suggest`。\n")
		return b.String()
	}

	b.WriteString("\n## 建议\n\n")
	for i, s := range suggestions {
		b.WriteString(fmt.Sprintf("### %d. %s\n\n", i+1, s.Title))
		b.WriteString(fmt.Sprintf("- **风险**：%s", s.RiskLevel))
		if s.ManualPROnly {
			b.WriteString("（**红线：仅人工 PR 建议单，不生成自动任务卡**）")
		}
		b.WriteString("\n")
		b.WriteString(fmt.Sprintf("- **理由**：%s\n", s.Reason))
		b.WriteString(fmt.Sprintf("- **建议变更**：%s\n", s.SuggestedChange))
		if s.SuggestedCard != nil {
			b.WriteString(fmt.Sprintf("- **可转任务卡**：%s（优先级 %s，%d 项清单）\n",
				s.SuggestedCard.Title, s.SuggestedCard.Priority, len(s.SuggestedCard.Checklist)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// topErrorStrings 把 Top 错误聚类格式化成 "system | source：N 次" 列表。
func topErrorStrings(top []TopError) []string {
	out := make([]string, 0, len(top))
	for _, t := range top {
		out = append(out, fmt.Sprintf("%s | %s：%d 次", t.System, t.Source, t.Count))
	}
	return out
}

// topSlowSkills 取 p99 最高的前 n 个技能，格式化成 "name：p99=Nms, 成功率=..%, timeout=N"。
func topSlowSkills(skills []SkillMetrics, n int) []string {
	sorted := append([]SkillMetrics(nil), skills...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].P99Ms > sorted[j].P99Ms })
	out := make([]string, 0, n)
	for i := 0; i < len(sorted) && i < n; i++ {
		s := sorted[i]
		if s.P99Ms == 0 && s.Total == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("%s：p99=%dms, 成功率=%.1f%%, timeout=%d, 熔断=%s",
			s.Name, s.P99Ms, s.SuccessRate*100, s.Timeouts, s.CircuitState))
	}
	return out
}
