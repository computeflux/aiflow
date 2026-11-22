// Package deploy 是 S5 部署编排核心（docs/plan/S5-软件接管与部署编排.md §3.2–§3.4）：
// 统一 compose 解析（一服务一文件）+ 依赖序部署计划（拓扑排序）+
// docker compose CLI 执行（build/up/restart/stop/ps）+ 健康检查（deployctl 与
// health-patrol 共用同一套定义）+ 版本容器动态管理（dockerapi Engine API，
// 静态 compose 的唯一例外，§3.4）。
package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Project 是统一 compose 项目的解析结果（deploy/docker-compose.yml + include 下全部文件）。
type Project struct {
	RepoRoot   string
	ComposeDir string   // deploy/ 绝对路径
	Files      []string // include 文件（相对仓库根，按 include 序 = 依赖基线序）
	Services   map[string]*Service
}

// Service 是一个 compose 服务（一服务一文件）。
type Service struct {
	Name          string
	File          string // compose 文件（相对仓库根）
	ContainerName string // 缺省 = 服务名
	Image         string
	Build         *BuildSpec
	Ports         []PortBinding
	EnvFiles      []string          // env_file（相对 compose 文件目录）
	Environment   map[string]string // environment（map / K=V list 归一为 map）
	Volumes       []string
	Networks      []string
	DependsOn     map[string]string // 服务名 → condition
	HealthCheck   *HealthCheckSpec
	HealthURL     string // 从 healthcheck test 推导的 HTTP 端点（无则空）
	Restart       string
	OneShot       bool // 非 restart: always（一次性容器，健康 = 成功退出）
	MemLimit      string
	CPUs          string
	Profiles      []string
	Command       []string
	Entrypoint    []string
}

// BuildSpec 是 build 定义（context 相对 compose 文件目录）。
type BuildSpec struct {
	Context    string
	Dockerfile string
	Args       map[string]string
}

// PortBinding 是 ports 条目（host:container）。
// Raw 保留 compose 原始字符串（展示与改参回传用，保证往返无歧义：
// 2 段式 "8001:8001" 与 3 段式 "127.0.0.1:8001:8001" 的 Host 字段语义不同，
// 仅靠 Host/Container 无法还原 3 段式的宿主端口）。
type PortBinding struct {
	Host      string `json:"host"`
	Container string `json:"container"`
	Raw       string `json:"raw"`
}

// HealthCheckSpec 是 compose healthcheck 定义（json tag 与前端契约一致，snake_case）。
type HealthCheckSpec struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval"`
	Timeout     string   `json:"timeout"`
	Retries     int      `json:"retries"`
	StartPeriod string   `json:"start_period"`
}

// IsTools 是否为 tools profile 一次性工具。
func (s *Service) IsTools() bool {
	for _, p := range s.Profiles {
		if p == "tools" {
			return true
		}
	}
	return false
}

// externalComposeServices 不纳管红线（compose 服务名；与 asset 包一致；
// deploy 为底层包不反向依赖 asset，故此处独立一份，改一处需同步另一处）。
var externalComposeServices = map[string]bool{
	"postiz":          true,
	"postiz-postgres": true,
	"postiz-redis":    true,
}

// DefaultManagedSet 返回默认纳管集（DB 不可用时的兜底，S5「一条命令恢复」依赖它）：
// 全部非 tools、非 external 的 compose 服务。external 红线（postiz 等第三方）始终排除，
// 不纳管资产在 deploy all / 自愈 / 版本管理中零触碰。
func DefaultManagedSet(p *Project) map[string]bool {
	out := map[string]bool{}
	for name, svc := range p.Services {
		if svc.IsTools() || externalComposeServices[name] {
			continue
		}
		out[name] = true
	}
	return out
}

type composeFile struct {
	Name     string
	Services map[string]*serviceDef `yaml:"services"`
}

type composeRoot struct {
	Name     string   `yaml:"name"`
	Include  []string `yaml:"include"`
	Networks any      `yaml:"networks"`
}

type serviceDef struct {
	Image         string          `yaml:"image"`
	Build         *buildDef       `yaml:"build"`
	ContainerName string          `yaml:"container_name"`
	Restart       string          `yaml:"restart"`
	EnvFile       yaml.Node       `yaml:"env_file"`
	Environment   yaml.Node       `yaml:"environment"`
	Ports         []string        `yaml:"ports"`
	Volumes       []string        `yaml:"volumes"`
	Networks      yaml.Node       `yaml:"networks"`
	DependsOn     yaml.Node       `yaml:"depends_on"`
	HealthCheck   *healthCheckDef `yaml:"healthcheck"`
	MemLimit      string          `yaml:"mem_limit"`
	CPUs          string          `yaml:"cpus"`
	Labels        []string        `yaml:"labels"`
	Profiles      []string        `yaml:"profiles"`
	Command       yaml.Node       `yaml:"command"`
	Entrypoint    yaml.Node       `yaml:"entrypoint"`
}

