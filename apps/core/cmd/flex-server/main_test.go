package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wetee/ai-workflow/internal/flex"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// doRun 打 /runs 并返回状态码 + 响应体。
func doRun(t *testing.T, graph string, input any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"graph": graph, "input": input})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	route := gin.New()
	route.POST("/runs", handleRun)
	route.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestRunSuccess(t *testing.T) {
	code, out := doRun(t, "weekly_ideas", map[string]any{
		"sources": []any{
			map[string]any{"name": "x", "items": []any{"AI agents are eating the stack", "temporal orchestration"}},
			map[string]any{"name": "github", "items": []any{"langgraph state graph tutorial"}},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("code = %d: %v", code, out)
	}
	final, _ := out["output"].(map[string]any)["final"].(map[string]any)
	if final == nil {
		t.Fatalf("missing final: %v", out)
	}
	if c, _ := final["count"].(float64); c <= 0 {
		t.Fatalf("count = %v", final["count"])
	}
}

func TestRunUnknownGraph(t *testing.T) {
	code, _ := doRun(t, "no_such_graph", map[string]any{})
	if code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", code)
	}
}

func TestRunInputSchemaFail(t *testing.T) {
	// 缺 sources
	code, out := doRun(t, "weekly_ideas", map[string]any{"foo": 1})
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d: %v", code, out)
	}
}

// 构造 21 节点线性图：S2 §3.3 硬约束节点 ≤ 20 → 必须拒绝
func TestRunNodeLimit(t *testing.T) {
	g := flex.New("overflow21", "n0")
	for i := 0; i < 21; i++ {
		i := i
		g.AddNode(fmt.Sprintf("n%d", i), func(_ context.Context, s flex.State) (flex.State, error) {
			return flex.State{fmt.Sprintf("n%d", i): true}, nil
		})
	}
	for i := 0; i < 20; i++ {
		g.AddEdge(fmt.Sprintf("n%d", i), fmt.Sprintf("n%d", i+1))
	}
	if err := g.Compile(); err != nil {
		t.Fatalf("compile: %v", err)
	}
	flex.Register(flex.GraphDef{Name: "overflow21", Graph: g})

	code, out := doRun(t, "overflow21", map[string]any{})
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d: %v", code, out)
	}
	if d := out["detail"].(string); !strings.Contains(d, "21 nodes > 20") {
		t.Fatalf("detail = %q", d)
	}
}

// 构造超大输入（> 50k token 估算）→ 必须拒绝
func TestRunTokenLimit(t *testing.T) {
	items := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		items = append(items, strings.Repeat("a", 500))
	}
	code, out := doRun(t, "weekly_ideas", map[string]any{
		"sources": []any{map[string]any{"name": "x", "items": items}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d: %v", code, out)
	}
}

// 阻塞图 + 缩短 runTimeout：验证 10 分钟硬超时 → 504 的代码路径
func TestRunTimeout(t *testing.T) {
	g := flex.New("blocker", "sleep")
	g.AddNode("sleep", func(ctx context.Context, _ flex.State) (flex.State, error) {
		<-ctx.Done()
		return flex.State{}, ctx.Err()
	})
	_ = g.Compile()
	flex.Register(flex.GraphDef{Name: "blocker", Graph: g})

	old := runTimeout
	runTimeout = 50 * time.Millisecond
	defer func() { runTimeout = old }()

	code, out := doRun(t, "blocker", map[string]any{})
	if code != http.StatusGatewayTimeout {
		t.Fatalf("code = %d: %v", code, out)
	}
}

func TestRunNodeError(t *testing.T) {
	g := flex.New("boomgraph", "boom")
	g.AddNode("boom", func(_ context.Context, _ flex.State) (flex.State, error) {
		return nil, fmt.Errorf("boom")
	})
	_ = g.Compile()
	flex.Register(flex.GraphDef{Name: "boomgraph", Graph: g})

	code, _ := doRun(t, "boomgraph", map[string]any{})
	if code != http.StatusInternalServerError {
		t.Fatalf("code = %d, want 500", code)
	}
}
