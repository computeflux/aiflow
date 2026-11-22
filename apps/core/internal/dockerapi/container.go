// 容器生命周期 / 网络 / 镜像 API（S5 版本容器动态管理，docs/plan/S5-软件接管与部署编排.md §3.4）。
//
// 版本容器是静态 compose 的唯一例外：deployctl version up/down 经本文件方法
// 动态创建/销毁（k8s 映射：controller 按版本创建 Deployment+Service，接口不变，
// docs/plan/evolve-plan.md §3.5）。
package dockerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ContainerInfo 是容器 inspect 结果（字段按需，只取版本容器管理需要的部分）。
type ContainerInfo struct {
	ID              string              `json:"Id"`
	Name            string              `json:"Name"`
	State           *ContainerStateInfo `json:"State"`
	NetworkSettings *NetworkSettings    `json:"NetworkSettings"`
}

// ContainerStateInfo 是容器状态。
type ContainerStateInfo struct {
	Status     string      `json:"Status"` // created | running | paused | restarting | removing | exited | dead
	Running    bool        `json:"Running"`
	Restarting bool        `json:"Restarting"`
	ExitCode   int         `json:"ExitCode"`
	StartedAt  string      `json:"StartedAt"`
	FinishedAt string      `json:"FinishedAt"`
	Health     *HealthInfo `json:"Health"`
}

// HealthInfo 是 docker 健康检查状态。
type HealthInfo struct {
	Status        string `json:"Status"` // healthy | unhealthy | starting
	FailingStreak int    `json:"FailingStreak"`
	LastExitCode  int    `json:"LastExitCode"`
}

// NetworkSettings 是容器网络设置。
type NetworkSettings struct {
	Networks map[string]any `json:"Networks"`
}

// ContainerBrief 是容器列表条目（GET /containers/json）。
// Docker Engine API 返回 Names []string（形如 ["/workflow-api"]），无单数 Name 字段。
type ContainerBrief struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Status string            `json:"Status"`
	Labels map[string]string `json:"Labels"`
}

// ContainerName 返回容器名（去前导 /）。
func (c *ContainerBrief) ContainerName() string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// ImageBrief 是镜像列表条目（GET /images/json）。
type ImageBrief struct {
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
}

