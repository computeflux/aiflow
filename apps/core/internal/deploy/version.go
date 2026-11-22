package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wetee/ai-workflow/internal/db"
	"github.com/wetee/ai-workflow/internal/dockerapi"
	"gorm.io/gorm"
)

// 版本容器常量（S5 §3.4：静态 compose 的唯一例外）。
const (
	// TestNetworkName 测试网络（internal bridge：不可访问外部网络；
	// 版本实例同时接入生产网 workflow-net 以可达生产数据，§3.4）。
	TestNetworkName = "workflow-test-net"
	// 版本实例内部端口 195xx 段（避免与生产实例同网同端口冲突）。
	versionPortBase = 19500
	versionPortSpan = 100
	// 版本实例容器名：skill-<name>-<version>（如 skill-like-1.4.1）。
	versionLabel = "compute-flux.version-instance"
)

// VersionManager 动态管理测试 / 灰度版本容器（deployctl version up/down，§3.4）。
//
// 灰度 = 网关在旧/新两个版本实例的 addr 间按 traffic_split 分流
// （skillgateway 按版本 config.addr 路由，§3.4）；回滚 = 分流清零 + 新实例停。
// 全部动作写 audit_logs（经调用方）；接口不变，k8s 下底层执行器换 kubectl（§3.5）。
type VersionManager struct {
	database *gorm.DB
	docker   *dockerapi.Client
	project  *Project
	root     string // 仓库根（读 env_file）
}

// NewVersionManager 创建版本容器管理器。
func NewVersionManager(database *gorm.DB, docker *dockerapi.Client, project *Project, root string) *VersionManager {
	return &VersionManager{database: database, docker: docker, project: project, root: root}
}

// VersionContainerName 版本实例容器名。
func VersionContainerName(skillName, version string) string {
	return fmt.Sprintf("skill-%s-%s", skillName, version)
}

// VersionPort 版本实例内部端口（195xx 段，按 skill 名确定性分配，测试/灰度实例同端口）。
func VersionPort(skillName string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(skillName))
	return versionPortBase + int(h.Sum32())%versionPortSpan
}

