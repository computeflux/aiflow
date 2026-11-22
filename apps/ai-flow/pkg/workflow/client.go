// [workflow-fork] P2-1: ai-bot → workflow-api (:8001, apps/core/cmd/api) typed read-only client.
//
// Design:
//   - Client.Do(ctx, method, path, query, body) is the single transport funnel:
//     it injects the user JWT from ctx (ContextKeyJWT — the context key agreed
//     with the upcoming identity.go work item) as `Authorization: Bearer <jwt>`
//     verbatim, applies a per-request timeout (default 10s; slow CRAG /search
//     uses DefaultRAGTimeout = 60s) and retries once on network errors / 5xx
//     responses (total attempts = 2). Timeouts / cancellations are terminal:
//     never retried, because the server cancels its pipeline with the request.
//   - Typed helpers cover the first batch of read-only endpoints:
//     GET /boards                      → Boards()
//     GET /boards/:id                  → Board(id)   (lists + cards preloaded server-side)
//     GET /cards/:id/executions        → CardExecutions(id)
//     GET /search?q=&k=&days=          → RAGSearch(q, k, days)  (CRAG RAG search)
//     GET /ops-reports[?type=]         → OpsReports(typ)
//     GET /services                    → Services()
//   - List responses are truncated to MaxListItems (default 50) per domain with
//     Truncated=true when the upstream returned more.
//
// The client is read-only by design: no helper posts/puts/deletes, and Do
// should only be used with GET paths by callers of this package.
package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ContextKey is the context key type for this package; values are stored with
// typed keys to avoid collisions.
type ContextKey string

// ContextKeyJWT carries the end-user JWT (as issued by workflow auth service)
// in the context. The client forwards it verbatim as Authorization: Bearer.
// The upcoming identity.go work item shares this key convention.
const ContextKeyJWT ContextKey = "workflow.jwt"

// Default tuning knobs (spec: 10s timeout / 1 retry; list truncation 50).
// RAG (/search) gets its own generous budget: the server runs embed +
// evaluate(LLM) + synthesize(LLM) serially, so a normal call cannot finish
// inside the default timeout.
const (
	DefaultTimeout      = 10 * time.Second
	DefaultRAGTimeout   = 60 * time.Second // CRAG /search: embed + evaluate + synthesize
	DefaultMaxAttempts  = 2                // initial + 1 retry
	DefaultMaxListItems = 50
)

// Client is a typed HTTP client for the workflow-api read surface.
type Client struct {
	baseURL    string
	httpClient *http.Client
	timeout    time.Duration
	ragTimeout time.Duration // slow-endpoint (/search CRAG) per-request timeout
	maxAttempt int
	maxItems   int // list truncation limit; 0 = DefaultMaxListItems
}

// NewClient creates a client for the given base URL (e.g. "http://127.0.0.1:8001").
// Trailing slashes are trimmed. Zero-value tuning falls back to defaults.
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{},
		timeout:    DefaultTimeout,
		ragTimeout: DefaultRAGTimeout,
		maxAttempt: DefaultMaxAttempts,
		maxItems:   DefaultMaxListItems,
	}
}

// SetTimeout overrides the per-request timeout.
func (c *Client) SetTimeout(d time.Duration) { c.timeout = d }

// SetRAGTimeout overrides the per-request timeout for slow CRAG endpoints
// (GET /search). Falls back to DefaultRAGTimeout when unset.
func (c *Client) SetRAGTimeout(d time.Duration) { c.ragTimeout = d }

// SetMaxAttempts overrides total attempts per request (>=1).
func (c *Client) SetMaxAttempts(n int) {
	if n >= 1 {
		c.maxAttempt = n
	}
}

// SetMaxListItems overrides the per-domain list truncation limit (>=1).
func (c *Client) SetMaxListItems(n int) {
	if n >= 1 {
		c.maxItems = n
	}
}

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// WithBaseURLOverride returns a shallow clone of the client whose requests
// target overrideURL instead of the configured base URL (empty override =
// keep the current base). Used by [workflow-fork] P4-2 write tools so that
// a single Approval broker / AuditSink can be shared across per-request
// clients (incl. httptest servers in unit tests) without mutating the
// process-wide client.
func (c *Client) WithBaseURLOverride(overrideURL string) *Client {
	clone := *c
	if strings.TrimSpace(overrideURL) != "" {
		clone.baseURL = strings.TrimRight(overrideURL, "/")
	}
	return &clone
}

// APIError describes a non-2xx response from workflow-api.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("workflow api: %s %s → %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// HTTPStatusOf extracts the upstream status code from an error, if any.
func HTTPStatusOf(err error) (int, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.StatusCode, true
	}
	return 0, false
}

