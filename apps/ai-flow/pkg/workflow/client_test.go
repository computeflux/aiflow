package workflow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL)
	c.SetTimeout(2 * time.Second)
	return c, srv
}

func TestDoJWTForwarded(t *testing.T) {
	var gotAuth string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	ctx := ContextWithJWT(context.Background(), "test-jwt-token")
	var out map[string]bool
	if err := c.Do(ctx, http.MethodGet, "/boards", nil, nil, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer test-jwt-token" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer test-jwt-token")
	}
	if !out["ok"] {
		t.Fatal("decode failed")
	}
}

func TestDoNoJWTNoHeader(t *testing.T) {
	var hasAuth bool
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth = r.Header["Authorization"]
		_, _ = w.Write([]byte(`{}`))
	})
	if err := c.Do(context.Background(), http.MethodGet, "/x", nil, nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if hasAuth {
		t.Fatal("expected no Authorization header without JWT in ctx")
	}
}

func TestDoQueryAndBody(t *testing.T) {
	var gotQuery string
	var gotBody map[string]string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	})
	q := map[string][]string{"q": {"hello world"}}
	body := map[string]string{"ping": "pong"}
	if err := c.Do(context.Background(), http.MethodPost, "/echo", q, body, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if !strings.Contains(gotQuery, "q=hello+world") {
		t.Fatalf("query = %q", gotQuery)
	}
	if gotBody["ping"] != "pong" {
		t.Fatalf("body = %v", gotBody)
	}
}

func TestRetryOnceOn5xx(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
			return
		}
		_, _ = w.Write([]byte(`{"boards":[{"id":1,"name":"b"}]}`))
	})
	boards, truncated, err := c.Boards(context.Background())
	if err != nil {
		t.Fatalf("Boards: %v", err)
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("hits = %d, want 2 (1 retry)", hits)
	}
	if len(boards) != 1 || boards[0].ID != 1 || truncated {
		t.Fatalf("boards = %+v truncated=%v", boards, truncated)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	})
	_, err := c.Board(context.Background(), 999)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	status, ok := HTTPStatusOf(err)
	if !ok || status != http.StatusNotFound {
		t.Fatalf("HTTPStatusOf(%v) = %d,%v", err, status, ok)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits = %d, want 1 (no retry on 4xx)", hits)
	}
}

func TestListTruncation(t *testing.T) {
	n := DefaultMaxListItems + 10
	names := make([]Board, n)
	for i := range names {
		names[i] = Board{ID: uint(i + 1)}
	}
	payload, _ := json.Marshal(map[string]any{"boards": names})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	boards, truncated, err := c.Boards(context.Background())
	if err != nil {
		t.Fatalf("Boards: %v", err)
	}
	if len(boards) != DefaultMaxListItems {
		t.Fatalf("len = %d, want %d", len(boards), DefaultMaxListItems)
	}
	if !truncated {
		t.Fatal("expected truncated=true")
	}
}

func TestBoardDetailTruncatesCards(t *testing.T) {
	cards := make([]Card, DefaultMaxListItems+5)
	for i := range cards {
		cards[i] = Card{ID: uint(i + 1)}
	}
	payload, _ := json.Marshal(BoardDetail{
		ID:    1,
		Name:  "board",
		Lists: []BoardList{{ID: 10, Name: "Todo", Cards: cards}},
	})
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/boards/1" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write(payload)
	})
	detail, err := c.Board(context.Background(), 1)
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(detail.Lists) != 1 || len(detail.Lists[0].Cards) != DefaultMaxListItems {
		t.Fatalf("cards not truncated: %+v", detail.Lists)
	}
}

