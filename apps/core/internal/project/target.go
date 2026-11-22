package project

import "strings"

// 目标名归一与「部署单元 → 项目」别名映射。
//
// 背景（卡 #102/#97 实测，2026-10-18）：系统里有三套名字，必须显式对齐，否则目标解析静默失败：
//   - error_logs.system / 容器名 = **部署单元名**（api、go-worker、pycraper…）；
//   - target_name / skills = 用户可选的**目标名**（projects 表项目名，可多值逗号分隔）；
//   - projects.name = **项目名**（1 项目 = 1 apps/<dir>；core / core-ui / actives / …）。
//
// 两者不同名时（api / go-worker 都属 apps/core），分诊产出的卡主目标未注册 → 调度秒拒
// （卡 #102）；审批 payload 里的多值目标串查 projects/skills 均不命中 →
// RunCD 报 skill_not_found → 任务永久卡在 approving（卡 #97）。
//
// 口径：与项目核心框架相关的一切都算 core（api / go-worker / audit / evolve-driver / …），
// board/core-ui 前端算 core-ui；技能不是目标（S9.8 §7.10 只按项目识别）。
//
// 未列入的名字原样返回——可能是真实项目名（pycraper / minishop / pi-web / dapp…），
// 交给 projects 注册表判定是否可执行。
var serviceAliases = map[string]string{
	// 核心框架（apps/core：cmd/* 各服务 + 运维 CLI）
	"api":                "core",
	"workflow-api":       "core",
	"core-api":           "core",
	"go-worker":          "core",
	"workflow-go-worker": "core",
	"core-worker":        "core",
	"audit":              "core",
	"evolve-driver":      "core",
	"sandbox-runner":     "core",
	"work-dispatch":      "core",
	"flex-server":        "core",
	"watchdog":           "core",
	"health-patrol":      "core",
	"ops-report":         "core",
	"knowledge-sync":     "core",
	"matrix-bot":         "core",
	"deployctl":          "core",
	"inventory":          "core",
	"agentctl":           "core",
	"skillctl":           "core",
	"agent-pi":           "core",
	"agent-ocr":          "core",
	// board 前端（apps/core-ui：原 board-ops/product/content/email/kit 已合并）
	"board":         "core-ui",
	"board-ops":     "core-ui",
	"board-product": "core-ui",
	"board-content": "core-ui",
	"board-email":   "core-ui",
	"board-kit":     "core-ui",
}

// AliasService 部署单元/服务名 → 项目名（未命中原样返回；大小写不敏感）。
func AliasService(name string) string {
	n := strings.TrimSpace(name)
	if n == "" {
		return ""
	}
	if p, ok := serviceAliases[strings.ToLower(n)]; ok {
		return p
	}
	return n
}

// NormalizeTarget 单目标归一：取项目名（去文件路径 / 函数名后缀）+ 部署单元别名映射。
//   - "pycraper/adapters/x.py"          → "pycraper"
//   - "pycraper.FetchActivities.fetch"  → "pycraper"
//   - "api"                             → "core"
func NormalizeTarget(target string) string {
	t := strings.TrimSpace(target)
	if i := strings.Index(t, "/"); i > 0 {
		t = t[:i]
	}
	if i := strings.Index(t, "."); i > 0 {
		t = t[:i]
	}
	return AliasService(strings.TrimSpace(t))
}

// NormalizeTargets 多值 target_name 归一：逐项 NormalizeTarget + 去空/去重/保序。
// 空/全无效 → ""（调用方按「无目标」处理）。
func NormalizeTargets(raw string) string {
	parts := ParseTargets(raw)
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, p := range parts {
		n := NormalizeTarget(p)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return strings.Join(out, ",")
}

// PrimaryTargetName 多值目标的主目标名（归一 + 别名后的第一个）；空/无值返回 ""。
// S1 执行链（沙箱 / 镜像 / CD 发布判定）只认主目标。
func PrimaryTargetName(raw string) string {
	parts := ParseTargets(NormalizeTargets(raw))
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// ParseTargets 拆分多值 target_name（trim/去空/去重/保序）。
func ParseTargets(s string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// ParseTargetsToSet 拆分多值 target_name 为集合（含空串判断用）。
func ParseTargetsToSet(s string) map[string]bool {
	out := make(map[string]bool)
	for _, p := range ParseTargets(s) {
		out[p] = true
	}
	return out
}
