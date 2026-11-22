// [workflow-fork] P2-3/P2-4 tests: read-only workflow tools.
// [workflow-fork] P4-2: All() now returns read + write surfaces; read
// behaviour (auth passthrough, silent results, query-before-write) is
// pinned here.

package workflowtools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/computeflux/ai-flow/pkg/tools"
	"github.com/computeflux/ai-flow/pkg/workflow"
)

type toolsIface = tools.Tool

func newTestClient(t *testing.T, handler http.HandlerFunc) *workflow.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := workflow.NewClient(srv.URL)
	c.SetMaxAttempts(1)
	return c
}

// authCtx returns a context carrying a user JWT, as the P3 identity
// passthrough provides in production.
func authCtx() context.Context {
	return workflow.ContextWithJWT(context.Background(), "test-jwt")
}

func TestAll_ToolNamesAndSchemas(t *testing.T) {
	tools := All(nil)
	// 6 read tools + 2 write surfaces (workflow_card / workflow_board).
	want := []string{
		ToolNameBoard, ToolNameCard, ToolNameRAG,
		ToolNameOpsReport, ToolNameService, ToolNameSkill,
		ToolNameCard, ToolNameBoard,
	}
	if len(tools) != len(want) {
		t.Fatalf("All() len = %d, want %d", len(tools), len(want))
	}
	for i, tool := range tools {
		if tool.Name() != want[i] {
			t.Fatalf("tools[%d].Name() = %q, want %q", i, tool.Name(), want[i])
		}
		if strings.TrimSpace(tool.Description()) == "" {
			t.Fatalf("%s: empty description", tool.Name())
		}
		if tool.Parameters() == nil {
			t.Fatalf("%s: nil parameters", tool.Name())
		}
	}
}

