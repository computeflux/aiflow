package dockerapi

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExecStdinHijackTimeout 回归（2026-09-30）：hijack 路径（带 stdin 的 exec）必须让
// ExecRequest.Timeout 真正生效——此前 ctx 超时只作用于 dial，agent 卡死时 io.ReadFull
// 会永久阻塞（实测 coder 卡 ~106 分钟），evolve-driver 串行链会被单次卡死。
// 需要本机 docker socket 与任一可 exec 的容器（默认 workflow-mysql，可用 TEST_CONTAINER 覆盖）。
func TestExecStdinHijackTimeout(t *testing.T) {
	if os.Getenv("DOCKER_HOST_TEST") == "" {
		t.Skip("set DOCKER_HOST_TEST=1 to run against local docker")
	}
	container := os.Getenv("TEST_CONTAINER")
	if container == "" {
		container = "workflow-mysql"
	}
	c, err := New("unix:///run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = c.Exec(context.Background(), ExecRequest{
		Container: container,
		Cmd:       []string{"sleep", "60"},
		Stdin:     []byte("x"),
		Timeout:   3 * time.Second,
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error, got nil (elapsed=%s)", elapsed)
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "context deadline") {
		t.Fatalf("expected deadline error, got: %v", err)
	}
	// 超时应在 ~3s 内返回，允许 15s 余量（含建 exec 实例开销）
	if elapsed > 15*time.Second {
		t.Fatalf("timeout not enforced: took %s", elapsed)
	}
	t.Logf("ok: enforced in %s (err=%v)", elapsed.Round(time.Millisecond), err)
}
