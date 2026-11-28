package deploy

// asset_scan.go：S5 软件资产画像（原 internal/asset，批次 G 并入 deploy 域）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

// （不纳管红线名单 externalComposeServices 见 compose.go，S5 §3.1 统一口径。）

// 资产 → 对外接口扫描源码（gin 路由 / CLI 子命令；启发式，画像用）。
var routeSourceFiles = map[string][]string{
	"workflow-api":   {"cmd/api/main.go", "internal/approval/routes.go", "internal/intervention/routes.go", "internal/skillgateway/handler.go"},
	"generate-skill": {"cmd/generate-skill/main.go"},
	"evaluate-skill": {"cmd/evaluate-skill/main.go"},
	"search-skill":   {"cmd/search-skill/main.go"},
	"sandbox-runner": {"cmd/sandbox-runner/main.go"},
	"flex-server":    {"cmd/flex-server/main.go"},
}

var cliSourceFiles = map[string]string{
	"skillctl":  "cmd/skillctl/main.go",
	"agentctl":  "cmd/agentctl/main.go",
	"deployctl": "cmd/deployctl/main.go",
	"inventory": "cmd/inventory/main.go",
	"ci-runner": "deploy/Dockerfile.ci",
}

// Scanner 是仓库扫描器（S5 §3.1 软件资产画像）。
type Scanner struct {
	root string
	db   *gorm.DB
}

// NewScanner 创建扫描器。root = 仓库根；database 用于读已注册技能（skills 表）。
func NewScanner(root string, database *gorm.DB) *Scanner {
	return &Scanner{root: root, db: database}
}

// Scan 扫描仓库生成全部资产画像（compose 服务 + 外部资产）。
func (s *Scanner) Scan(ctx context.Context) ([]SoftwareAsset, error) {
	project, err := LoadProject(s.root)
	if err != nil {
		return nil, fmt.Errorf("解析 compose 项目: %w", err)
	}
	order, err := project.TopoOrder()
	if err != nil {
		return nil, err
	}

	envPorts := s.envPorts() // 根 .env 的 *_PORT 键（画像参考）
	skillByAsset := s.skillInterfaces()

	assets := make([]SoftwareAsset, 0, len(order)+8)
	// 1. compose 服务（一服务一文件）
	for _, name := range order {
		svc := project.Services[name]
		if svc.IsTools() {
			kind := "cli"
			if cliSourceFiles[name] == "" {
				kind = "go-service"
			}
			assets = append(assets, s.toolAsset(svc, kind, envPorts, skillByAsset))
			continue
		}
		managed := true
		method := "compose"
		if externalComposeServices[name] {
			managed, method = false, "external"
		}
		assets = append(assets, s.composeAsset(svc, managed, method, envPorts, skillByAsset))
	}

	// 2. 外部资产（apps/* 不在 compose 内的目录）
	for _, dir := range s.externalAppDirs(project) {
		assets = append(assets, s.externalAsset(dir, envPorts, skillByAsset))
	}
	return assets, nil
}

// externalAsset 生成外部资产（不纳管；红线：deploy all / 自愈 / 版本管理零触碰）。
// dir 为相对仓库根的路径（如 apps/minishop）。
func (s *Scanner) externalAsset(dir string, envPorts map[string]string, skillByAsset map[string][]interfaceItem) SoftwareAsset {
	abs := filepath.Join(s.root, dir)
	name := filepath.Base(dir)
	kind, language := externalKind(abs)
	ports := []map[string]string{}
	profile := map[string]any{"source": "external", "path": filepath.ToSlash(dir), "env_ports": envPorts}

	// 端口：本目录 .env 的 *_PORT 键 + Dockerfile EXPOSE + vite 配置
	if envMap := s.parseEnvFile(filepath.Join(abs, ".env")); envMap != nil {
		for k, v := range envMap {
			if strings.HasSuffix(k, "_PORT") {
				ports = append(ports, map[string]string{"env_key": k, "port": v})
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(abs, "Dockerfile")); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if f := strings.Fields(strings.TrimSpace(line)); len(f) >= 2 && strings.EqualFold(f[0], "EXPOSE") {
				ports = append(ports, map[string]string{"expose": f[1]})
			}
		}
	}
	if p, ok := s.vitePort(abs); ok {
		profile["vite_port"] = p
		ports = append(ports, map[string]string{"vite": strconv.Itoa(p)})
	}
	if mod := s.goModuleInfo(abs); mod != nil {
		profile["module"] = mod["module"]
		profile["go_version"] = mod["go_version"]
		profile["deps"] = map[string]int{"go_require": mod["require"].(int)}
	}
	if n := s.npmDepCount(abs); n > 0 {
		profile["deps"] = mergeInt(profile["deps"], "npm", n)
	}
	if n := s.pythonDepCount(abs); n > 0 {
		profile["deps"] = mergeInt(profile["deps"], "python", n)
	}
	sortPorts(ports)

	return SoftwareAsset{
		Name:        name,
		Path:        filepath.ToSlash(dir),
		Kind:        kind,
		Language:    language,
		Ports:       mustJSON(ports),
		StartMethod: "external",
		Interfaces:  mustJSON(s.interfacesFor(name, kind, skillByAsset)),
		Owner:       s.owner(name, envPorts),
		Profile:     mustJSON(profile),
		AutoManaged: 0,
	}
}