// Do performs a request against workflow-api and decodes the JSON response
// into out (out may be nil to discard the body). It is the single funnel for
// all typed helpers: JWT from ctx → Authorization header; query encoded into
// the URL; body JSON-encoded when non-nil. Retries once (per maxAttempt) on
// transport errors and 5xx responses — but NEVER on timeout/cancellation:
// the server cancels its own work when the request context dies, so a
// timed-out attempt is terminal (retrying would run expensive pipelines
// twice).
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	return c.do(ctx, method, path, query, body, out, c.requestTimeout())
}

func (c *Client) do(
	ctx context.Context,
	method, path string,
	query url.Values,
	body any,
	out any,
	timeout time.Duration,
) error {
	if c.baseURL == "" {
		return fmt.Errorf("workflow client: empty base URL")
	}
	u := c.baseURL + "/" + strings.TrimLeft(path, "/")
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("workflow client: encode body: %w", err)
		}
		bodyBytes = b
	}

	var lastErr error
	for attempt := 0; attempt < c.attempts(); attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, method, u, bytesReader(bodyBytes))
		if err != nil {
			cancel()
			return fmt.Errorf("workflow client: new request: %w", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		if jwt := JWTFromContext(ctx); jwt != "" {
			req.Header.Set("Authorization", "Bearer "+jwt)
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			cancel()
			lastErr = fmt.Errorf("workflow client: %s %s: %w", method, path, err)
			// Never retry timeouts / context cancellation: the upstream
			// request (and its server-side pipeline) is already canceled.
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return lastErr
			}
			continue // retry on other transport errors
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MiB guard
		closeErr := resp.Body.Close()
		cancel()
		if rerr != nil {
			lastErr = fmt.Errorf("workflow client: read response %s %s: %w", method, path, rerr)
			continue
		}
		if closeErr != nil {
			lastErr = fmt.Errorf("workflow client: close response %s %s: %w", method, path, closeErr)
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: snippet(data)}
			continue // retry on server error
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Body: snippet(data)}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("workflow client: decode %s %s: %w", method, path, err)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("workflow client: %s %s: no attempts made", method, path)
	}
	return lastErr
}

func (c *Client) attempts() int {
	if c.maxAttempt < 1 {
		return 1
	}
	return c.maxAttempt
}

func (c *Client) requestTimeout() time.Duration {
	if c.timeout > 0 {
		return c.timeout
	}
	return DefaultTimeout
}

func (c *Client) ragRequestTimeout() time.Duration {
	if c.ragTimeout > 0 {
		return c.ragTimeout
	}
	return DefaultRAGTimeout
}

func (c *Client) limit() int {
	if c.maxItems > 0 {
		return c.maxItems
	}
	return DefaultMaxListItems
}

// JWTFromContext extracts the user JWT stored under ContextKeyJWT.
// Accepts string and []byte values; returns "" when absent.
func JWTFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	switch v := ctx.Value(ContextKeyJWT).(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	// Fallback: the board turn bridge (agent.runTurn) injects the caller
	// identity via WithIdentity (identityCtxKey), not ContextKeyJWT. Resolve
	// the JWT from the Identity so both injection paths reach the workflow
	// client's Authorization header (2026-11-20 fix: board chat identity
	// passthrough broken — tools always saw "no user token").
	if id, err := IdentityFromContext(ctx); err == nil && id != nil {
		return strings.TrimSpace(id.JWT)
	}
	return ""
}

// ContextWithJWT returns a context carrying the user JWT for this client.
func ContextWithJWT(ctx context.Context, jwt string) context.Context {
	return context.WithValue(ctx, ContextKeyJWT, jwt)
}

func bytesReader(b []byte) io.Reader {
	if b == nil {
		return nil
	}
	return bytes.NewReader(b)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 512 {
		return s[:512] + "..."
	}
	return s
}

// --- Typed domain views (only fields the ai-bot consumes are typed; the
// server returns richer objects — untyped extras are dropped on decode) ---

// Board is a board summary from GET /boards.
type Board struct {
	ID        uint   `json:"id"`
	Name      string `json:"name"`
	Desc      string `json:"description"`
	UpdatedAt string `json:"updated_at"`
}

// BoardList is one column of a board (GET /boards/:id, cards preloaded).
type BoardList struct {
	ID       uint   `json:"id"`
	BoardID  uint   `json:"board_id"`
	Name     string `json:"name"`
	Position int    `json:"position"`
	Cards    []Card `json:"cards"`
}