func TestRAGSearch(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("q"); got != "golang" {
			t.Errorf("q = %q", got)
		}
		if got := r.URL.Query().Get("k"); got != "3" {
			t.Errorf("k = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"query":      "golang",
			"sufficient": true,
			"documents": []map[string]any{
				{"chunk": map[string]any{"id": "c1", "content": "hello"}, "score": 0.9},
			},
		})
	})
	res, err := c.RAGSearch(context.Background(), "golang", 3, 0)
	if err != nil {
		t.Fatalf("RAGSearch: %v", err)
	}
	if !res.Sufficient || len(res.Documents) != 1 || res.Documents[0].Chunk.Content != "hello" {
		t.Fatalf("res = %+v", res)
	}
	if res.Truncated {
		t.Fatal("unexpected truncated")
	}
	if _, err := c.RAGSearch(context.Background(), "  ", 1, 1); err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestCardExecutionsAndOpsReportsAndServices(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/cards/7/executions", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"card_id":    7,
			"executions": []map[string]any{{"work_item_no": "wi-1", "status": "done"}},
		})
	})
	mux.HandleFunc("/ops-reports", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("type"); got != "daily" {
			t.Errorf("type = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"id": 1, "type": "daily", "report_date": "2026-11-10", "content": "daily report body"}},
			"total": 2,
		})
	})
	mux.HandleFunc("/services", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"services": []map[string]any{{"name": "api", "state": "running", "managed": true}},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClient(srv.URL)

	execs, truncated, err := c.CardExecutions(context.Background(), 7)
	if err != nil || len(execs) != 1 || execs[0].WorkItemNo != "wi-1" || truncated {
		t.Fatalf("CardExecutions: %v %+v %v", err, execs, truncated)
	}

	reports, truncated, err := c.OpsReports(context.Background(), "daily")
	if err != nil || len(reports) != 1 {
		t.Fatalf("OpsReports: %v %+v", err, reports)
	}
	if reports[0].Content != "daily report body" || reports[0].ReportDate != "2026-11-10" {
		t.Fatalf("OpsReports fields not mapped from upstream: %+v", reports[0])
	}
	if !truncated { // total=2 > items=1
		t.Fatal("expected truncated from total")
	}

	svcs, truncated, err := c.Services(context.Background())
	if err != nil || len(svcs) != 1 || svcs[0].Name != "api" || truncated {
		t.Fatalf("Services: %v %+v %v", err, svcs, truncated)
	}
}

func TestTimeoutNoRetry(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	})
	c.SetTimeout(50 * time.Millisecond)
	_, _, err := c.Boards(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	// Timeouts are terminal: the server cancels its work with the request
	// context, so a timed-out attempt must NOT be retried.
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits = %d, want 1 (timeout is not retried)", hits)
	}
}

func TestJWTFromContextVariants(t *testing.T) {
	if got := JWTFromContext(nil); got != "" {
		t.Fatalf("nil ctx = %q", got)
	}
	ctx := context.WithValue(context.Background(), ContextKeyJWT, []byte("bytes-jwt"))
	if got := JWTFromContext(ctx); got != "bytes-jwt" {
		t.Fatalf("bytes ctx = %q", got)
	}
	if got := JWTFromContext(context.Background()); got != "" {
		t.Fatalf("empty ctx = %q", got)
	}
}

func TestRAGSearchUsesRAGTimeout(t *testing.T) {
	// The test client sets SetTimeout(2s); a /search that takes ~2.5s must
	// still succeed because RAGSearch uses the dedicated RAG timeout
	// (DefaultRAGTimeout = 60s), proving the slow endpoint is not bound by
	// the fast default and is not retried into a double pipeline run.
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(2500 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"query":      "slow",
			"sufficient": true,
			"documents":  []map[string]any{},
		})
	})
	res, err := c.RAGSearch(context.Background(), "slow", 0, 0)
	if err != nil {
		t.Fatalf("RAGSearch: %v", err)
	}
	if !res.Sufficient {
		t.Fatalf("res = %+v", res)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits = %d, want 1 (single attempt under RAG timeout)", hits)
	}
}

func TestRAGTimeoutOverrideApplies(t *testing.T) {
	var hits int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	})
	c.SetRAGTimeout(50 * time.Millisecond)
	_, err := c.RAGSearch(context.Background(), "q", 0, 0)
	if err == nil {
		t.Fatal("expected RAG timeout error")
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits = %d, want 1 (timeout not retried)", hits)
	}
}