// Up 起版本实例（env = "test" 或 "gray"）。
//
// 语义（§3.4）：
//   - test：独立测试网络起测试实例（195xx 内部端口），status=testing，供人工测试；
//   - gray：生产网络起版本实例，网关 skill_versions.config.addr 指向该实例，
//     status=gray，traffic_split=5（对接 S1 灰度推进）；
//   - cli 型技能无常驻实例：只更新版本状态（invoker 按次 docker run 镜像，版本 = image tag）。
//
// 返回实例地址（service 型）或说明（cli 型）。
func (m *VersionManager) Up(ctx context.Context, skillName, version, env string) (string, error) {
	var skill db.Skill
	if err := m.database.WithContext(ctx).Where("name = ?", skillName).First(&skill).Error; err != nil {
		return "", fmt.Errorf("查 skill %s: %w", skillName, err)
	}
	var ver db.SkillVersion
	if err := m.database.WithContext(ctx).
		Where("skill_id = ? AND version = ?", skill.ID, version).First(&ver).Error; err != nil {
		return "", fmt.Errorf("查 skill_versions %s v%s: %w", skillName, version, err)
	}
	if ver.Image == "" {
		return "", fmt.Errorf("skill_versions %s v%s 无 image（版本 = image tag，先构建并登记）", skillName, version)
	}

	// cli 型技能无常驻实例（§3.4）
	if skill.Type == "cli" {
		ver.TrafficSplit = 0
		if env == "gray" {
			ver.Status = "gray"
			ver.TrafficSplit = 5
		} else {
			ver.Status = "testing"
		}
		if err := m.saveVersion(ctx, &ver); err != nil {
			return "", err
		}
		return fmt.Sprintf("cli 型技能无常驻实例（版本=%s，invoker 按次 docker run 镜像）", ver.Image), nil
	}

	// service 型：找基线 compose 服务（取 env / 资源限额 / 网络）
	base, err := m.baseService(&skill)
	if err != nil {
		return "", err
	}
	name := VersionContainerName(skillName, version)
	port := VersionPort(skillName)

	// 镜像必须已存在（本地构建或由 CI 产出）
	if ok, err := m.imageExists(ctx, ver.Image); err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("镜像 %s 不存在（先构建：docker build -t %s ...）", ver.Image, ver.Image)
	}

	// 测试网络（internal：不可访问外部网络；版本实例同时接生产网以可达生产数据）
	networks := []string{"workflow-net"}
	if env == "test" {
		if err := m.docker.CreateNetwork(ctx, TestNetworkName, "bridge", true); err != nil {
			return "", fmt.Errorf("创建测试网络 %s: %w", TestNetworkName, err)
		}
		networks = append(networks, TestNetworkName)
	}

	// 已存在同名实例（重跑）：先停掉旧的
	if exists, _ := m.docker.ContainerExists(ctx, name); exists {
		log.Printf("version up: 实例 %s 已存在，先停旧实例", name)
		if err := m.docker.StopContainer(ctx, name, 10); err != nil {
			log.Printf("WARN: stop %s: %v", name, err)
		}
		if err := m.docker.RemoveContainerWithVolumes(ctx, name, true); err != nil {
			return "", fmt.Errorf("删除旧实例 %s: %w", name, err)
		}
	}

	envs, err := m.serviceEnv(base)
	if err != nil {
		return "", err
	}
	// 195xx 端口约定：版本实例的监听端口由版本 config 的 port_env 字段指定
	//（如 SKILL_LIKE_PORT）。不注入时实例继承基线服务端口（如 9500），
	// 与生产实例同网同端口冲突，config.addr 指向 195xx 必连不上。
	if portEnv, err := m.configPortEnv(&ver); err != nil {
		return "", err
	} else if portEnv != "" {
		envs = append(envs, fmt.Sprintf("%s=%d", portEnv, port))
	}
	spec := dockerapi.ContainerCreateSpec{
		Name:     name,
		Image:    ver.Image,
		Env:      envs,
		Networks: networks,
		Memory:   parseMemLimit(base.MemLimit),
		NanoCPUs: parseCPUs(base.CPUs),
		Labels:   map[string]string{"part-of": "compute-flux-workflow", versionLabel: "1"},
	}
	id, err := m.docker.CreateContainer(ctx, spec)
	if err != nil {
		return "", fmt.Errorf("创建版本实例 %s: %w", name, err)
	}
	if err := m.docker.StartContainer(ctx, id); err != nil {
		m.docker.RemoveContainerWithVolumes(ctx, id, true)
		return "", fmt.Errorf("启动版本实例 %s: %w", name, err)
	}
	if ok, reason := m.waitRunning(ctx, name, 120*time.Second); !ok {
		m.docker.RemoveContainerWithVolumes(ctx, id, true)
		return "", fmt.Errorf("版本实例 %s 未进入 running: %s", name, reason)
	}

	// 网关路由：config.addr 指向版本实例（skillgateway 按版本 config.addr 路由，§3.4）
	addr := fmt.Sprintf("http://%s:%d", name, port)
	if err := m.setVersionAddr(ctx, &ver, addr); err != nil {
		return "", err
	}
	ver.TrafficSplit = 0
	if env == "gray" {
		ver.Status = "gray"
		ver.TrafficSplit = 5 // 对接 S1 灰度推进（5→50→100）
	} else {
		ver.Status = "testing"
	}
	if err := m.saveVersion(ctx, &ver); err != nil {
		return "", err
	}
	return addr, nil
}

// Down 停版本实例（回滚 / retired，§3.4）：
// 停 + 删容器；分流清零；status：gray→retired、testing→draft；
// config.addr 还原为基线服务 addr（skills 行）。
func (m *VersionManager) Down(ctx context.Context, skillName, version string) error {
	var skill db.Skill
	if err := m.database.WithContext(ctx).Where("name = ?", skillName).First(&skill).Error; err != nil {
		return fmt.Errorf("查 skill %s: %w", skillName, err)
	}
	var ver db.SkillVersion
	if err := m.database.WithContext(ctx).
		Where("skill_id = ? AND version = ?", skill.ID, version).First(&ver).Error; err != nil {
		return fmt.Errorf("查 skill_versions %s v%s: %w", skillName, version, err)
	}

	name := VersionContainerName(skillName, version)
	if exists, _ := m.docker.ContainerExists(ctx, name); exists {
		if err := m.docker.StopContainer(ctx, name, 10); err != nil {
			return fmt.Errorf("停版本实例 %s: %w", name, err)
		}
		if err := m.docker.RemoveContainerWithVolumes(ctx, name, true); err != nil {
			return fmt.Errorf("删版本实例 %s: %w", name, err)
		}
	}

	ver.TrafficSplit = 0
	switch ver.Status {
	case "gray":
		ver.Status = "retired"
	case "testing":
		ver.Status = "draft"
	}
	if err := m.setVersionAddr(ctx, &ver, skill.Addr); err != nil {
		return err
	}
	return m.saveVersion(ctx, &ver)
}

// StopVersion 是 approval.gray 灰度回滚的钩子（S1 §3.4：回滚 = 分流清零 + 新实例停）。
func (m *VersionManager) StopVersion(ctx context.Context, skillName, version string) error {
	return m.Down(ctx, skillName, version)
}