// Card is a board card.
type Card struct {
	ID           uint   `json:"id"`
	ListID       uint   `json:"list_id"`
	Title        string `json:"title"`
	Status       string `json:"status"`
	Type         string `json:"type"`
	Priority     string `json:"priority"` // low | medium | high | urgent (upstream db.BoardCard)
	WorkItemNo   string `json:"work_item_no"`
	EvolveTaskNo string `json:"evolve_task_no"`
}

// BoardDetail is the full board from GET /boards/:id (lists + cards).
type BoardDetail struct {
	ID    uint        `json:"id"`
	Name  string      `json:"name"`
	Desc  string      `json:"description"`
	Lists []BoardList `json:"lists"`
}

// Execution is one workflow execution of a card (GET /cards/:id/executions).
type Execution struct {
	WorkItemNo   string `json:"work_item_no"`
	Kind         string `json:"kind"`
	Status       string `json:"status"`
	Title        string `json:"title"`
	TargetName   string `json:"target_name"`
	EvolveTaskNo string `json:"evolve_task_no"`
	TaskStatus   string `json:"task_status"`
	PRUrl        string `json:"pr_url"`
	CreatedAt    string `json:"created_at"`
}

// RAGDocument is one retrieved document (GET /search); mirrors
// qdrant.SearchResult{Chunk, Score} on the server side.
type RAGDocument struct {
	Chunk RAGChunk `json:"chunk"`
	Score float32  `json:"score"`
}

// RAGChunk is a retrieved content chunk.
type RAGChunk struct {
	ID              string `json:"id"`
	Content         string `json:"content"`
	Source          string `json:"source"`
	Keyword         string `json:"keyword"`
	URL             string `json:"url"`
	SourceType      string `json:"source_type,omitempty"`
	Confidentiality string `json:"confidentiality,omitempty"`
	DocTitle        string `json:"doc_title,omitempty"`
}

// RAGResult is the CRAG search response (GET /search).
type RAGResult struct {
	Query      string        `json:"query"`
	Sufficient bool          `json:"sufficient"`
	Reasoning  string        `json:"reasoning"`
	Answer     string        `json:"answer"`
	Documents  []RAGDocument `json:"documents"`
	Truncated  bool          `json:"truncated"`
}

// OpsReport is one ops report row (GET /ops-reports); mirrors
// ops.OpsReport{ID, ReportDate, Type, Content, Metrics, CreatedAt}.
type OpsReport struct {
	ID         uint   `json:"id"`
	Type       string `json:"type"`
	ReportDate string `json:"report_date"`
	Content    string `json:"content"`
	CreatedAt  string `json:"created_at"`
}

