// P4-1（ai-flow 两阶段确认审计双写）：POST/GET /audit-logs 入口单测。
//
// 不连真实 MySQL：以 sqlite :memory: 顶替 audit_logs 表，验证请求 → 落库
// 映射（actor / action / target / detail.on_behalf_of）、参数校验、身份收口
// （actor 固定 ai-flow、action 白名单、on_behalf_of 以 JWT sub 为准）与
// 非 admin 只能查自己的读侧限制。

package approval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/wetee/ai-workflow/internal/db"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newAuditTestStore(t *testing.T) *Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := gdb.Exec(`CREATE TABLE audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_id INTEGER NULL,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		note TEXT,
		detail TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`).Error; err != nil {
		t.Fatalf("create audit_logs: %v", err)
	}
	return &Store{db: &db.DB{DB: gdb}, cfg: &Config{}}
}

// postAudit posts an audit payload. jwtUser/jwtRoles simulate the identity
// injected by the requireAuth/requireRole middleware (empty = no-auth env).
func postAudit(t *testing.T, s *Store, body string, jwtUser string, jwtRoles []string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/audit-logs", inject(jwtUser, jwtRoles), s.appendAuditLog)
	req := httptest.NewRequest(http.MethodPost, "/audit-logs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// inject simulates the authn middleware context injection.
func inject(user string, roles []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if user != "" {
			c.Set("authn_user_id", user)
			c.Set("authn_roles", roles)
		}
		c.Next()
	}
}

func TestAppendAuditLogHappyPath(t *testing.T) {
	s := newAuditTestStore(t)
	w := postAudit(t, s, `{"actor":"ai-flow","on_behalf_of":"user-42","tool":"publish_card","params_hash":"abc123","result":"confirmed_executed","action":"picoclaw_write_confirm"}`, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var rows []db.AuditLog
	if err := s.db.Find(&rows).Error; err != nil {
		t.Fatalf("query audit_logs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.Actor != "ai-flow" || row.Action != "picoclaw_write_confirm" || row.Target != "publish_card" {
		t.Fatalf("row mapping mismatch: %+v", row)
	}
	// on_behalf_of 落 detail：验收「audit_logs 可按 on_behalf_of 查到记录」。
	var detail map[string]any
	if err := json.Unmarshal([]byte(row.Detail), &detail); err != nil {
		t.Fatalf("detail not JSON: %v (%s)", err, row.Detail)
	}
	if detail["on_behalf_of"] != "user-42" || detail["tool"] != "publish_card" || detail["params_hash"] != "abc123" {
		t.Fatalf("detail mismatch: %v", detail)
	}
}

func TestAppendAuditLogDefaultsAndValidation(t *testing.T) {
	s := newAuditTestStore(t)

	// actor / action omitted → defaults ai-flow / picoclaw_write_confirm.
	w := postAudit(t, s, `{"tool":"t1","on_behalf_of":"u1","params_hash":"h1"}`, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	// missing tool → 400.
	w = postAudit(t, s, `{"actor":"ai-flow"}`, "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing tool: status = %d, want 400", w.Code)
	}
	// malformed JSON → 400.
	w = postAudit(t, s, `{oops`, "", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("malformed json: status = %d, want 400", w.Code)
	}

	var count int64
	if err := s.db.Model(&db.AuditLog{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("rows = %d, want 1 (validation rejections must not insert)", count)
	}
	var row db.AuditLog
	if err := s.db.First(&row).Error; err != nil {
		t.Fatalf("first: %v", err)
	}
	if row.Actor != "ai-flow" || row.Action != "picoclaw_write_confirm" {
		t.Fatalf("defaults not applied: %+v", row)
	}
}

func TestAppendAuditLogIdentityHardening(t *testing.T) {
	s := newAuditTestStore(t)

	// 1. actor 自报非 ai-flow → 403（actor 固定服务身份）。
	w := postAudit(t, s, `{"actor":"admin","tool":"t","on_behalf_of":"alice"}`, "alice", []string{"admin"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("actor=admin: status = %d, want 403", w.Code)
	}

	// 2. action 白名单：伪造审批事件流 → 403。
	w = postAudit(t, s, `{"tool":"APV-1","on_behalf_of":"alice","action":"approval_approved"}`, "alice", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("action=approval_approved: status = %d, want 403", w.Code)
	}
	w = postAudit(t, s, `{"tool":"t","on_behalf_of":"alice","action":"cd_failed"}`, "alice", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("action=cd_failed: status = %d, want 403", w.Code)
	}

	// 3. on_behalf_of 与 JWT sub 不一致 → 403（不能代表其它用户）。
	w = postAudit(t, s, `{"tool":"t","on_behalf_of":"victim"}`, "alice", nil)
	if w.Code != http.StatusForbidden {
		t.Fatalf("on_behalf_of=victim: status = %d, want 403", w.Code)
	}

	// 4. on_behalf_of 与 sub 一致 → 放行，且落库以 JWT sub 为准。
	w = postAudit(t, s, `{"tool":"t","on_behalf_of":"alice","params_hash":"h"}`, "alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("self on_behalf_of: status = %d body = %s", w.Code, w.Body.String())
	}

	// 5. 未显式传 on_behalf_of → 以 JWT sub 补齐。
	w = postAudit(t, s, `{"tool":"t2","params_hash":"h2"}`, "bob", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("missing on_behalf_of: status = %d body = %s", w.Code, w.Body.String())
	}

	var rows []db.AuditLog
	if err := s.db.Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (403 rejections must not insert)", len(rows))
	}
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal([]byte(row.Detail), &detail); err != nil {
			t.Fatalf("detail not JSON: %v", err)
		}
		if row.Actor != "ai-flow" {
			t.Fatalf("actor leaked from body: %+v", row)
		}
		if detail["on_behalf_of"] == "" || detail["on_behalf_of"] == "victim" {
			t.Fatalf("on_behalf_of not taken from JWT sub: %+v", detail)
		}
	}
}

func TestListAuditLogsByOnBehalfOf(t *testing.T) {
	s := newAuditTestStore(t)

	// 写入两条不同 on_behalf_of 的审计 + 一条其它 actor 记录（无 auth 环境
	// 兼容路径：on_behalf_of 取请求体自报）。
	if w := postAudit(t, s, `{"on_behalf_of":"alice","tool":"t1","params_hash":"h1","result":"confirmed_executed"}`, "", nil); w.Code != http.StatusOK {
		t.Fatalf("post 1: %d %s", w.Code, w.Body.String())
	}
	if w := postAudit(t, s, `{"on_behalf_of":"bob","tool":"t2","params_hash":"h2","result":"confirmed_executed"}`, "", nil); w.Code != http.StatusOK {
		t.Fatalf("post 2: %d %s", w.Code, w.Body.String())
	}
	s.Log(context.Background(), "admin", "approval_approved", "APV-1", nil)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/audit-logs", s.listAuditLogs)

	query := func(qs string) []db.AuditLog {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/audit-logs"+qs, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", qs, w.Code, w.Body.String())
		}
		var resp struct {
			Logs  []db.AuditLog `json:"logs"`
			Total int           `json:"total"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %s: %v", qs, err)
		}
		return resp.Logs
	}

	// P4-1 验收：audit_logs 可按 on_behalf_of 查到记录。
	if logs := query("?on_behalf_of=alice"); len(logs) != 1 || logs[0].Target != "t1" {
		t.Fatalf("on_behalf_of=alice: %+v", logs)
	}
	if logs := query("?on_behalf_of=bob"); len(logs) != 1 || logs[0].Target != "t2" {
		t.Fatalf("on_behalf_of=bob: %+v", logs)
	}
	if logs := query("?on_behalf_of=carol"); len(logs) != 0 {
		t.Fatalf("on_behalf_of=carol: %+v, want empty", logs)
	}
	if logs := query("?actor=admin"); len(logs) != 1 || logs[0].Action != "approval_approved" {
		t.Fatalf("actor=admin: %+v", logs)
	}
}

