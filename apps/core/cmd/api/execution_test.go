package main

import "testing"

// TestNormalizeJSONText MySQL JSON 列的空值（字面量 null / 空对象 / 空数组）应归为空串，
// 否则没写过 review 的任务会让看板多出一个点开只有 "null" 的「审核记录」入口。
func TestNormalizeJSONText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"null", ""},
		{" null ", ""},
		{"{}", ""},
		{"[]", ""},
		{`{"ok":true,"comments":[]}`, `{"ok":true,"comments":[]}`},
		{`{"ok":true,"comments":[{"file":"a.ts","line":1,"level":"error","message":"x"}]}`,
			`{"ok":true,"comments":[{"file":"a.ts","line":1,"level":"error","message":"x"}]}`},
	}
	for _, tc := range cases {
		if got := normalizeJSONText(tc.in); got != tc.want {
			t.Errorf("normalizeJSONText(%q)=%q，期望 %q", tc.in, got, tc.want)
		}
	}
}