func TestBoardTool_ListAndGet(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/boards":
			_, _ = w.Write([]byte(`{"boards":[{"id":1,"name":"Bug 看板"}]}`))
		case "/boards/1":
			_, _ = w.Write([]byte(`{"id":1,"name":"Bug 看板","lists":[{"id":10,"name":"Backlog","cards":[{"id":214,"title":"P2-3: workflow tools"}]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	boardTool := NewBoardTool(c)

	res := boardTool.Execute(authCtx(), map[string]any{"action": "list"})
	if res.IsError {
		t.Fatalf("list failed: %s", res.ForLLM)
	}
	if !res.Silent {
		t.Fatalf("list result must be Silent (tool output is LLM context only)")
	}
	if !strings.Contains(res.ForLLM, "Bug 看板") {
		t.Fatalf("list result = %q", res.ForLLM)
	}
	if !strings.HasPrefix(res.ForLLM, "<untrusted_data>") || !strings.HasSuffix(res.ForLLM, "</untrusted_data>") {
		t.Fatalf("list result not wrapped in <untrusted_data>: %q", res.ForLLM)
	}

	res = boardTool.Execute(authCtx(), map[string]any{"action": "get", "id": float64(1)})
	if res.IsError {
		t.Fatalf("get failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "P2-3: workflow tools") {
		t.Fatalf("get result missing card title: %q", res.ForLLM)
	}

	res = boardTool.Execute(authCtx(), map[string]any{"action": "get"})
	if !res.IsError {
		t.Fatal("get without id should error (query-before-write)")
	}

	res = boardTool.Execute(authCtx(), map[string]any{"action": "delete"})
	if !res.IsError {
		t.Fatal("unknown action should error")
	}
}

func TestSkillTool_ListAndRuns(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/skills":
			_, _ = w.Write([]byte(`{"skills":[{"id":1,"name":"xhs-like","runtime":"go","live_version":"v1.2.0"}]}`))
		case "/api/skills/xhs-like/runs":
			_, _ = w.Write([]byte(`{"runs":[{"id":9,"skill_name":"xhs-like","version":"v1.2.0","status":"ok","created_at":"2026-11-20T15:00:00Z"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	skillTool := NewSkillTool(c)

	res := skillTool.Execute(authCtx(), map[string]any{"action": "list"})
	if res.IsError {
		t.Fatalf("list failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "xhs-like") {
		t.Fatalf("list result = %q", res.ForLLM)
	}

	res = skillTool.Execute(authCtx(), map[string]any{"action": "runs", "name": "xhs-like"})
	if res.IsError {
		t.Fatalf("runs failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "ok") {
		t.Fatalf("runs result missing run status: %q", res.ForLLM)
	}

	res = skillTool.Execute(authCtx(), map[string]any{"action": "runs"})
	if !res.IsError {
		t.Fatal("runs without name should error")
	}

	res = skillTool.Execute(authCtx(), map[string]any{"action": "invoke"})
	if !res.IsError {
		t.Fatal("unknown action should error (this tool is read-only)")
	}
}

func TestCardTool_RequiresRealID(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cards/214/executions" {
			_, _ = w.Write([]byte(`{"executions":[{"work_item_no":"wi-1","status":"done"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	cardTool := NewCardTool(c)
	res := cardTool.Execute(authCtx(), map[string]any{})
	if !res.IsError {
		t.Fatal("missing id should error")
	}
	res = cardTool.Execute(authCtx(), map[string]any{"id": json.Number("214")})
	if res.IsError {
		t.Fatalf("executions failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "wi-1") {
		t.Fatalf("result = %q", res.ForLLM)
	}
}

func TestRAGTool_EmptyQuery(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"query":"发布","sufficient":true,"documents":[]}`))
	})
	ragTool := NewRAGTool(c)

	if res := ragTool.Execute(authCtx(), map[string]any{}); !res.IsError {
		t.Fatal("empty query should error")
	}
	res := ragTool.Execute(authCtx(), map[string]any{"query": "发布"})
	if res.IsError {
		t.Fatalf("search failed: %s", res.ForLLM)
	}
}

func TestOpsReportAndServiceTools(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ops-reports":
			_, _ = w.Write([]byte(`{"items":[{"id":1,"type":"daily","summary":"ok"}],"total":1}`))
		case "/services":
			_, _ = w.Write([]byte(`{"services":[{"name":"workflow-api","state":"running"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	res := NewOpsReportTool(c).Execute(authCtx(), map[string]any{"type": "daily"})
	if res.IsError || !strings.Contains(res.ForLLM, "daily") {
		t.Fatalf("ops reports: %q", res.ForLLM)
	}
	res = NewServiceTool(c).Execute(authCtx(), nil)
	if res.IsError || !strings.Contains(res.ForLLM, "workflow-api") {
		t.Fatalf("services: %q", res.ForLLM)
	}
}

func TestTools_RejectWriteMethods(t *testing.T) {
	// The read-only tools (P2-1/P2-3) never issue non-GET requests even on
	// weird args.
	var methods []string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		_, _ = w.Write([]byte(`{}`))
	})
	for _, tool := range readToolsForTest(c) {
		tool.Execute(authCtx(), map[string]any{"action": "delete", "id": float64(1), "query": "x"})
	}
	for _, m := range methods {
		if m != http.MethodGet {
			t.Fatalf("non-GET request issued: %v", methods)
		}
	}
}

func TestBoardTool_BoardIDPassedAsQuery(t *testing.T) {
	// workflow_board action=get must hit /boards/:id exactly (real ids only;
	// fabricated ids surface as upstream 404 errors, never silent success).
	var path string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"board not found"}`))
	})

	res := NewBoardTool(c).Execute(authCtx(), map[string]any{
		"action": "get",
		"id":     float64(999999),
	})
	if !res.IsError {
		t.Fatalf("expected upstream 404 error, got: %q", res.ForLLM)
	}
	if path != "/boards/999999" {
		t.Fatalf("request path = %q, want /boards/999999", path)
	}
}

func TestTools_RequireUserJWT(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request issued without user JWT: %s", r.URL.Path)
	})
	for _, tool := range readToolsForTest(c) {
		res := tool.Execute(context.Background(), map[string]any{"action": "list", "id": float64(1), "query": "x"})
		if !res.IsError || !strings.Contains(res.ForLLM, "no user token") {
			t.Fatalf("%s: expected missing-token error, got: %q", tool.Name(), res.ForLLM)
		}
	}
}

// readToolsForTest returns the five read-only tools (P2-3) for
// interface-level tests.
func readToolsForTest(c *workflow.Client) []toolsIface {
	return []toolsIface{
		NewBoardTool(c), NewCardTool(c), NewRAGTool(c),
		NewOpsReportTool(c), NewServiceTool(c),
	}
}