type buildDef struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
}

type healthCheckDef struct {
	Test        yaml.Node `yaml:"test"`
	Interval    string    `yaml:"interval"`
	Timeout     string    `yaml:"timeout"`
	Retries     int       `yaml:"retries"`
	StartPeriod string    `yaml:"start_period"`
}

// LoadProject 解析统一 compose 项目（deploy/docker-compose.yml 的 include 链）。
func LoadProject(repoRoot string) (*Project, error) {
	composeDir := filepath.Join(repoRoot, "deploy")
	rootFile := filepath.Join(composeDir, "docker-compose.yml")
	raw, err := os.ReadFile(rootFile)
	if err != nil {
		return nil, fmt.Errorf("read docker-compose.yml: %w", err)
	}
	var root composeRoot
	if err := yaml.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse docker-compose.yml: %w", err)
	}
	if len(root.Include) == 0 {
		return nil, fmt.Errorf("docker-compose.yml 无 include（S0 结构破坏？）")
	}

	p := &Project{
		RepoRoot:   repoRoot,
		ComposeDir: composeDir,
		Services:   map[string]*Service{},
	}
	for _, rel := range root.Include {
		path := filepath.Join(composeDir, rel)
		p.Files = append(p.Files, filepath.ToSlash(filepath.Join("deploy", rel)))
		cf, err := parseComposeFile(path)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", rel, err)
		}
		for name, sd := range cf.Services {
			if _, ok := p.Services[name]; ok {
				return nil, fmt.Errorf("duplicate service %q in %s", name, rel)
			}
			svc, err := toService(name, filepath.ToSlash(filepath.Join("deploy", rel)), sd)
			if err != nil {
				return nil, fmt.Errorf("service %s in %s: %w", name, rel, err)
			}
			p.Services[name] = svc
		}
	}
	return p, nil
}

func parseComposeFile(path string) (*composeFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cf composeFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		return nil, err
	}
	cf.Name = filepath.Base(path)
	return &cf, nil
}

func toService(name, file string, sd *serviceDef) (*Service, error) {
	svc := &Service{
		Name:          name,
		File:          file,
		ContainerName: sd.ContainerName,
		Image:         sd.Image,
		Restart:       sd.Restart,
		Volumes:       sd.Volumes,
		Profiles:      sd.Profiles,
		MemLimit:      sd.MemLimit,
		CPUs:          sd.CPUs,
	}
	if sd.Build != nil {
		svc.Build = &BuildSpec{
			Context:    sd.Build.Context,
			Dockerfile: sd.Build.Dockerfile,
			Args:       sd.Build.Args,
		}
		if svc.Build.Dockerfile == "" {
			svc.Build.Dockerfile = "Dockerfile"
		}
	}
	for _, p := range sd.Ports {
		b, err := parsePort(p)
		if err != nil {
			return nil, err
		}
		svc.Ports = append(svc.Ports, b)
	}
	if v, err := stringListFromNode(&sd.EnvFile); err == nil {
		svc.EnvFiles = v
	}
	if m, err := envMapFromNode(&sd.Environment); err == nil {
		svc.Environment = m
	}
	if v, err := stringListFromNode(&sd.Networks); err == nil {
		svc.Networks = v
	}
	if m, err := dependsMapFromNode(&sd.DependsOn); err == nil {
		svc.DependsOn = m
	}
	if sd.HealthCheck != nil {
		test, err := stringListFromNode(&sd.HealthCheck.Test)
		if err != nil {
			return nil, fmt.Errorf("healthcheck test: %w", err)
		}
		svc.HealthCheck = &HealthCheckSpec{
			Test:        test,
			Interval:    sd.HealthCheck.Interval,
			Timeout:     sd.HealthCheck.Timeout,
			Retries:     sd.HealthCheck.Retries,
			StartPeriod: sd.HealthCheck.StartPeriod,
		}
		svc.HealthURL = extractHealthURL(test)
	}
	if v, err := stringListFromNode(&sd.Command); err == nil {
		svc.Command = v
	}
	if v, err := stringListFromNode(&sd.Entrypoint); err == nil {
		svc.Entrypoint = v
	}
	// 一次性容器判定：非 restart: always（on-failure / no）
	svc.OneShot = svc.Restart != "" && svc.Restart != "always"
	if svc.Restart == "" {
		svc.OneShot = true // compose 缺省 restart: "no"
	}
	if svc.ContainerName == "" {
		svc.ContainerName = svc.Name
	}
	return svc, nil
}

