package agentcli

import (
	"testing"
)

// TestReviewExcludedWithPatterns 锁定默认表：生成产物必须排除，人工代码不能误伤。
func TestReviewExcludedWithPatterns(t *testing.T) {
	patterns := ReviewExcludePatterns(nil)

	excluded := []string{
		"apps/minishop/package-lock.json",
		"apps/minishop/src/contracts/evm/token.sol.abi.json",
		"apps/minishop/.env.production",
		"apps/minishop/.env.ssr-test",
		"apps/c2c/index.html",
		"apps/minishop/dist/bundle.js",
		"apps/minishop/node_modules/react/index.js",
		"apps/core/hacks/gen/zz_generated_types.go",
		"apps/x/y.pb.go",
		"apps/x/build/app.min.js",
	}
	for _, p := range excluded {
		if !ReviewExcludedWithPatterns(p, patterns) {
			t.Errorf("应排除: %s", p)
		}
	}

	kept := []string{
		"apps/minishop/src/pages/RechargeModal.tsx",
		"apps/minishop/src/shared/api/c2c.ts",
		"apps/core/cmd/api/main.go",
		"apps/minishop/src/contracts/evm/tokenSol.ts", // 项目特有生成物：由 apps/minishop/.aiflow 追加，不在默认表
		"apps/core/AGENTS.md",
	}
	for _, p := range kept {
		if ReviewExcludedWithPatterns(p, patterns) {
			t.Errorf("不应排除: %s", p)
		}
	}
}

// TestReviewExcludeExtra 锁定 .aiflow 追加语义：extra 追加到默认表、去重、默认项仍生效。
func TestReviewExcludeExtra(t *testing.T) {
	extra := []string{
		"apps/minishop/src/contracts/evm/tokenSol.ts",
		"**/fixtures/**",
		"apps/minishop/src/contracts/evm/tokenSol.ts", // 重复项
	}
	patterns := ReviewExcludePatterns(extra)
	if len(patterns) != len(defaultReviewExclude)+2 {
		t.Fatalf("模式数 = %d, want %d", len(patterns), len(defaultReviewExclude)+2)
	}
	if !ReviewExcludedWithPatterns("apps/minishop/src/contracts/evm/tokenSol.ts", patterns) {
		t.Error("extra 中的文件应被排除")
	}
	if !ReviewExcludedWithPatterns("apps/x/fixtures/a.json", patterns) {
		t.Error("extra 中的 glob 应命中")
	}
	// 默认表仍然生效
	if !ReviewExcludedWithPatterns("apps/minishop/package-lock.json", patterns) {
		t.Error("追加语义下默认表仍应生效")
	}
	// 空 extra → 仅默认表
	if got := ReviewExcludePatterns(nil); len(got) != len(defaultReviewExclude) {
		t.Errorf("空 extra 时模式数 = %d, want %d", len(got), len(defaultReviewExclude))
	}
}

func TestCompileExcludePattern(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"**/foo", "a/b/foo", true},
		{"**/foo", "foo", true},
		{"foo/**", "foo/a/b.txt", true},
		{"foo/**", "foo", true},
		{"foo/**", "xfoo/a", false},
		{"*.abi.json", "a/b/token.sol.abi.json", true},
		{"*.abi.json", "a/b/token.ts", false},
		{"apps/c2c/index.html", "apps/c2c/index.html", true},
		{"apps/c2c/index.html", "x/apps/c2c/index.html", false},
	}
	for _, c := range cases {
		re := compileExcludePattern(c.pattern)
		if re == nil {
			t.Fatalf("编译失败: %s", c.pattern)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("%s 匹配 %s = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
