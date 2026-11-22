package dockerapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// exec.go：docker exec（含 hijack 流式 stdin/stdout）。自 client.go 拆出（批次 F，纯机械搬迁）。

// ExecRequest 描述一次 docker exec 调用（容器内执行命令，hijack 流式收发）。
type ExecRequest struct {
	Container string
	Cmd       []string
	Env       []string
	Workdir   string
	Stdin     []byte
	Timeout   time.Duration // 0 表示不设超时
}

// ExecResult 是 exec 结果（stdout/stderr 截断见调用方）。
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Exec 在容器内执行命令（等价 docker exec -i）。
func (c *Client) Exec(ctx context.Context, r ExecRequest) (*ExecResult, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}

	// 1. 创建 exec 实例
	var createResp struct {
		ID string `json:"Id"`
	}
	createBody := map[string]any{
		"AttachStdin":  true,
		"AttachStdout": true,
		"AttachStderr": true,
		"Cmd":          r.Cmd,
	}
	if r.Env != nil {
		createBody["Env"] = r.Env
	}
	if r.Workdir != "" {
		createBody["WorkingDir"] = r.Workdir
	}
	if err := c.do(ctx, "POST", "/containers/"+r.Container+"/exec", createBody, &createResp); err != nil {
		return nil, err
	}
	execID := createResp.ID

	// 2. 启动（docker 28 / API 1.51 的 stdin 协议：
	//    start 请求是普通 JSON body + Upgrade 头 → engine 回 101；
	//    之后在 hijacked 连接上写**原始** stdin（无 8 字节头，engine 自行包多路复用头），
	//    写完 EOF 后 engine 在进程退出时关闭流。无 stdin 时走普通 JSON 请求。）
	var stdout, stderr string
	if len(r.Stdin) > 0 {
		var e error
		stdout, stderr, e = c.execStartHijack(ctx, execID, r.Stdin)
		if e != nil {
			return nil, e
		}
	} else {
		startReq, err := http.NewRequestWithContext(ctx, "POST", c.base+"/exec/"+execID+"/start", bytes.NewReader([]byte(`{"Tty":false}`)))
		if err != nil {
			return nil, err
		}
		startReq.Header.Set("Content-Type", "application/json")
		startResp, err := c.http.Do(startReq)
		if err != nil {
			return nil, err
		}
		sb, _ := io.ReadAll(io.LimitReader(startResp.Body, 4096))
		startResp.Body.Close()
		if startResp.StatusCode >= 300 {
			return nil, fmt.Errorf("exec start: HTTP %d: %s", startResp.StatusCode, strings.TrimSpace(string(sb)))
		}
	}

	// 3. 等待退出（hijack 路径已阻塞至进程退出；此处取 exit code）
	var waitResp struct {
		ExitCode int `json:"StatusCode"`
	}
	if err := c.do(ctx, "POST", "/exec/"+execID+"/start/wait", nil, &waitResp); err != nil {
		// 某些版本无 /start/wait，回退 /json 轮询
		if err2 := c.waitForExec(ctx, execID, &waitResp); err2 != nil {
			return nil, fmt.Errorf("wait exec: %v (fallback: %v)", err, err2)
		}
	}

	// 4. 无 stdin 时输出从 /exec/{id}/logs 收集（多路复用流，需解复用；须在退出后取）
	if len(r.Stdin) == 0 {
		var e error
		stdout, stderr, e = c.execLogs(ctx, execID)
		if e != nil {
			return nil, e
		}
	}
	return &ExecResult{ExitCode: waitResp.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

// execStartHijack 按 docker 28 的 hijack 协议启动带 stdin 的 exec：
// 原始 socket 写请求（JSON body + Connection/Upgrade: tcp）→ 读 101 →
// 写原始 stdin → CloseWrite（EOF）→ 解复用输出流至进程退出。
// 注：docker 28 无 /exec/{id}/logs 路由（404），输出只能从 hijacked 流收集。
func (c *Client) execStartHijack(ctx context.Context, execID string, stdin []byte) (string, string, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	// hijack 连接建立后，ctx 只能靠显式手段作用到读写上，否则 agent 卡死时
	// io.ReadFull 会永久阻塞、ExecRequest.Timeout 形同虚设（2026-09-30 实测 106 分钟未返回）：
	//  1) 有 deadline 时设到连接上（读写超时即返回）；
	//  2) 监听 ctx.Done() 主动关闭连接（覆盖 cancel 与超时）。
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	body := []byte(`{"Detach":false,"Tty":false}`)
	req := "POST /exec/" + execID + "/start HTTP/1.1\r\n" +
		"Host: api.moby.localhost\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Content-Type: application/json\r\n" +
		"Upgrade: tcp\r\n\r\n"
	if _, err := conn.Write(append([]byte(req), body...)); err != nil {
		return "", "", err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return "", "", err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(br, 4096))
		return "", "", fmt.Errorf("exec start: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if len(stdin) > 0 {
		if _, err := conn.Write(stdin); err != nil {
			return "", "", err
		}
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite() // EOF：进程读完 stdin
	}
	// 解复用输出流（8 字节头：stream + 4 字节 BE 长度）；engine 在进程退出时关闭其侧 → EOF
	var out, errb bytes.Buffer
	for {
		hdr := make([]byte, 8)
		if _, err := io.ReadFull(br, hdr); err != nil {
			if e := ctx.Err(); e != nil {
				return out.String(), errb.String(), fmt.Errorf("exec %s: %w (agent 未在超时内退出)", execID, e)
			}
			break // EOF（进程退出）
		}
		n := int(binary.BigEndian.Uint32(hdr[4:]))
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			if e := ctx.Err(); e != nil {
				return out.String(), errb.String(), fmt.Errorf("exec %s: %w (agent 未在超时内退出)", execID, e)
			}
			break
		}
		switch hdr[0] {
		case 1:
			out.Write(buf)
		case 2:
			errb.Write(buf)
		}
	}
	return out.String(), errb.String(), nil
}

func (c *Client) waitForExec(ctx context.Context, execID string, out *struct {
	ExitCode int `json:"StatusCode"`
}) error {
	for {
		var j struct {
			Running  bool `json:"Running"`
			ExitCode int  `json:"ExitCode"`
		}
		if err := c.do(ctx, "GET", "/exec/"+execID+"/json", nil, &j); err != nil {
			return err
		}
		if !j.Running {
			out.ExitCode = j.ExitCode
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// execLogs 读取 exec 的合并日志并解复用（stdout=1 / stderr=2）。
func (c *Client) execLogs(ctx context.Context, execID string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/exec/"+execID+"/logs?stdout=1&stderr=1&stream=1", nil)
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
		return "", "", fmt.Errorf("exec logs: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return demux(resp.Body)
}
