package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

// Executor 是 docker compose CLI 执行器（deployctl / health-patrol 共用）。
//
// deployctl 与 health-patrol 容器基于 deploy/Dockerfile.deploy（docker CLI + compose
// 插件 + shell），经挂载的 Docker socket 驱动宿主 daemon；compose 文件是单一事实
// 来源，本执行器只发命令、不改任何配置（S5 回滚红线）。
type Executor struct {
	composeDir string        // deploy/ 绝对路径
	root       string        // 仓库根（docker-compose.yml 所在项目）
	healthTO   time.Duration // 单次健康探测超时（DEPLOY_HEALTH_TIMEOUT 秒）
}

// NewExecutor 创建执行器。composeDir = deploy/ 目录绝对路径。
func NewExecutor(root, composeDir string, healthTimeoutSec int) *Executor {
	if healthTimeoutSec <= 0 {
		healthTimeoutSec = 30
	}
	return &Executor{
		root:       root,
		composeDir: composeDir,
		healthTO:   time.Duration(healthTimeoutSec) * time.Second,
	}
}

// compose 执行 docker compose（-f 根 compose 文件，cwd = deploy/ 使 env_file 相对路径正确）。
func (e *Executor) compose(ctx context.Context, args ...string) (string, error) {
	full := append([]string{"compose", "-f", filepath.Join(e.composeDir, "docker-compose.yml")}, args...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = e.composeDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("docker %s: %v: %s",
			strings.Join(full, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// BuildAll 构建全部带 build 的服务（compose v2 内部并行构建，已缓存的层秒过）。
// 失败重试 1 次（S5 §3.2：每步失败重试 1 次）。
func (e *Executor) BuildAll(ctx context.Context) error {
	svcs := []string{}
	for _, s := range sortedServices(e) {
		if s.Build != nil {
			svcs = append(svcs, s.Name)
		}
	}
	if len(svcs) == 0 {
		return nil
	}
	log.Printf("build: %d services: %s", len(svcs), strings.Join(svcs, " "))
	args := append([]string{"build"}, svcs...)
	if out, err := e.compose(ctx, args...); err != nil {
		log.Printf("build 失败，重试一次: %v", err)
		if out2, err2 := e.compose(ctx, args...); err2 != nil {
			return fmt.Errorf("build 重试后仍失败: %v: %s", err2, strings.TrimSpace(out2))
		}
		_ = out
	}
	return nil
}

// BuildOne 构建单个服务（deploy <target> 用）。
func (e *Executor) BuildOne(ctx context.Context, name string) error {
	out, err := e.compose(ctx, "build", name)
	if err != nil {
		log.Printf("build %s 失败，重试一次: %v", name, err)
		out2, err2 := e.compose(ctx, "build", name)
		if err2 != nil {
			return fmt.Errorf("build %s 重试后仍失败: %v: %s", name, err2, strings.TrimSpace(out2))
		}
		_ = out
	}
	return nil
}

// UpAll 启动全部服务（docker compose up -d；compose 按 depends_on 序启动）。
func (e *Executor) UpAll(ctx context.Context) error {
	out, err := e.compose(ctx, "up", "-d")
	if err != nil {
		return fmt.Errorf("up -d: %v: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// UpOne 启动单个服务；withBuild=true 时先重建镜像（改代码后必须重建才生效）。
func (e *Executor) UpOne(ctx context.Context, name string, withBuild bool) error {
	args := []string{"up", "-d"}
	if withBuild {
		args = append(args, "--build")
	}
	args = append(args, name)
	out, err := e.compose(ctx, args...)
	if err != nil {
		return fmt.Errorf("up -d %s: %v: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// RestartOne 重启单个服务（docker compose restart；不重建镜像）。
func (e *Executor) RestartOne(ctx context.Context, name string) error {
	out, err := e.compose(ctx, "restart", name)
	if err != nil {
		return fmt.Errorf("restart %s: %v: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// StopOne 停止单个服务（docker compose stop；镜像与数据保留，可再启动）。
func (e *Executor) StopOne(ctx context.Context, name string) error {
	out, err := e.compose(ctx, "stop", name)
	if err != nil {
		return fmt.Errorf("stop %s: %v: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// RollbackImage 镜像回滚：临时 override 文件（/tmp，不落仓库、不改生产配置）把
// 该服务 image 换成旧 tag，再 up -d（不 --build：旧 tag 镜像直接启用）。
func (e *Executor) RollbackImage(ctx context.Context, name, oldImage string) error {
	override := fmt.Sprintf("services:\n  %s:\n    image: %s\n", name, oldImage)
	path := filepath.Join(e.composeDir, "deployctl-override-"+name+".yml")
	if err := writeFile(path, []byte(override)); err != nil {
		return fmt.Errorf("写 override 文件: %w", err)
	}
	defer os.Remove(path)
	out, err := e.compose(ctx, "-f", path, "up", "-d", name)
	if err != nil {
		return fmt.Errorf("rollback %s → %s: %v: %s", name, oldImage, err, strings.TrimSpace(out))
	}
	return nil
}

// PS 返回全部容器的状态（docker compose ps --format json）。
// key 同时按服务名与容器名索引（部分服务容器名 ≠ 服务名，如 mysql → workflow-mysql）。
func (e *Executor) PS(ctx context.Context) (map[string]*ContainerState, error) {
	out, err := e.compose(ctx, "ps", "--format", "json")
	if err != nil {
		return nil, err
	}
	rows, err := parsePSOut(out)
	if err != nil {
		return nil, fmt.Errorf("parse compose ps json: %w", err)
	}
	m := map[string]*ContainerState{}
	for _, r := range rows {
		st := &ContainerState{
			ID:       r.ID,
			Name:     r.Name,
			Service:  r.Service,
			State:    r.State,
			Status:   r.Status,
			Health:   r.Health,
			ExitCode: r.ExitCode,
		}
		if st.Service != "" {
			m[st.Service] = st
		}
		if st.Name != "" && st.Name != st.Service {
			if _, ok := m[st.Name]; !ok {
				m[st.Name] = st
			}
		}
	}
	return m, nil
}

// parsePSOut 解析 compose ps --format json 输出，兼容两种格式：
// JSON 数组（[{...}, ...]）与 NDJSON（每行一个 {...}，compose v2.38+ 默认）。
func parsePSOut(out string) ([]psRow, error) {
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var rows []psRow
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	rows := []psRow{}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	for {
		var r psRow
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, nil
}

type psRow struct {
	ID       string `json:"ID"`
	Name     string `json:"Name"`
	Service  string `json:"Service"`
	State    string `json:"State"`
	Status   string `json:"Status"`
	Health   string `json:"Health"`
	ExitCode int    `json:"ExitCode"`
}

// ContainerState 是 compose ps 的一条容器状态。
type ContainerState struct {
	ID       string
	Name     string
	Service  string
	State    string // running | exited | paused | restarting | dead
	Status   string
	Health   string // healthy | unhealthy | starting | ""（无 healthcheck）
	ExitCode int
}

// sortedServices 返回按 TopoOrder 排序的服务（确定性输出）。
func sortedServices(e *Executor) []*Service {
	p, err := LoadProject(e.root)
	if err != nil {
		log.Printf("WARN: load compose project: %v", err)
		return nil
	}
	order, err := p.TopoOrder()
	if err != nil {
		log.Printf("WARN: topo order: %v", err)
		return nil
	}
	out := make([]*Service, 0, len(order))
	for _, name := range order {
		out = append(out, p.Services[name])
	}
	return out
}
