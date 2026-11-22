package workitem

import "time"

// ScoreWeights 是轻重缓急四维打分的权重（§6.2）。
// 默认：严重度 > 紧急度 = 影响面 > 复现频率。
type ScoreWeights struct {
	Severity  float64
	Urgency   float64
	Impact    float64
	Frequency float64
}

// DefaultWeights 返回默认权重（可经 .env 覆盖，见 cmd/work-dispatch）。
func DefaultWeights() ScoreWeights {
	return ScoreWeights{Severity: 3, Urgency: 2, Impact: 2, Frequency: 1}
}

// P0Threshold 是 P0 分档阈值（≥ 此分即 P0，插队）。
const P0Threshold = 15.0

// ComputeScore 计算综合分 = w·(severity, urgency, impact, frequency)（§6.2）。
func ComputeScore(w ScoreWeights, severity, urgency, impact, frequency int) float64 {
	return w.Severity*float64(severity) + w.Urgency*float64(urgency) +
		w.Impact*float64(impact) + w.Frequency*float64(frequency)
}

// BandFromScore 把综合分分档为 P0-P3（§6.2）：
// P0 ≥ 15 / P1 ≥ 11 / P2 ≥ 7 / P3 < 7。
func BandFromScore(score float64) string {
	switch {
	case score >= P0Threshold:
		return "P0"
	case score >= 11:
		return "P1"
	case score >= 7:
		return "P2"
	default:
		return "P3"
	}
}

// PriorityToCard 把 P0-P3 映射到看板卡优先级（urgent/high/medium/low）。
func PriorityToCard(p string) string {
	switch p {
	case "P0":
		return "urgent"
	case "P1":
		return "high"
	case "P3":
		return "low"
	default:
		return "medium"
	}
}

// UrgencyFromDueAt 由 SLA/期望完成时间算紧急度（0-4，§6.1）：
// 已超时=4 / <24h=3 / <72h=2 / 更远或无=1。
func UrgencyFromDueAt(dueAt *time.Time, now time.Time) int {
	if dueAt == nil {
		return 1
	}
	d := dueAt.Sub(now)
	switch {
	case d <= 0:
		return 4
	case d <= 24*time.Hour:
		return 3
	case d <= 72*time.Hour:
		return 2
	default:
		return 1
	}
}

// FrequencyFromCount 由近 24h 出现次数算复现频率（0-4，§6.1）。
func FrequencyFromCount(count int) int {
	switch {
	case count > 50:
		return 4
	case count > 10:
		return 3
	case count > 3:
		return 2
	default:
		return 1
	}
}

// SeverityFromLevel 降级分诊（LLM 不可用，§7.4 红线）：fatal=4 / error=3 / warning=1 / 其他=2。
func SeverityFromLevel(level string) int {
	switch level {
	case "fatal", "panic":
		return 4
	case "error":
		return 3
	case "warning":
		return 1
	default:
		return 2
	}
}