// Service is one deploy service row (GET /services).
type Service struct {
	Name      string `json:"name"`
	Container string `json:"container"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Health    string `json:"health"`
	Managed   bool   `json:"managed"`
}

// --- Typed read helpers ---

// Boards returns the board list (truncated to the configured limit).
func (c *Client) Boards(ctx context.Context) ([]Board, bool, error) {
	var resp struct {
		Boards []Board `json:"boards"`
	}
	if err := c.Do(ctx, http.MethodGet, "/boards", nil, nil, &resp); err != nil {
		return nil, false, err
	}
	items, truncated := truncate(resp.Boards, c.limit())
	return items, truncated, nil
}

// Board returns a board with its lists and cards (GET /boards/:id).
func (c *Client) Board(ctx context.Context, id int) (*BoardDetail, error) {
	var detail BoardDetail
	if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("/boards/%d", id), nil, nil, &detail); err != nil {
		return nil, err
	}
	for i := range detail.Lists {
		list := &detail.Lists[i]
		cards, _ := truncate(list.Cards, c.limit())
		list.Cards = cards
	}
	return &detail, nil
}

// CardExecutions returns the execution history of a card.
func (c *Client) CardExecutions(ctx context.Context, id int) ([]Execution, bool, error) {
	var resp struct {
		Executions []Execution `json:"executions"`
	}
	if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("/cards/%d/executions", id), nil, nil, &resp); err != nil {
		return nil, false, err
	}
	items, truncated := truncate(resp.Executions, c.limit())
	return items, truncated, nil
}

// RAGSearch performs a CRAG RAG search (GET /search). k<=0 / days<=0 use the
// server defaults (5 / 7).
func (c *Client) RAGSearch(ctx context.Context, q string, k, days int) (*RAGResult, error) {
	if strings.TrimSpace(q) == "" {
		return nil, fmt.Errorf("workflow client: rag search: empty query")
	}
	qv := url.Values{"q": {q}}
	if k > 0 {
		qv.Set("k", fmt.Sprintf("%d", k))
	}
	if days > 0 {
		qv.Set("days", fmt.Sprintf("%d", days))
	}
	var res RAGResult
	// Slow endpoint: CRAG server-side runs embed + evaluate(LLM) +
	// synthesize(LLM); use the dedicated (much larger) RAG timeout.
	if err := c.do(ctx, http.MethodGet, "/search", qv, nil, &res, c.ragRequestTimeout()); err != nil {
		return nil, err
	}
	docs, truncated := truncate(res.Documents, c.limit())
	res.Documents = docs
	res.Truncated = truncated
	return &res, nil
}

// OpsReports lists ops reports; typ may be "" (all), "daily", "weekly", ...
func (c *Client) OpsReports(ctx context.Context, typ string) ([]OpsReport, bool, error) {
	var qv url.Values
	if strings.TrimSpace(typ) != "" {
		qv = url.Values{"type": {typ}}
	}
	var resp struct {
		Items []OpsReport `json:"items"`
		Total int         `json:"total"`
	}
	if err := c.Do(ctx, http.MethodGet, "/ops-reports", qv, nil, &resp); err != nil {
		return nil, false, err
	}
	items, truncated := truncate(resp.Items, c.limit())
	if resp.Total > len(items) {
		truncated = true
	}
	return items, truncated, nil
}

// Services lists deploy services.
func (c *Client) Services(ctx context.Context) ([]Service, bool, error) {
	var resp struct {
		Services []Service `json:"services"`
	}
	if err := c.Do(ctx, http.MethodGet, "/services", nil, nil, &resp); err != nil {
		return nil, false, err
	}
	items, truncated := truncate(resp.Services, c.limit())
	return items, truncated, nil
}

// --- Skill registry (S1 skill gateway, GET /api/skills*) ---

// SkillRun is one skill invocation record (skills gateway skill_runs).
type SkillRun struct {
	ID        uint   `json:"id"`
	SkillName string `json:"skill_name"`
	Version   string `json:"version"`
	Status    string `json:"status"` // ok | fail | timeout | rejected
	LatencyMs *int64 `json:"latency_ms"`
	Error     string `json:"error"`
	CreatedAt string `json:"created_at"`
}

// Skill is a skill registry entry (GET /api/skills / /api/skills/:name) with
// its live version and (detail view) recent runs / run count. Schema fields
// stay raw JSON (the LLM reads them as-is; we do not re-schema the gateway).
type Skill struct {
	ID           uint            `json:"id"`
	Name         string          `json:"name"`
	Owner        string          `json:"owner"`
	Runtime      string          `json:"runtime"`
	Type         string          `json:"type"`
	Entrypoint   string          `json:"entrypoint"`
	Addr         string          `json:"addr"`
	Container    string          `json:"container"`
	LiveVersion  string          `json:"live_version"`
	Versions     json.RawMessage `json:"versions,omitempty"`
	RecentRuns   []SkillRun      `json:"recent_runs,omitempty"`
	RunCount     int64           `json:"run_count,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Limits       json.RawMessage `json:"limits,omitempty"`
	Permissions  json.RawMessage `json:"permissions,omitempty"`
}

// Skills lists the skill registry (GET /api/skills; the gateway view already
// embeds each skill's live_version + versions).
func (c *Client) Skills(ctx context.Context) ([]Skill, bool, error) {
	var resp struct {
		Skills []Skill `json:"skills"`
	}
	if err := c.Do(ctx, http.MethodGet, "/api/skills", nil, nil, &resp); err != nil {
		return nil, false, err
	}
	items, truncated := truncate(resp.Skills, c.limit())
	return items, truncated, nil
}

// SkillRuns returns recent invocation records of one skill
// (GET /api/skills/:name/runs).
func (c *Client) SkillRuns(ctx context.Context, name string, limit int) ([]SkillRun, bool, error) {
	var qv url.Values
	if limit > 0 {
		qv = url.Values{"limit": {fmt.Sprintf("%d", limit)}}
	}
	var resp struct {
		Runs []SkillRun `json:"runs"`
	}
	if err := c.Do(ctx, http.MethodGet,
		"/api/skills/"+url.PathEscape(name)+"/runs", qv, nil, &resp); err != nil {
		return nil, false, err
	}
	items, truncated := truncate(resp.Runs, c.limit())
	return items, truncated, nil
}

// truncate caps s at n, returning the slice and whether items were dropped.
func truncate[T any](s []T, n int) ([]T, bool) {
	if n > 0 && len(s) > n {
		return s[:n], true
	}
	return s, false
}