// baseService 找 skill 的基线 compose 服务（skill.container 优先；
// 否则按 skills.addr 的 host 匹配 compose 服务容器名/服务名）。
func (m *VersionManager) baseService(skill *db.Skill) (*Service, error) {
	if skill.Container != "" {
		if svc, ok := m.project.Services[skill.Container]; ok {
			return svc, nil
		}
	}
	host := strings.TrimPrefix(strings.TrimPrefix(skill.Addr, "http://"), "https://")
	host = strings.Split(host, ":")[0]
	for _, svc := range m.project.Services {
		if svc.Name == host || svc.ContainerName == host {
			return svc, nil
		}
	}
	return nil, fmt.Errorf("skill %s 找不到基线 compose 服务（container=%q addr=%q）", skill.Name, skill.Container, skill.Addr)
}

// configPortEnv 读版本 config 快照的 port_env 字段（版本实例监听端口对应的 env 名）。
func (m *VersionManager) configPortEnv(ver *db.SkillVersion) (string, error) {
	if ver.Config == "" || ver.Config == "null" {
		return "", nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(ver.Config), &cfg); err != nil {
		return "", fmt.Errorf("解析 version config: %w", err)
	}
	pe, _ := cfg["port_env"].(string)
	return pe, nil
}

// serviceEnv 组装版本实例 env（基线服务的 env_file + environment 覆盖）。
func (m *VersionManager) serviceEnv(svc *Service) ([]string, error) {
	envs := map[string]string{}
	dir := filepath.Join(m.root, filepath.Dir(svc.File))
	for _, rel := range svc.EnvFiles {
		path := filepath.Join(dir, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue // env_file 缺失不阻断（compose 行为一致）
			}
			return nil, fmt.Errorf("读 env_file %s: %w", path, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok {
				envs[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	for k, v := range svc.Environment {
		envs[k] = v
	}
	out := make([]string, 0, len(envs))
	for k, v := range envs {
		out = append(out, k+"="+v)
	}
	return out, nil
}

// setVersionAddr 更新 skill_versions.config 快照的 addr 字段（网关按版本 addr 路由）。
func (m *VersionManager) setVersionAddr(ctx context.Context, ver *db.SkillVersion, addr string) error {
	var cfg map[string]any
	if ver.Config != "" && ver.Config != "null" {
		if err := json.Unmarshal([]byte(ver.Config), &cfg); err != nil {
			cfg = map[string]any{}
		}
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["addr"] = addr
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化 version config: %w", err)
	}
	ver.Config = string(b)
	return nil
}

func (m *VersionManager) saveVersion(ctx context.Context, ver *db.SkillVersion) error {
	if err := m.database.WithContext(ctx).Save(ver).Error; err != nil {
		return fmt.Errorf("更新 skill_versions: %w", err)
	}
	return nil
}

// imageExists 镜像本地是否存在。
func (m *VersionManager) imageExists(ctx context.Context, image string) (bool, error) {
	images, err := m.docker.ListImages(ctx, map[string][]string{"reference": {image}})
	if err != nil {
		return false, fmt.Errorf("列镜像 %s: %w", image, err)
	}
	return len(images) > 0, nil
}

// waitRunning 轮询容器直至 running。
func (m *VersionManager) waitRunning(ctx context.Context, name string, wait time.Duration) (bool, string) {
	deadline := time.Now().Add(wait)
	var last string
	for {
		info, err := m.docker.InspectContainer(ctx, name)
		if err != nil {
			last = err.Error()
		} else if info.State != nil {
			last = info.State.Status
			if info.State.Running {
				return true, "running"
			}
			if info.State.Status == "exited" {
				return false, fmt.Sprintf("exited code %d", info.State.ExitCode)
			}
		}
		if time.Now().After(deadline) {
			return false, last
		}
		select {
		case <-ctx.Done():
			return false, "ctx done"
		case <-time.After(2 * time.Second):
		}
	}
}

// parseMemLimit 解析 compose mem_limit（"512m" / "1g" / 裸字节）为字节。
func parseMemLimit(s string) int64 {
	if s == "" {
		return 0
	}
	s = strings.ToLower(strings.TrimSpace(s))
	var mult int64 = 1
	switch {
	case strings.HasSuffix(s, "gb"):
		mult, s = 1<<30, strings.TrimSuffix(s, "gb")
	case strings.HasSuffix(s, "mb"):
		mult, s = 1<<20, strings.TrimSuffix(s, "mb")
	case strings.HasSuffix(s, "kb"):
		mult, s = 1<<10, strings.TrimSuffix(s, "kb")
	case strings.HasSuffix(s, "g"):
		mult, s = 1<<30, strings.TrimSuffix(s, "g")
	case strings.HasSuffix(s, "m"):
		mult, s = 1<<20, strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "k"):
		mult, s = 1<<10, strings.TrimSuffix(s, "k")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}

// parseCPUs 解析 compose cpus（"0.5"）为 nano CPUs。
func parseCPUs(s string) int64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return int64(f * 1e9)
}
