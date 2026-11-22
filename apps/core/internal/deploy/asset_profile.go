package deploy

// asset_profile.go：S5 软件资产画像（原 internal/asset，批次 G 并入 deploy 域）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// profile.go：资产画像的辅助探测（go.mod / package.json / pyproject / vite 端口 /
// .env 解析 / 端口排序 / JSON 归一）。自 scan.go 拆出（批次 F，纯机械搬迁，行为无变更）。

// goModuleInfo 解析 go.mod（module / go 版本 / require 依赖数）。
func (s *Scanner) goModuleInfo(dir string) map[string]any {
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return nil
	}
	out := map[string]any{}
	inRequire := false
	requireCount := 0
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "module "):
			out["module"] = strings.TrimSpace(strings.TrimPrefix(t, "module "))
		case strings.HasPrefix(t, "go "):
			out["go_version"] = strings.TrimSpace(strings.TrimPrefix(t, "go "))
		case t == "require (":
			inRequire = true
		case inRequire && t == ")":
			inRequire = false
		case inRequire && strings.Contains(t, " "):
			if !strings.HasPrefix(t, "//") {
				requireCount++
			}
		}
	}
	out["require"] = requireCount
	if out["module"] == nil {
		return nil
	}
	return out
}

// npmDepCount 统计 package.json 依赖数（dependencies + devDependencies）。
func (s *Scanner) npmDepCount(dir string) int {
	raw, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return 0
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return 0
	}
	return len(pkg.Dependencies) + len(pkg.DevDependencies)
}

// pythonDepCount 统计 pyproject.toml [dependencies] 条目数。
func (s *Scanner) pythonDepCount(dir string) int {
	raw, err := os.ReadFile(filepath.Join(dir, "pyproject.toml"))
	if err != nil {
		return 0
	}
	n, inDeps := 0, false
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "[dependencies]"):
			inDeps = true
		case strings.HasPrefix(t, "["):
			inDeps = false
		case inDeps && strings.HasPrefix(t, `"`) && !strings.HasPrefix(t, `#`):
			n++
		}
	}
	return n
}

// vitePort 读 vite 配置的 server.port。
func (s *Scanner) vitePort(dir string) (int, bool) {
	for _, rel := range []string{"vite.config.ts", "vite.config.js"} {
		raw, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			continue
		}
		re := regexp.MustCompile(`port\s*:\s*(\d+)`)
		if m := re.FindSubmatch(raw); m != nil {
			n, _ := strconv.Atoi(string(m[1]))
			return n, true
		}
	}
	return 0, false
}

// parseEnvFile 解析 .env（KEY=VALUE 行；注释 / 空行跳过；只取键值对，不做 shell 展开）。
func (s *Scanner) parseEnvFile(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}

func resolveContext(root, base, rel string) string {
	if rel == "" {
		rel = "."
	}
	return filepath.Join(root, base, rel)
}

func mergeInt(v any, key string, n int) map[string]int {
	m, ok := v.(map[string]int)
	if !ok {
		m = map[string]int{}
	}
	m[key] = n
	return m
}

func sortPorts(ports []map[string]string) {
	keys := make([]string, 0, len(ports))
	for _, p := range ports {
		keys = append(keys, mustJSON(p))
	}
	order := map[string]int{}
	for i, k := range keys {
		order[k] = i
	}
	sort.SliceStable(ports, func(i, j int) bool {
		return order[mustJSON(ports[i])] < order[mustJSON(ports[j])]
	})
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func b2i(b bool) int8 {
	if b {
		return 1
	}
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