// composeAsset 生成 compose 服务资产。
func (s *Scanner) composeAsset(svc *Service, managed bool, method string, envPorts map[string]string, skillByAsset map[string][]interfaceItem) SoftwareAsset {
	kind := composeKind(svc, s.root)
	ports := []map[string]string{}
	for _, p := range svc.Ports {
		ports = append(ports, map[string]string{"host": p.Host, "container": p.Container})
	}
	profile := map[string]any{
		"source":         "compose",
		"compose_file":   svc.File,
		"container_name": svc.ContainerName,
		"image":          svc.Image,
		"one_shot":       svc.OneShot,
		"depends_on":     svc.DependsOn,
		"env_ports":      envPorts,
	}
	if svc.Build != nil {
		profile["build"] = map[string]any{
			"context":    svc.Build.Context,
			"dockerfile": svc.Build.Dockerfile,
			"args":       svc.Build.Args,
		}
		ctxDir := s.resolveBuildContext(svc)
		if mod := s.goModuleInfo(ctxDir); mod != nil {
			profile["module"] = mod["module"]
			profile["go_version"] = mod["go_version"]
			profile["deps"] = map[string]int{"go_require": int(mod["require"].(int))}
		}
	}
	if svc.HealthCheck != nil {
		profile["healthcheck"] = map[string]any{
			"test":         svc.HealthCheck.Test,
			"interval":     svc.HealthCheck.Interval,
			"timeout":      svc.HealthCheck.Timeout,
			"retries":      svc.HealthCheck.Retries,
			"start_period": svc.HealthCheck.StartPeriod,
			"http_url":     svc.HealthURL,
		}
	}
	if svc.MemLimit != "" || svc.CPUs != "" {
		profile["resources"] = map[string]string{"mem_limit": svc.MemLimit, "cpus": svc.CPUs}
	}

	return SoftwareAsset{
		Name:        svc.Name,
		Path:        svc.File,
		Kind:        kind,
		Language:    languageOf(kind),
		Ports:       mustJSON(ports),
		StartMethod: method,
		Service:     svc.Name,
		Interfaces:  mustJSON(s.interfacesFor(svc.Name, kind, skillByAsset)),
		Owner:       s.owner(svc.Name, envPorts),
		Profile:     mustJSON(profile),
		AutoManaged: b2i(managed),
	}
}

// toolAsset 生成 tools profile 一次性工具资产（纳管：参与 inventory / 部署镜像构建）。
func (s *Scanner) toolAsset(svc *Service, kind string, envPorts map[string]string, skillByAsset map[string][]interfaceItem) SoftwareAsset {
	profile := map[string]any{
		"source":       "compose",
		"compose_file": svc.File,
		"image":        svc.Image,
		"one_shot":     true,
		"env_ports":    envPorts,
	}
	if svc.Build != nil {
		profile["build"] = map[string]any{"context": svc.Build.Context, "dockerfile": svc.Build.Dockerfile, "args": svc.Build.Args}
	}
	return SoftwareAsset{
		Name:        svc.Name,
		Path:        svc.File,
		Kind:        kind,
		Language:    languageOf(kind),
		Ports:       "[]",
		StartMethod: "compose",
		Service:     svc.Name,
		Interfaces:  mustJSON(s.interfacesFor(svc.Name, kind, skillByAsset)),
		Owner:       s.owner(svc.Name, envPorts),
		Profile:     mustJSON(profile),
		AutoManaged: 1,
	}
}

