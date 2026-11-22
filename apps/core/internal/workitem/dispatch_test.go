package workitem

import "testing"

// TestShouldBypassRejected 锁定"无明确目标 → rejected"旁路（§7.5）：
// 仅空 target_name 不进自动调度（S1 沙箱无法定位，必然失败）；core 系不再旁路（S9.8 §7.10）。
func TestShouldBypassRejected(t *testing.T) {
	cases := []struct {
		name       string
		targetName string
		want       bool
	}{
		{"core 系", "core", false},
		{"core-ui 系", "core-ui", false},
		{"空目标名", "", true},
		{"纯空白目标名", "   ", true},
		{"多选含 core", "pycraper,core", false},
		{"正常项目目标", "pycraper", false},
		{"正常项目目标2", "workflow-api", false},
	}
	for _, c := range cases {
		if got := shouldBypassRejected(c.targetName); got != c.want {
			t.Errorf("shouldBypassRejected(%q) = %v, want %v（%s）", c.targetName, got, c.want, c.name)
		}
	}
}