// InspectContainer 取容器详情（按名字）。
func (c *Client) InspectContainer(ctx context.Context, name string) (*ContainerInfo, error) {
	var info ContainerInfo
	if err := c.do(ctx, "GET", "/containers/"+name+"/json", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// StartContainer 启动容器。
func (c *Client) StartContainer(ctx context.Context, name string) error {
	return c.do(ctx, "POST", "/containers/"+name+"/start", nil, nil)
}

// StopContainer 停止容器（timeoutSec ≤ 0 用 docker 默认 10s）。
func (c *Client) StopContainer(ctx context.Context, name string, timeoutSec int) error {
	q := "/containers/" + name + "/stop"
	if timeoutSec > 0 {
		q += fmt.Sprintf("?t=%d", timeoutSec)
	}
	return c.do(ctx, "POST", q, nil, nil)
}

// RemoveContainer 删除容器（force=1 可删运行中容器），保留匿名卷。
// 重建/回滚路径用：避免有状态服务丢数据。
func (c *Client) RemoveContainer(ctx context.Context, name string, force bool) error {
	return c.removeContainerEx(ctx, name, force, false)
}

// RemoveContainerWithVolumes 删除容器并删除其匿名卷（v=1）。
// 版本实例生命周期 / 临时容器清理用：这些容器不承载有效数据，删卷避免累积泄漏。
func (c *Client) RemoveContainerWithVolumes(ctx context.Context, name string, force bool) error {
	return c.removeContainerEx(ctx, name, force, true)
}

func (c *Client) removeContainerEx(ctx context.Context, name string, force, removeVolumes bool) error {
	q := "/containers/" + name
	params := []string{}
	if force {
		params = append(params, "force=1")
	}
	if removeVolumes {
		params = append(params, "v=1")
	}
	if len(params) > 0 {
		q += "?" + strings.Join(params, "&")
	}
	return c.do(ctx, "DELETE", q, nil, nil)
}

// RenameContainer 重命名容器（Engine API: POST /containers/{id}/rename?name=<new>）。
func (c *Client) RenameContainer(ctx context.Context, name, newName string) error {
	return c.do(ctx, "POST", "/containers/"+name+"/rename?name="+newName, nil, nil)
}

// ContainerExists 容器是否存在。
func (c *Client) ContainerExists(ctx context.Context, name string) (bool, error) {
	_, err := c.InspectContainer(ctx, name)
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

// ListContainers 列出容器（all=true 含已停止；filters 形如 {"label": ["part-of=compute-flux-workflow"]}）。
func (c *Client) ListContainers(ctx context.Context, all bool, filters map[string][]string) ([]ContainerBrief, error) {
	q := "/containers/json?all=" + boolStr(all)
	if f, err := filtersQuery(filters); err == nil && f != "" {
		q += "&filters=" + f
	}
	var out []ContainerBrief
	if err := c.do(ctx, "GET", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ListImages 列出镜像（filters 形如 {"reference": ["pyreplay:1.4.1"]}）。
func (c *Client) ListImages(ctx context.Context, filters map[string][]string) ([]ImageBrief, error) {
	q := "/images/json"
	if f, err := filtersQuery(filters); err == nil && f != "" {
		q += "?filters=" + f
	}
	var out []ImageBrief
	if err := c.do(ctx, "GET", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ImageExists 检查镜像是否本地存在（按 reference 过滤，如 "pyreplay:1.4.1"）。
func (c *Client) ImageExists(ctx context.Context, reference string) (bool, error) {
	images, err := c.ListImages(ctx, map[string][]string{"reference": {reference}})
	if err != nil {
		return false, err
	}
	return len(images) > 0, nil
}

// filtersQuery 把 filters map 编码为 Engine API 的 query 参数（JSON + URL quote）。
func filtersQuery(filters map[string][]string) (string, error) {
	if len(filters) == 0 {
		return "", nil
	}
	b, err := json.Marshal(filters)
	if err != nil {
		return "", err
	}
	return url.QueryEscape(string(b)), nil
}

// CreateNetwork 创建网络（幂等：已存在返回 nil）。
// internal=true 时网络不可访问外部（测试网络隔离，S5 §3.4）。
func (c *Client) CreateNetwork(ctx context.Context, name, driver string, internal bool) error {
	if driver == "" {
		driver = "bridge"
	}
	body := map[string]any{"Name": name, "Driver": driver, "Internal": internal}
	if err := c.do(ctx, "POST", "/networks/create", body, nil); err != nil {
		if isConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

// RemoveNetwork 删除网络（不存在返回 nil）。
func (c *Client) RemoveNetwork(ctx context.Context, name string) error {
	if err := c.do(ctx, "DELETE", "/networks/"+name, nil, nil); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

// NetworkExists 网络是否存在。
func (c *Client) NetworkExists(ctx context.Context, name string) (bool, error) {
	var out []struct {
		Name string `json:"Name"`
	}
	if err := c.do(ctx, "GET", "/networks", nil, &out); err != nil {
		return false, err
	}
	for _, n := range out {
		if n.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// ConnectNetwork 把容器接入命名网络（版本容器多网络：生产网 + 测试网）。
func (c *Client) ConnectNetwork(ctx context.Context, network, container string) error {
	body := map[string]any{"Container": container}
	if err := c.do(ctx, "POST", "/networks/"+network+"/connect", body, nil); err != nil {
		if isConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

// ContainerCreateSpec 是常驻容器创建参数（版本容器；安全参数由调用方写死）。
type ContainerCreateSpec struct {
	Name        string
	Image       string
	Env         []string
	Cmd         []string
	Entrypoint  []string // nil = 沿用镜像默认；空切片 = 清除
	WorkingDir  string
	Labels      map[string]string
	Networks    []string // 命名网络（第一个 = 主网络）
	Memory      int64    // 字节（与生产一致，S5 §3.4）
	NanoCPUs    int64
	PidsLimit   int
	Tmpfs       []string
	CapDrop     []string
	SecurityOpt []string
}

// CreateContainer 创建常驻容器（不启动），返回容器 ID。
func (c *Client) CreateContainer(ctx context.Context, spec ContainerCreateSpec) (string, error) {
	hostCfg := map[string]any{
		"Memory":        spec.Memory,
		"NanoCpus":      spec.NanoCPUs,
		"PidsLimit":     spec.PidsLimit,
		"CapDrop":       spec.CapDrop,
		"SecurityOpt":   spec.SecurityOpt,
		"RestartPolicy": map[string]any{"Name": "no"},
	}
	if spec.PidsLimit == 0 {
		hostCfg["PidsLimit"] = nil
	}
	if len(spec.Networks) > 0 {
		hostCfg["NetworkMode"] = spec.Networks[0]
	} else {
		hostCfg["NetworkMode"] = "default"
	}
	if len(spec.Tmpfs) > 0 {
		tmpfs := map[string]string{}
		for _, t := range spec.Tmpfs {
			tmpfs[t] = ""
		}
		hostCfg["Tmpfs"] = tmpfs
	}

	cfg := map[string]any{
		"Image": spec.Image,
		"Env":   spec.Env,
		"Cmd":   spec.Cmd,
	}
	if spec.Entrypoint != nil {
		cfg["Entrypoint"] = spec.Entrypoint
	}
	if spec.WorkingDir != "" {
		cfg["WorkingDir"] = spec.WorkingDir
	}
	if len(spec.Labels) > 0 {
		cfg["Labels"] = spec.Labels
	}
	createBody := map[string]any{"HostConfig": hostCfg}
	for k, v := range cfg {
		createBody[k] = v
	}

	var createResp struct {
		ID       string `json:"Id"`
		ErrMsg   string `json:"ErrorMsg"`
		Warnings []string
	}
	if err := c.do(ctx, "POST", "/containers/create?name="+spec.Name, createBody, &createResp); err != nil {
		return "", err
	}
	if createResp.ErrMsg != "" {
		return "", fmt.Errorf("create container %s: %s", spec.Name, createResp.ErrMsg)
	}

	// 接入其余命名网络
	for _, net := range spec.Networks[1:] {
		if err := c.ConnectNetwork(ctx, net, spec.Name); err != nil {
			c.RemoveContainerWithVolumes(ctx, createResp.ID, true) // 临时容器清理：删匿名卷
			return "", fmt.Errorf("connect %s to %s: %w", spec.Name, net, err)
		}
	}
	return createResp.ID, nil
}

// RestartContainer 重启容器。
func (c *Client) RestartContainer(ctx context.Context, name string, timeoutSec int) error {
	q := "/containers/" + name + "/restart"
	if timeoutSec > 0 {
		q += fmt.Sprintf("?t=%d", timeoutSec)
	}
	return c.do(ctx, "POST", q, nil, nil)
}

// ContainerLogs 取容器日志（tail 行；0 = 全部）。返回合并的 stdout+stderr 文本。
// tty=true 时 Docker 返回未多路复用的原始流（无 8 字节头），直接读取；
// 否则按多路复用流解复用（调用方从 inspect 的 Config.Tty 传入）。
func (c *Client) ContainerLogs(ctx context.Context, name string, tail int, tty bool) (string, error) {
	q := fmt.Sprintf("/containers/%s/logs?stdout=1&stderr=1&stream=0&tail=%d", name, tail)
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+q, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("container logs: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if tty {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return string(b), err
	}
	stdout, stderr, _ := demux(io.LimitReader(resp.Body, 4<<20)) // 限长兜底：单条日志行可能极大（内嵌 base64/长 JSON），防 OOM
	return stdout + stderr, nil
}

// ContainerFull 是容器 inspect 的完整结果（重建容器时保留原配置用）。
// Config / HostConfig 保留原始 JSON（map），重建时原样回传 + 局部覆盖。
type ContainerFull struct {
	ID              string              `json:"Id"`
	Name            string              `json:"Name"`
	State           *ContainerStateInfo `json:"State"`
	Config          map[string]any      `json:"Config"`
	HostConfig      map[string]any      `json:"HostConfig"`
	NetworkSettings *NetworkSettings    `json:"NetworkSettings"`
}

// InspectContainerFull 取容器完整 inspect 结果（含 Config / HostConfig 原始 JSON）。
func (c *Client) InspectContainerFull(ctx context.Context, name string) (*ContainerFull, error) {
	var info ContainerFull
	if err := c.do(ctx, "GET", "/containers/"+name+"/json", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// PortBinding 是容器端口绑定（HostConfig.PortBindings 的条目）。
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// CreateContainerRaw 以原始 create body 创建容器（重建容器用：
// 从 inspect 取原 Config/HostConfig 局部覆盖后回传）。返回容器 ID。
func (c *Client) CreateContainerRaw(ctx context.Context, name string, body map[string]any) (string, error) {
	var resp struct {
		ID     string `json:"Id"`
		ErrMsg string `json:"ErrorMsg"`
	}
	if err := c.do(ctx, "POST", "/containers/create?name="+name, body, &resp); err != nil {
		return "", err
	}
	if resp.ErrMsg != "" {
		return "", fmt.Errorf("create container %s: %s", name, resp.ErrMsg)
	}
	return resp.ID, nil
}

// isNotFound / isConflict 判定 Engine API 状态码（404 / 409；
// 错误文本格式见 client.go do() 的 "docker <method> <path>: HTTP <code>: ..."）。
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

func isConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 409")
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
