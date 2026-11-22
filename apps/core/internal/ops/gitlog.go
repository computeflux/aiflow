package ops

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// GitHotspots 统计近 7 天 `apps/*` 与 `internal/*` 的 git 变更热点（按文件 commit 次数）。
// 仓库不可用 / git 失败时返回空 map（不阻断建议生成）。
func GitHotspots(ctx context.Context, repoRoot string) (map[string]int, error) {
	cmd := exec.CommandContext(ctx, "git",
		"log", "--since=7.days.ago", "--name-only", "--pretty=format:", "--", "apps", "internal")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		// git 不可用（如容器未挂载仓库）→ 降级为空热点
		return map[string]int{}, nil
	}
	counts := map[string]int{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, "\t") {
			continue
		}
		counts[line]++
	}
	return counts, nil
}

// TopGitHotspots 取变更次数最多的前 n 个文件。
func TopGitHotspots(counts map[string]int, n int) []string {
	type kv struct {
		file string
		n    int
	}
	items := make([]kv, 0, len(counts))
	for f, c := range counts {
		items = append(items, kv{f, c})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].n != items[j].n {
			return items[i].n > items[j].n
		}
		return items[i].file < items[j].file
	})
	out := make([]string, 0, n)
	for i := 0; i < len(items) && i < n; i++ {
		out = append(out, fmt.Sprintf("%s (%d 次)", items[i].file, items[i].n))
	}
	return out
}
