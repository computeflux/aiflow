package dockerapi

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestExecStdinHijack 需要本机 docker socket 与运行中的 pi-web 容器（仅本地手动跑）。
func TestExecStdinHijack(t *testing.T) {
	if os.Getenv("DOCKER_HOST_TEST") == "" {
		t.Skip("set DOCKER_HOST_TEST=1 to run against local docker")
	}
	c, err := New("unix:///run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Exec(context.Background(), ExecRequest{
		Container: "pi-web",
		Cmd:       []string{"cat"},
		Stdin:     []byte(`{"op":"id"}`),
		Timeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != `{"op":"id"}` {
		t.Fatalf("exit=%d stdout=%q stderr=%q", res.ExitCode, res.Stdout, res.Stderr)
	}
	t.Log("ok: exit=0 stdout matched")
}
