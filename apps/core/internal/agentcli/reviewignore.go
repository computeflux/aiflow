package agentcli

import (
	"regexp"
	"strings"
)

// 评审排除规则（生成产物不进 reviewer）。
//
// 为什么必须排除：reviewer 会对 diff 里的文件逐条报问题；生成产物（合约 ABI 绑定、
// 单文件构建产物、lock 文件）没有可「修」的语义，coder 无法让它们通过复审 →
// 任务会被永远卡在「reviewer error → 重修」循环（2026-10-28 实测：minishop 任务
// 因 token.sol.abi.json / tokenSol.ts / .env.* 反复打回）。
//
// 规则来源（单一来源，不写两套）：
//   - 本文件的 defaultReviewExclude：与仓库无关的通用生成物；
//   - apps/<app>/.aiflow 的 review_ignore：项目特有生成物（相对 apps/<app>/，逗号分隔），
//     由 driver 读取、拼成仓库相对路径后经 OCR_EXCLUDE 传入。
var defaultReviewExclude = []string{
	// 锁文件 / 依赖清单（由包管理器生成）
	"**/package-lock.json",
	"**/bun.lock",
	"**/pnpm-lock.yaml",
	"**/yarn.lock",
	"**/go.sum",
	// 构建输出
	"**/node_modules/**",
	"**/dist/**",
	"**/build/**",
	"**/.next/**",
	"**/vendor/**",
	"**/*.min.js",
	"**/*.min.css",
	"**/*.map",
	// 代码生成
	"**/*.pb.go",
	"**/zz_generated*",
	"**/*.gen.go",
	"**/*.abi.json",
	"**/generated/**",
	// 环境配置（含密钥，既不该送外部 LLM，也不是「代码问题」）
	"**/.env",
	"**/.env.*",
	// 单文件构建产物（c2c 体验台：vite-plugin-singlefile 输出后 go:embed）
	"apps/c2c/index.html",
}

// ReviewExcludePatterns 返回生效的排除模式：**内置默认表 + extra（来自 .aiflow review_ignore）**，
// 去重保序。extra 为空 → 仅默认表。
func ReviewExcludePatterns(extra []string) []string {
	out := make([]string, 0, len(defaultReviewExclude)+len(extra))
	seen := make(map[string]bool, len(defaultReviewExclude)+len(extra))
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, p := range defaultReviewExclude {
		add(p)
	}
	for _, p := range extra {
		add(p)
	}
	return out
}

// SplitExcludePatterns 拆分逗号分隔的模式（去空白、丢空项）。
func SplitExcludePatterns(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ReviewExcludedWithPatterns 判断文件路径是否命中任一排除模式（gitignore 子集：** / * / ?）。
func ReviewExcludedWithPatterns(path string, patterns []string) bool {
	p := normalizeReviewPath(path)
	if p == "" {
		return false
	}
	for _, pat := range patterns {
		if re := compileExcludePattern(pat); re != nil && re.MatchString(p) {
			return true
		}
	}
	return false
}

func normalizeReviewPath(path string) string {
	p := strings.TrimSpace(strings.ReplaceAll(path, "\\", "/"))
	p = strings.TrimPrefix(p, "./")
	return p
}

// compileExcludePattern 把 gitignore 风格模式编译成正则（只支持常用的 ** / * / ?）：
//
//	**/foo   → 任意层级的 foo
//	foo/**   → foo 及其下所有文件
//	*.abi.json → 任意层级的 basename
//	apps/c2c/index.html → 精确匹配（含 / 时不加任意目录前缀）
func compileExcludePattern(pattern string) *regexp.Regexp {
	pat := normalizeReviewPath(pattern)
	if pat == "" {
		return nil
	}

	prefix := ""
	switch {
	case strings.HasPrefix(pat, "**/"):
		prefix = "(?:.*/)?"
		pat = strings.TrimPrefix(pat, "**/")
	case !strings.Contains(pat, "/"):
		prefix = "(?:.*/)?"
	}

	suffix := ""
	if strings.HasSuffix(pat, "/**") {
		pat = strings.TrimSuffix(pat, "/**")
		suffix = "(?:/.*)?"
	}

	var b strings.Builder
	b.WriteString("^")
	b.WriteString(prefix)
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(suffix)
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
