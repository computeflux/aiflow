package ops

import (
	"testing"
)

func TestParseSuggestions_StrictJSON(t *testing.T) {
	raw := `[{"title":"优化 X 点赞限流","reason":"频繁 timeout","suggested_change":"调大 cooldown","risk_level":"low","suggested_card":{"title":"调大 X 点赞 cooldown","description":"...","priority":"medium","checklist":["改 .env","重建 pyreplay"]}}]`
	out := parseSuggestions(raw)
	if len(out) != 1 {
		t.Fatalf("expected 1 suggestion, got %d", len(out))
	}
	if out[0].RiskLevel != "low" || out[0].SuggestedCard == nil || len(out[0].SuggestedCard.Checklist) != 2 {
		t.Fatalf("bad parse: %+v", out[0])
	}
}

func TestParseSuggestions_CodeFence(t *testing.T) {
	raw := "```json\n[{\"title\":\"a\",\"reason\":\"r\",\"suggested_change\":\"c\",\"risk_level\":\"high\"}]\n```"
	out := parseSuggestions(raw)
	if len(out) != 1 || out[0].Title != "a" {
		t.Fatalf("expected fence-stripped parse, got %+v", out)
	}
}

func TestParseSuggestions_Garbage(t *testing.T) {
	if out := parseSuggestions("not json at all"); out != nil {
		t.Fatalf("expected nil for garbage, got %+v", out)
	}
}

func TestApplyRiskRedLine_HighNoCard(t *testing.T) {
	sug := []Suggestion{
		{Title: "a", RiskLevel: "high", SuggestedCard: &SuggestedCard{Title: "x"}},
		{Title: "b", RiskLevel: "low", SuggestedCard: &SuggestedCard{Title: "y"}},
	}
	applyRiskRedLine(&sug)
	if !sug[0].ManualPROnly || sug[0].SuggestedCard != nil {
		t.Fatalf("high risk must be manual-only with no card: %+v", sug[0])
	}
	if sug[1].ManualPROnly || sug[1].SuggestedCard == nil {
		t.Fatalf("low risk must keep card: %+v", sug[1])
	}
}

func TestPercentile(t *testing.T) {
	if v, ok := percentile([]int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 0.99); !ok || v != 10 {
		t.Fatalf("p99 of 1..10 = %d (ok=%v), want 10", v, ok)
	}
	if _, ok := percentile(nil, 0.99); ok {
		t.Fatalf("empty percentile should be ok=false")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("a|b\nc", 2); got != "a/…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate("short", 10); got != "short" {
		t.Fatalf("short truncate = %q", got)
	}
}

func TestTopSlowSkills(t *testing.T) {
	skills := []SkillMetrics{
		{Name: "fast", P99Ms: 100, Total: 5},
		{Name: "slow", P99Ms: 5000, Total: 5, SuccessRate: 0.8},
		{Name: "empty", P99Ms: 0, Total: 0},
	}
	out := topSlowSkills(skills, 2)
	if len(out) != 2 || out[0] == "" {
		t.Fatalf("topSlowSkills = %v", out)
	}
}
