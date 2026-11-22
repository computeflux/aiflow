// Package dockerapi 是极简 Docker Engine API 客户端（stdlib only，无第三方依赖）。
//
// 只覆盖 S1 进化链需要的两个场景：
//   - Exec：向容器注入 stdin 执行命令并收集 stdout/stderr/exit code（evolve-driver
//     调 agent adapter：docker exec pi-web / ocr ...）；
//   - RunContainer：起一个一次性容器跑命令（sandbox-runner 跑沙箱测试）。
//
// 安全参数（网络/内存/只读等）由调用方在 ContainerConfig 里写死，客户端不做任何
// 覆盖；调用方不可传任意 docker CLI 参数。
package dockerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Client 通过 unix socket 或 tcp 连接 Docker Engine。
type Client struct {
	base string
	http *http.Client
	dial func(ctx context.Context) (net.Conn, error)
}

// New 创建客户端。host 形如 "unix:///var/run/docker.sock" 或 "tcp://sandbox-dind:2375"。
func New(host string) (*Client, error) {
	base, tr, dial, err := dialerFor(host)
	if err != nil {
		return nil, err
	}
	return &Client{
		base: base,
		http: &http.Client{Transport: tr, Timeout: 0},
		dial: dial,
	}, nil
}

func dialerFor(host string) (string, *http.Transport, func(context.Context) (net.Conn, error), error) {
	switch {
	case strings.HasPrefix(host, "tcp://"):
		addr := strings.TrimPrefix(host, "tcp://")
		var d net.Dialer
		return "http://" + addr, &http.Transport{},
			func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }, nil
	case strings.HasPrefix(host, "unix://") || strings.HasPrefix(host, "/"):
		sock := strings.TrimPrefix(host, "unix://")
		var d net.Dialer
		dial := func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, "unix", sock) }
		return "http://docker", &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}, dial, nil
	default:
		return "", nil, nil, fmt.Errorf("unsupported docker host scheme: %q", host)
	}
}

// do 发起请求；body 非 nil 时 JSON 编码；resp 非 nil 时 JSON 解码。
func (c *Client) do(ctx context.Context, method, path string, body, resp any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpReq := req
	respOut, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer respOut.Body.Close()
	if respOut.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(respOut.Body, 4096))
		return fmt.Errorf("docker %s %s: HTTP %d: %s", method, path, respOut.StatusCode, strings.TrimSpace(string(b)))
	}
	if resp != nil {
		return json.NewDecoder(respOut.Body).Decode(resp)
	}
	return nil
}

// ExecRequest 是 exec 参数。

// ContainerSpec 是一次性容器参数（安全参数由调用方写死）。
