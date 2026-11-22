package deploy

// asset_interfaces.go：S5 软件资产画像（原 internal/asset，批次 G 并入 deploy 域）。

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/wetee/ai-workflow/internal/db"
)

// interfaces.go：资产对外接口扫描（技能 / gin 路由 / CLI 子命令启发式）。
// 自 scan.go 拆出（批次 F，纯机械搬迁，行为无变更）。

func (s *Scanner) skillInterfaces() map[string][]interfaceItem {
	var skills []db.Skill
	if s.db == nil {
		return map[string][]interfaceItem{}
	}
	if err := s.db.Find(&skills).Error; err != nil {
		return map[string][]interfaceItem{}
	}
	out := map[string][]interfaceItem{}
	for _, sk := range skills {
		item := interfaceItem{Type: "skill", Name: sk.Name, Endpoint: "/api/skills/" + sk.Name + "/invoke"}
		if sk.Container != "" {
			out[sk.Container] = append(out[sk.Container], item)
		}
		out[sk.Name] = append(out[sk.Name], item)
	}
	return out
}

// interfacesFor 汇总资产的对外接口（技能 + gin 路由 + CLI 子命令）。
func (s *Scanner) interfacesFor(name, kind string, skillByAsset map[string][]interfaceItem) []interfaceItem {
	out := skillByAsset[name]
	if files, ok := routeSourceFiles[name]; ok && kind == "go-service" {
		for _, rel := range files {
			out = append(out, s.scanGinRoutes(rel)...)
		}
	}
	if rel, ok := cliSourceFiles[name]; ok && kind == "cli" && rel != "deploy/Dockerfile.ci" {
		for _, sub := range s.scanCLISubcommands(rel) {
			out = append(out, interfaceItem{Type: "cli", Name: sub})
		}
	}
	return out
}

var routeRe = regexp.MustCompile(`(?m)[a-zA-Z_][a-zA-Z0-9_]*\.(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\(\s*"([^"]+)"`)
var cliCaseRe = regexp.MustCompile(`(?m)case\s+"([^"]+)"\s*:`)

// scanGinRoutes 扫 gin 路由注册（启发式：r.GET("/...") 等）。
func (s *Scanner) scanGinRoutes(rel string) []interfaceItem {
	raw, err := os.ReadFile(filepath.Join(s.root, rel))
	if err != nil {
		return nil
	}
	out := []interfaceItem{}
	seen := map[string]bool{}
	for _, m := range routeRe.FindAllStringSubmatch(string(raw), -1) {
		key := m[1] + " " + m[2]
		if !seen[key] {
			seen[key] = true
			out = append(out, interfaceItem{Type: "http", Name: m[1] + " " + m[2], Endpoint: m[2]})
		}
	}
	return out
}

// scanCLISubcommands 扫 CLI 子命令（switch args[0] 的 case "..."）。
func (s *Scanner) scanCLISubcommands(rel string) []string {
	raw, err := os.ReadFile(filepath.Join(s.root, rel))
	if err != nil {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, m := range cliCaseRe.FindAllStringSubmatch(string(raw), -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	return out
}
