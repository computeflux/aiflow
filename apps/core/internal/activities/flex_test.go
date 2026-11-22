package activities

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newFlexTestServer 模拟 flex-server /runs 的指定响应。
func newFlexTestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runs" {
			t.Errorf("path = %s, want /runs", r.URL.Path)
		}
		var req struct {
			Graph string         `json:"graph"`
			Input map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Graph != "weekly_ideas" {
			t.Errorf("graph = %q, want weekly_ideas", req.Graph)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestFlexActivitySuccess(t *testing.T) {
	srv := newFlexTestServer(t, http.StatusOK, `{"ok":true,"output":{"final":{"ideas":["a"],"count":1}}}`)
	defer srv.Close()
	t.Setenv("FLEX_ENDPOINT", srv.URL)

	a := &Activities{} // DB 为 nil：recordFlexRun 为 no-op
	out, err := a.FlexActivity(context.Background(), "weekly_ideas", map[string]any{"sources": []any{}})
	if err != nil {
		t.Fatalf("FlexActivity: %v", err)
	}
	final, _ := out["final"].(map[string]any)
	if final == nil {
		t.Fatalf("output missing final: %v", out)
	}
}

func TestFlexActivityTimeout(t *testing.T) {
	srv := newFlexTestServer(t, http.StatusGatewayTimeout, `{"detail":"run timed out"}`)
	defer srv.Close()
	t.Setenv("FLEX_ENDPOINT", srv.URL)

	a := &Activities{}
	if _, err := a.FlexActivity(context.Background(), "weekly_ideas", map[string]any{}); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestFlexActivityFailStatus(t *testing.T) {
	srv := newFlexTestServer(t, http.StatusBadRequest, `{"detail":"input too large"}`)
	defer srv.Close()
	t.Setenv("FLEX_ENDPOINT", srv.URL)

	a := &Activities{}
	if _, err := a.FlexActivity(context.Background(), "weekly_ideas", map[string]any{}); err == nil {
		t.Fatal("expected HTTP 400 error")
	}
}

func TestFlexActivityBadShape(t *testing.T) {
	srv := newFlexTestServer(t, http.StatusOK, `{"ok":false}`)
	defer srv.Close()
	t.Setenv("FLEX_ENDPOINT", srv.URL)

	a := &Activities{}
	if _, err := a.FlexActivity(context.Background(), "weekly_ideas", map[string]any{}); err == nil {
		t.Fatal("expected bad-shape error")
	}
}