func TestListAuditLogsNonAdminScopedToSelf(t *testing.T) {
	s := newAuditTestStore(t)

	// 写入 alice / bob 各一条。
	if w := postAudit(t, s, `{"on_behalf_of":"alice","tool":"t1"}`, "", nil); w.Code != http.StatusOK {
		t.Fatalf("post alice: %d", w.Code)
	}
	if w := postAudit(t, s, `{"on_behalf_of":"bob","tool":"t2"}`, "", nil); w.Code != http.StatusOK {
		t.Fatalf("post bob: %d", w.Code)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/audit-logs", inject("alice", []string{"board_approver"}), s.listAuditLogs)

	// 非 admin：不传 on_behalf_of → 强制只看自己。
	req := httptest.NewRequest(http.MethodGet, "/audit-logs", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var resp struct {
		Logs []db.AuditLog `json:"logs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Logs) != 1 || !strings.Contains(resp.Logs[0].Detail, "alice") {
		t.Fatalf("non-admin unscoped: %+v, want only alice", resp.Logs)
	}

	// 非 admin：查别人的 on_behalf_of → 403。
	req = httptest.NewRequest(http.MethodGet, "/audit-logs?on_behalf_of=bob", nil)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin query bob: status = %d, want 403", w.Code)
	}

	// admin：不受限。
	r2 := gin.New()
	r2.GET("/audit-logs", inject("root", []string{"admin"}), s.listAuditLogs)
	req = httptest.NewRequest(http.MethodGet, "/audit-logs?on_behalf_of=bob", nil)
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin query bob: status = %d body = %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode admin: %v", err)
	}
	if len(resp.Logs) != 1 || !strings.Contains(resp.Logs[0].Detail, "bob") {
		t.Fatalf("admin query bob: %+v", resp.Logs)
	}
}