// parsePort 解析 compose ports 条目（"host:container" / "127.0.0.1:host:container" / "container"）。
// PortBinding.Host 是宿主机绑定地址（3 段式的 ip；2 段式为主机端口）；Container 是容器端口。
// Raw 保留原始字符串（展示/改参回传往返用）。
func parsePort(p string) (PortBinding, error) {
	p = strings.TrimSpace(p)
	parts := strings.Split(p, ":")
	switch len(parts) {
	case 1:
		return PortBinding{Host: "", Container: parts[0], Raw: p}, nil
	case 2:
		return PortBinding{Host: parts[0], Container: parts[1], Raw: p}, nil
	case 3:
		return PortBinding{Host: parts[0], Container: parts[2], Raw: p}, nil
	default:
		return PortBinding{}, fmt.Errorf("invalid port %q", p)
	}
}

// stringListFromNode 解析 YAML 标量或字符串列表（空节点返回 nil, nil）。
func stringListFromNode(n *yaml.Node) ([]string, error) {
	if n == nil || n.IsZero() {
		return nil, nil
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}, nil
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, item := range n.Content {
			out = append(out, item.Value)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unexpected yaml node kind %d", n.Kind)
	}
}

// envMapFromNode 解析 environment（map 或 K=V 列表）为 map。
func envMapFromNode(n *yaml.Node) (map[string]string, error) {
	if n == nil || n.IsZero() {
		return nil, nil
	}
	out := map[string]string{}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			out[n.Content[i].Value] = n.Content[i+1].Value
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, found := strings.Cut(item.Value, "=")
			if found {
				out[k] = v
			}
		}
	default:
		return nil, fmt.Errorf("unexpected environment node kind %d", n.Kind)
	}
	return out, nil
}

// dependsMapFromNode 解析 depends_on（map: name→{condition} 或 name 列表）为 map。
func dependsMapFromNode(n *yaml.Node) (map[string]string, error) {
	if n == nil || n.IsZero() {
		return nil, nil
	}
	out := map[string]string{}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			name := n.Content[i].Value
			cond := "service_started"
			if child := n.Content[i+1]; child.Kind == yaml.MappingNode {
				for j := 0; j < len(child.Content); j += 2 {
					if child.Content[j].Value == "condition" {
						cond = child.Content[j+1].Value
					}
				}
			} else if child.Kind == yaml.ScalarNode && child.Value != "" {
				cond = child.Value
			}
			out[name] = cond
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			out[item.Value] = "service_started"
		}
	default:
		return nil, fmt.Errorf("unexpected depends_on node kind %d", n.Kind)
	}
	return out, nil
}

// extractHealthURL 从 healthcheck test 推导 HTTP 健康端点
// （wget/curl 形式的 URL；TCP 拨号 / PID 存活 / 其它探针无 HTTP 端点）。
func extractHealthURL(test []string) string {
	joined := strings.Join(test, " ")
	for _, kw := range []string{"wget", "curl"} {
		idx := strings.Index(joined, kw)
		if idx < 0 {
			continue
		}
		rest := joined[idx+len(kw):]
		for _, tok := range strings.Fields(rest) {
			if strings.HasPrefix(tok, "http://") || strings.HasPrefix(tok, "https://") {
				return strings.Trim(tok, `"'`)
			}
		}
	}
	return ""
}

// TopoOrder 返回依赖序（Kahn 拓扑排序；种子序 = include 文件序 =
// 硬编码基线 infra → core → skills → evolve → tools，与 compose depends_on 一致）。
func (p *Project) TopoOrder() ([]string, error) {
	indeg := map[string]int{}
	adj := map[string][]string{} // dep → dependents
	for name, svc := range p.Services {
		indeg[name] = len(svc.DependsOn)
		for dep := range svc.DependsOn {
			if _, ok := p.Services[dep]; !ok {
				return nil, fmt.Errorf("service %s depends on unknown %s", name, dep)
			}
			adj[dep] = append(adj[dep], name)
		}
	}
	// 种子序：按 include 文件序 + 文件内出现序（稳定）
	order := []string{}
	seen := map[string]bool{}
	for _, file := range p.Files {
		for name, svc := range p.Services {
			if svc.File == file && !seen[name] {
				seen[name] = true
				order = append(order, name)
			}
		}
	}
	queue := make([]string, 0, len(order))
	indegCopy := map[string]int{}
	for _, n := range order {
		indegCopy[n] = indeg[n]
		if indeg[n] == 0 {
			queue = append(queue, n)
		}
	}
	out := make([]string, 0, len(order))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		out = append(out, n)
		for _, m := range adj[n] {
			indegCopy[m]--
			if indegCopy[m] == 0 {
				queue = append(queue, m)
			}
		}
	}
	if len(out) != len(order) {
		return nil, fmt.Errorf("depends_on 存在环（%d/%d 未排序）", len(out), len(order))
	}
	return out, nil
}
