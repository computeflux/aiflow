package workitem

import (
	"testing"
	"time"
)

func TestComputeScore(t *testing.T) {
	w := DefaultWeights() // 3,2,2,1
	// 3*4 + 2*4 + 2*4 + 1*4 = 12+8+8+4 = 32
	if got := ComputeScore(w, 4, 4, 4, 4); got != 32 {
		t.Errorf("ComputeScore(4,4,4,4) = %v, want 32", got)
	}
	// 3*0 + 2*1 + 2*1 + 1*1 = 0+2+2+1 = 5
	if got := ComputeScore(w, 0, 1, 1, 1); got != 5 {
		t.Errorf("ComputeScore(0,1,1,1) = %v, want 5", got)
	}
}

func TestBandFromScore(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{15, "P0"},
		{20, "P0"},
		{14.9, "P1"},
		{11, "P1"},
		{10.9, "P2"},
		{7, "P2"},
		{6.9, "P3"},
		{0, "P3"},
	}
	for _, c := range cases {
		if got := BandFromScore(c.score); got != c.want {
			t.Errorf("BandFromScore(%v) = %s, want %s", c.score, got, c.want)
		}
	}
}

func TestPriorityToCard(t *testing.T) {
	cases := map[string]string{
		"P0": "urgent", "P1": "high", "P2": "medium", "P3": "low", "": "medium",
	}
	for p, want := range cases {
		if got := PriorityToCard(p); got != want {
			t.Errorf("PriorityToCard(%q) = %s, want %s", p, got, want)
		}
	}
}

func TestUrgencyFromDueAt(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		due  *time.Time
		want int
	}{
		{"nil", nil, 1},
		{"overdue", ptrTime(now.Add(-time.Hour)), 4},
		{"within_24h", ptrTime(now.Add(12 * time.Hour)), 3},
		{"within_72h", ptrTime(now.Add(48 * time.Hour)), 2},
		{"far", ptrTime(now.Add(7 * 24 * time.Hour)), 1},
	}
	for _, c := range cases {
		if got := UrgencyFromDueAt(c.due, now); got != c.want {
			t.Errorf("UrgencyFromDueAt(%s) = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestFrequencyFromCount(t *testing.T) {
	cases := []struct {
		count int
		want  int
	}{
		{0, 1}, {3, 1}, {4, 2}, {10, 2}, {11, 3}, {50, 3}, {51, 4},
	}
	for _, c := range cases {
		if got := FrequencyFromCount(c.count); got != c.want {
			t.Errorf("FrequencyFromCount(%d) = %d, want %d", c.count, got, c.want)
		}
	}
}

func TestSeverityFromLevel(t *testing.T) {
	cases := map[string]int{
		"fatal": 4, "panic": 4, "error": 3, "warning": 1, "other": 2,
	}
	for lvl, want := range cases {
		if got := SeverityFromLevel(lvl); got != want {
			t.Errorf("SeverityFromLevel(%q) = %d, want %d", lvl, got, want)
		}
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