// externalAppDirs 返回 apps/ 下不在 compose 内的子目录（外部资产候选）。
func (s *Scanner) externalAppDirs(project *Project) []string {
	entries, err := os.ReadDir(filepath.Join(s.root, "apps"))
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if project.Services[name] != nil {
			continue // 已在 compose 内（board / pi-web / social / mailer / auth）
		}
		out = append(out, filepath.Join("apps", name))
	}
	sort.Strings(out)
	return out
}

// composeKind 判定 compose 服务类型（按 Dockerfile / 构建上下文语言）。
func composeKind(svc *Service, root string) string {
	if svc.Build == nil {
		return "docker-app"
	}
	switch svc.Build.Dockerfile {
	case "deploy/Dockerfile.go":
		return "go-service"
	case "deploy/Dockerfile.board":
		return "vue-app"
	case "deploy/Dockerfile.core-ui":
		return "react-app"
	case "deploy/Dockerfile.auth", "deploy/Dockerfile.evolve":
		return "go-service"
	case "deploy/Dockerfile.pi-web", "deploy/Dockerfile.ocr":
		return "docker-app"
	case "deploy/Dockerfile.ci":
		return "cli"
	}
	// 上下文目录语言（pyproject.toml → python；go.mod → go）
	ctxDir := resolveContext(root, filepath.Dir(svc.File), svc.Build.Context)
	if fileExists(filepath.Join(ctxDir, "pyproject.toml")) {
		return "python-worker"
	}
	if fileExists(filepath.Join(ctxDir, "go.mod")) {
		return "go-service"
	}
	return "docker-app"
}

// externalKind 判定外部资产类型（go.mod / package.json / pyproject.toml）。
func externalKind(dir string) (kind, language string) {
	switch {
	case fileExists(filepath.Join(dir, "go.mod")):
		if fileExists(filepath.Join(dir, "Dockerfile")) || fileExists(filepath.Join(dir, "docker.sh")) || fileExists(filepath.Join(dir, "docker-compose.yml")) {
			return "docker-app", "go"
		}
		return "go-service", "go"
	case fileExists(filepath.Join(dir, "package.json")):
		raw, _ := os.ReadFile(filepath.Join(dir, "package.json"))
		var pkg struct {
			Dependencies    map[string]string `json:"dependencies"`
			DevDependencies map[string]string `json:"devDependencies"`
		}
		_ = json.Unmarshal(raw, &pkg)
		deps := map[string]bool{}
		for k := range pkg.Dependencies {
			deps[k] = true
		}
		for k := range pkg.DevDependencies {
			deps[k] = true
		}
		has := func(sub string) bool {
			for k := range deps {
				if strings.Contains(k, sub) {
					return true
				}
			}
			return false
		}
		switch {
		case has("react"):
			return "react-app", "react (typescript)"
		case has("vue"):
			return "vue-app", "vue (typescript)"
		case has("next"):
			return "docker-app", "node (next)"
		default:
			return "docker-app", "node"
		}
	case fileExists(filepath.Join(dir, "pyproject.toml")):
		return "python-worker", "python"
	default:
		return "docker-app", ""
	}
}

func languageOf(kind string) string {
	switch kind {
	case "go-service":
		return "go"
	case "python-worker":
		return "python"
	case "vue-app":
		return "vue (typescript)"
	case "react-app":
		return "react (typescript)"
	case "cli":
		return "go"
	default:
		return "docker"
	}
}

// envPorts 解析根 .env 的 *_PORT 键（画像参考，不映射到具体资产）。
func (s *Scanner) envPorts() map[string]string {
	m := s.parseEnvFile(filepath.Join(s.root, ".env"))
	out := map[string]string{}
	for k, v := range m {
		if strings.HasSuffix(k, "_PORT") {
			out[k] = v
		}
	}
	return out
}

// owner 属主：根 .env 的 OWNER_<NAME> 约定（- → _、大写），缺省 wetee。
func (s *Scanner) owner(name string, _ map[string]string) string {
	envMap := s.parseEnvFile(filepath.Join(s.root, ".env"))
	key := "OWNER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	if v, ok := envMap[key]; ok && v != "" {
		return v
	}
	return "wetee"
}

// skillInterfaces 已注册技能 → 资产名映射（skills 表，S5 §3.1 对外接口维度）。
// 映射：skill.container（如 social）或 skill.name 本身（generate / evaluate / search）。
type interfaceItem struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
}

// resolveBuildContext 解析 build context（相对 compose 文件目录）。
func (s *Scanner) resolveBuildContext(svc *Service) string {
	return resolveContext(s.root, filepath.Dir(svc.File), svc.Build.Context)
}
