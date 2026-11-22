package dockerapi

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// run.go：一次性容器（create/start/日志/清理）。自 client.go 拆出（批次 F，纯机械搬迁）。

// ContainerSpec 描述一次性容器：create/start 后收集日志并自动清理。
type ContainerSpec struct {
	Name        string
	Image       string
	Cmd         []string
	Env         []string
	Workdir     string
	User        string // 运行用户（"uid:gid"）；空 = 镜像默认（通常 root）
	Mounts      []Mount
	NetworkMode string
	Memory      int64 // 字节
	CPUs        float64
	PidsLimit   int
	ReadOnly    bool
	Tmpfs       []string
	CapDrop     []string
	SecurityOpt []string
	Timeout     time.Duration
}

// Mount 是一次性容器挂载。
//
//	Volume=false（默认）：Source 为**宿主绝对路径**的 bind 挂载；
//	Volume=true：Source 为**命名卷**（如 workflow_ci-cache）。
type Mount struct {
	Source      string
	Destination string
	ReadOnly    bool
	Volume      bool // 命名卷（Type=volume）；否则 bind（Type=bind）
}

// RunContainer 起一个一次性容器跑命令，等待退出后删除容器，返回 stdout/stderr/exit code。
func (c *Client) RunContainer(ctx context.Context, spec ContainerSpec) (*ExecResult, error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	hostCfg := map[string]any{
		"NetworkMode": spec.NetworkMode,
		"Memory":      spec.Memory,
		"NanoCpus":    int64(spec.CPUs * 1e9),
		"PidsLimit":   spec.PidsLimit,
		"ReadOnly":    spec.ReadOnly,
		"CapDrop":     spec.CapDrop,
		"SecurityOpt": spec.SecurityOpt,
	}
	// 注意：PidsLimit 为 0 时不设置（docker 语义：null=不限）
	if spec.PidsLimit == 0 {
		hostCfg["PidsLimit"] = nil
	}
	if spec.NetworkMode == "" {
		hostCfg["NetworkMode"] = "default"
	}
	if len(spec.Tmpfs) > 0 {
		// 本宿主 docker 版本 Tmpfs 为 map[string]string（旧 API 形态；空串 = 无选项）
		tmpfs := map[string]string{}
		for _, t := range spec.Tmpfs {
			tmpfs[t] = ""
		}
		hostCfg["Tmpfs"] = tmpfs
	}
	var mounts []map[string]any
	for _, m := range spec.Mounts {
		mtype := "bind"
		if m.Volume {
			mtype = "volume"
		}
		// ⚠️ 只读必须用 API 的 **ReadOnly** 字段：早先发的是 "RW": !ReadOnly + "Mode"，
		// 两者都不是 docker Mount 结构的字段（会被 daemon 忽略）→ 请求的只读挂载实际全是 rw。
		// 2026-10-19 实测：沙箱 /work 在 /proc/self/mountinfo 里显示 rw（只靠 --cap-drop ALL 侥幸挡住写入）、
		// CI 容器（无 cap-drop）以 root 往 worktree 写了一堆 root 属主缓存。
		mounts = append(mounts, map[string]any{
			"Source":   m.Source,
			"Target":   m.Destination,
			"Type":     mtype,
			"ReadOnly": m.ReadOnly,
		})
	}
	if len(mounts) > 0 {
		hostCfg["Mounts"] = mounts
	}

	cfg := map[string]any{
		"Image": spec.Image,
		"Cmd":   spec.Cmd,
	}
	// 清除镜像自带 entrypoint（等价 docker run image cmd）：必须传**空数组**；
	// null 会被 engine 当作“沿用镜像默认 entrypoint”（2026-09-30 实测：ci-runner 有 entrypoint
	// → 脚本被当成 $1（TASK_NO 变脚本路径）、JUnit 文件名错位、摘要 glob 匹配不到）。
	cfg["Entrypoint"] = []string{}
	if spec.Env != nil {
		cfg["Env"] = spec.Env
	}
	if spec.Workdir != "" {
		cfg["WorkingDir"] = spec.Workdir
	}
	// 运行用户：非 root 运行可避免在 bind 挂载（worktree）里留下 root 属主文件
	// （root 写出的文件，uid 1001 的 driver 后续 resetWorktree / git clean 会失败）
	if spec.User != "" {
		cfg["User"] = spec.User
	}
	// 容器配置字段在 create 请求的顶层（HostConfig 嵌套），不能包在 "ContainerConfig" 下
	createBody := map[string]any{"HostConfig": hostCfg}
	for k, v := range cfg {
		createBody[k] = v
	}

	// 1. 创建
	var createResp struct {
		ID       string `json:"Id"`
		Warnings []string
		ErrMsg   string `json:"ErrorMsg"`
	}
	if err := c.do(ctx, "POST", "/containers/create?name="+spec.Name, createBody, &createResp); err != nil {
		return nil, err
	}
	if createResp.ErrMsg != "" {
		return nil, fmt.Errorf("create container: %s", createResp.ErrMsg)
	}
	containerID := createResp.ID

	// 2. 启动
	if err := c.do(ctx, "POST", "/containers/"+containerID+"/start", nil, nil); err != nil {
		c.removeContainer(ctx, containerID)
		return nil, err
	}

	// 3. 等待退出
	var waitResp struct {
		ExitCode int `json:"StatusCode"`
	}
	// 默认 condition（not-running）即等待容器退出；API 无 "exit" 取值
	if err := c.do(ctx, "POST", "/containers/"+containerID+"/wait", nil, &waitResp); err != nil {
		c.removeContainer(ctx, containerID)
		return nil, err
	}

	// 4. 日志
	stdout, stderr, err := c.containerLogs(ctx, containerID)
	c.removeContainer(ctx, containerID)
	if err != nil {
		return nil, err
	}
	return &ExecResult{ExitCode: waitResp.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

func (c *Client) containerLogs(ctx context.Context, id string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/containers/"+id+"/logs?stdout=1&stderr=1&stream=1", nil)
	if err != nil {
		return "", "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", fmt.Errorf("container logs: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return demux(resp.Body)
}

func (c *Client) removeContainer(ctx context.Context, id string) {
	rmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = c.do(rmCtx, "DELETE", "/containers/"+id+"?force=1&v=1", nil, nil)
}

// demux 解复用 Docker 的多路复用日志流（8 字节头：1 流号 + 3 保留 + 4 大端长度）。
func demux(r io.Reader) (stdout, stderr string, err error) {
	var out, errb bytes.Buffer
	hdr := make([]byte, 8)
	for {
		n, err := io.ReadFull(r, hdr)
		if n == 0 {
			if err == io.EOF {
				break
			}
			if err != nil && err != io.ErrUnexpectedEOF {
				break
			}
			// 尾部不足 8 字节，按非复用纯文本处理
			out.WriteString(string(hdr[:n]))
			break
		}
		if err != nil {
			return out.String(), errb.String(), nil
		}
		stream := hdr[0]
		size := int(uint32(hdr[4])<<24 | uint32(hdr[5])<<16 | uint32(hdr[6])<<8 | uint32(hdr[7]))
		chunk := make([]byte, size)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return out.String(), errb.String(), nil
		}
		switch stream {
		case 1:
			out.Write(chunk)
		case 2:
			errb.Write(chunk)
		}
	}
	return out.String(), errb.String(), nil
}
