package main

import "testing"

// TestHasConflictMarkers 冲突标记检测：只认行首标记，不能误伤代码里的 ==== 比较。
func TestHasConflictMarkers(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"无标记", "func main() {\n\tprintln(\"ok\")\n}\n", false},
		{"普通等号比较", "if a === b {\n  x()\n}\n", false},
		{"七等号在行首", "<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> main\n", true},
		{"只有分隔符", "a\n=======\nb\n", true},
		{"只有 ours 标记", "<<<<<<< HEAD\nours\n", true},
		{"只有 theirs 标记", "theirs\n>>>>>>> main\n", true},
		{"行内标记不算", " // <<<<<<< 说明文字\n", false},
		{"空内容", "", false},
	}
	for _, c := range cases {
		if got := hasConflictMarkers(c.in); got != c.want {
			t.Errorf("%s: hasConflictMarkers=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestStripCodeFence 模型爱加 ```围栏，必须剥掉。
func TestStripCodeFence(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"无围栏", "package main\n", "package main\n"},
		{"带语言围栏", "```go\npackage main\n```", "package main\n"},
		{"无语言围栏", "```\npackage main\n```", "package main\n"},
		{"前后空白", "  \n```ts\nconst a = 1;\n```\n  ", "const a = 1;\n"},
	}
	for _, c := range cases {
		if got := stripCodeFence(c.in); got != c.want {
			t.Errorf("%s: got %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestIsBinaryContent NUL 字节判定。
func TestIsBinaryContent(t *testing.T) {
	if isBinaryContent([]byte("hello world\n")) {
		t.Error("纯文本不应判为二进制")
	}
	if !isBinaryContent([]byte{0x89, 0x50, 0x4e, 0x47, 0x00, 0x01}) {
		t.Error("含 NUL 应判为二进制")
	}
	if isBinaryContent(nil) {
		t.Error("空内容不应判为二进制")
	}
}

// TestConflictAuditDetail 审计详情必须是合法 JSON（audit_logs.detail 是 JSON 列）。
func TestConflictAuditDetail(t *testing.T) {
	s := conflictAuditDetail("main", []string{"a.go"}, []string{"a.go"})
	if !contains(s, `"base":"main"`) || !contains(s, `"resolved_files":["a.go"]`) {
		t.Errorf("审计详情格式不符: %s", s)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
