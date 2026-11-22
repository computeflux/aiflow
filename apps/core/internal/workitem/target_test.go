package workitem

import "testing"

// TestShouldBypassRejectedAliases 别名映射后「api」不再是“无目标”（旧实现会因此旁路）。
func TestShouldBypassRejectedAliases(t *testing.T) {
	cases := map[string]bool{
		"api":      false, // 别名 → core：不旁路
		"":         true,
		"  ":       true,
		"core":     false,
		"core-ui":  false,
		"api,core": false,
	}
	for in, want := range cases {
		if got := shouldBypassRejected(in); got != want {
			t.Errorf("shouldBypassRejected(%q) = %v, want %v", in, got, want)
		}
	}
}
