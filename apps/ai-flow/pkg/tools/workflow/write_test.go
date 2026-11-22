// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// [workflow-fork] P4-2 unit tests: workflow 写工具（card/board write
// actions）。覆盖验收标准：
//   - 越权组合测试 100% 拒绝且提示所需角色（delete→admin，approve/decision→
//     board_approver；preview 与 confirm 两阶段同样拦截）；
//   - 高危单次仅允许 1 个动作（多动作 args 直接拒绝）；
//   - 无 token / 伪造 token / 过期 token / 参数篡改 → 全部拒绝，0 次 API 写；
//   - 正常链路「preview → confirm → 执行」：写调用命中真实端点、JWT 透传、
//     审计双写落 POST /audit-logs（on_behalf_of 可查）；
//   - 未配置 secret（PICOCLAW_APPROVAL_SECRET）时 confirm fail-closed。

package workflowtools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/computeflux/ai-flow/pkg/tools"
	"github.com/computeflux/ai-flow/pkg/workflow"
)

// --- test harness -------------------------------------------------------

type recAPI struct {
	t    *testing.T
	mu   sync.Mutex
	recs []apiRecord
}

type apiRecord struct {
	Method string
	Path   string
	User   string // Bearer token (acts as the identity marker in tests)
	Body   map[string]any
}

func newRecAPI(t *testing.T, statusFor func(method, path string) int) (*workflow.Client, *recAPI) {
	t.Helper()
	api := &recAPI{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		api.mu.Lock()
		api.recs = append(api.recs, apiRecord{
			Method: r.Method,
			Path:   r.URL.Path,
			User:   strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
			Body:   body,
		})
		api.mu.Unlock()
		status := http.StatusOK
		if statusFor != nil {
			status = statusFor(r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(cannedBody(r.Method, r.URL.Path)))
	}))
	t.Cleanup(srv.Close)
	c := workflow.NewClient(srv.URL)
	c.SetMaxAttempts(1)
	return c, api
}

// cannedBody returns a minimal JSON body for recorded endpoints so the
// read-surface integrations (list/get/executions) decode real payloads.
func cannedBody(method, path string) string {
	switch path {
	case "/boards":
		return `{"boards":[{"id":1,"name":"Bug 看板"}]}`
	case "/boards/1":
		return `{"id":1,"name":"Bug 看板","lists":[{"id":10,"name":"Backlog","cards":[{"id":214,"title":"P2-3: workflow tools"}]}]}`
	case "/cards/214/executions":
		return `{"executions":[{"work_item_no":"wi-1","status":"done"}]}`
	}
	return `{}`
}

func (a *recAPI) writes() []apiRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []apiRecord
	for _, r := range a.recs {
		if r.Method != http.MethodGet {
			out = append(out, r)
		}
	}
	return out
}

func (a *recAPI) find(method, path string) []apiRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []apiRecord
	for _, r := range a.recs {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// newDeps builds write deps whose client AND audit sink target the same
// httptest API (the audit double-write lands as POST /audit-logs).
func newDeps(secret string, c *workflow.Client) *WriteDeps {
	d := NewWriteDeps(c, secret)
	d.BaseURLOverride = c.BaseURL()
	return d
}

func identityCtx(user string, roles ...string) context.Context {
	ctx := workflow.WithIdentity(context.Background(), &workflow.Identity{
		UserID: user,
		Roles:  roles,
		JWT:    "jwt-" + user,
	})
	return workflow.ContextWithJWT(ctx, "jwt-"+user)
}

// previewToken runs the preview phase and extracts the confirm token.
func previewToken(t *testing.T, tool toolsIface, ctx context.Context, args map[string]any) (string, *workflow.Preview) {
	t.Helper()
	res := tool.Execute(ctx, args)
	if res.IsError {
		t.Fatalf("preview failed: %s", res.ForLLM)
	}
	var prev workflow.Preview
	body := res.ForLLM
	start := strings.Index(body, "<untrusted_data>")
	end := strings.Index(body, "</untrusted_data>")
	if start < 0 || end < 0 {
		t.Fatalf("preview result not wrapped: %q", body)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(body[start+len("<untrusted_data>"):end])), &prev); err != nil {
		t.Fatalf("preview decode: %v\n%s", err, body)
	}
	return prev.Token, &prev
}

// --- catalog / gating ---------------------------------------------------

func TestCatalog_HighRiskRoleMapping(t *testing.T) {
	cases := map[string]struct {
		role string
		high bool
	}{
		ActionDelete:   {RoleAdmin, true},
		ActionApprove:  {RoleBoardApprover, true},
		ActionDecision: {RoleBoardApprover, true},
		ActionCreate:   {"", false},
		ActionUpdate:   {"", false},
		ActionMove:     {"", false},
		ActionComment:  {"", false},
		ActionFinish:   {"", false},
		ActionRerun:    {"", false},
		ActionAddList:  {"", false},
	}
	for action, want := range cases {
		role, high := RequiredRole(action)
		if role != want.role || high != want.high {
			t.Fatalf("RequiredRole(%q) = (%q,%v), want (%q,%v)", action, role, high, want.role, want.high)
		}
		if high != IsHighRisk(action) {
			t.Fatalf("IsHighRisk(%q) inconsistent with RequiredRole", action)
		}
	}
}

// TestWriteTools_PermissionMatrix 越权组合测试：高危动作 × 角色缺失组合必须
// 100% 拒绝，并且错误信息提示所需角色；任何阶段不允许触碰真实 API。
func TestWriteTools_PermissionMatrix(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)

	type combo struct {
		toolName string
		action   string
		roles    []string
		wantRole string // "" = 不应被角色门拒绝
	}
	comboList := []combo{
		// delete（卡/列/看板）→ admin
		{ToolNameCard, ActionDelete, nil, RoleAdmin},
		{ToolNameCard, ActionDelete, []string{"board_approver"}, RoleAdmin},
		{ToolNameCard, ActionDelete, []string{"viewer"}, RoleAdmin},
		{ToolNameBoard, ActionDelete, nil, RoleAdmin},
		{ToolNameBoard, ActionDelete, []string{"board_approver"}, RoleAdmin},
		// approve / decision → board_approver
		{ToolNameCard, ActionApprove, nil, RoleBoardApprover},
		{ToolNameCard, ActionApprove, []string{"viewer"}, RoleBoardApprover},
		{ToolNameCard, ActionDecision, nil, RoleBoardApprover},
		{ToolNameCard, ActionDecision, []string{"viewer"}, RoleBoardApprover},
		// 正例：具备角色 → 不被角色门拒绝（进入 preview）
		{ToolNameCard, ActionDelete, []string{"admin"}, ""},
		{ToolNameBoard, ActionDelete, []string{"admin", "board_approver"}, ""},
		{ToolNameCard, ActionApprove, []string{"board_approver"}, ""},
		{ToolNameCard, ActionDecision, []string{"board_approver"}, ""},
	}

	for i, cb := range comboList {
		tool := NewCardWriteTool(deps)
		if cb.toolName == ToolNameBoard {
			tool = NewBoardWriteTool(deps)
		}
		ctx := identityCtx("u-matrix", cb.roles...)
		args := map[string]any{"action": cb.action, "id": float64(7), "target": "card"}
		if cb.toolName == ToolNameBoard {
			args["target"] = "board"
		}
		res := tool.Execute(ctx, args)

		if cb.wantRole != "" {
			// 越权组合：必须拒绝 + 提示所需角色。
			if !res.IsError {
				t.Fatalf("combo %d (%s/%s roles=%v): expected denial, got success", i, cb.toolName, cb.action, cb.roles)
			}
			if !strings.Contains(res.ForLLM, "越权") || !strings.Contains(res.ForLLM, `"`+cb.wantRole+`"`) {
				t.Fatalf("combo %d (%s/%s): denial must name the required role %q, got: %s", i, cb.toolName, cb.action, cb.wantRole, res.ForLLM)
			}
		} else if res.IsError {
			t.Fatalf("combo %d (%s/%s roles=%v): authorized preview failed: %s", i, cb.toolName, cb.action, cb.roles, res.ForLLM)
		}
	}

	// 角色门控发生在任何 API 调用之前：全程 0 次写请求。
	if writes := api.writes(); len(writes) != 0 {
		t.Fatalf("permission-gated matrix must not touch the API, got %d writes", len(writes))
	}
}

// TestWriteTools_NoIdentity 未登录/缺 JWT：写动作全部拒绝并提示登录。
func TestWriteTools_NoIdentity(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)
	tool := NewCardWriteTool(deps)

	for _, action := range WriteActions {
		res := tool.Execute(context.Background(), map[string]any{"action": action, "id": float64(1)})
		if !res.IsError {
			t.Fatalf("action %q without identity: expected denial", action)
		}
		if !strings.Contains(res.ForLLM, "未登录/缺 JWT") {
			t.Fatalf("action %q: denial must mention 未登录/缺 JWT, got %s", action, res.ForLLM)
		}
	}
	if writes := api.writes(); len(writes) != 0 {
		t.Fatalf("identity-gated writes must not touch the API, got %d", len(writes))
	}
}

// TestWriteTools_MultiActionRejected 高危单次仅允许 1 个动作。
func TestWriteTools_MultiActionRejected(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)
	tool := NewCardWriteTool(deps)
	ctx := identityCtx("u-multi", "admin")

	cases := []map[string]any{
		{"action": "delete", "id": float64(1), "actions": []any{"delete", "create"}},
		{"action": "delete,update", "id": float64(1)},
		{"action": "create", "title": "批量", "cards": []any{map[string]any{"title": "a"}, map[string]any{"title": "b"}}},
	}
	for i, args := range cases {
		res := tool.Execute(ctx, args)
		if !res.IsError {
			t.Fatalf("multi-action case %d: expected denial", i)
		}
		if !strings.Contains(res.ForLLM, "单次仅允许 1 个动作") {
			t.Fatalf("multi-action case %d: denial must state the single-action rule, got %s", i, res.ForLLM)
		}
	}

	// 单动作不受影响：preview 正常签发。
	res := tool.Execute(ctx, map[string]any{"action": "delete", "id": float64(1), "target": "card"})
	if res.IsError {
		t.Fatalf("single high-risk action by admin should preview: %s", res.ForLLM)
	}
	if writes := api.writes(); len(writes) != 0 {
		t.Fatalf("preview phase must not write, got %d", len(writes))
	}
}

// --- token lifecycle ----------------------------------------------------

// TestWriteTools_NoTokenConfirm 无 token 的 confirm：100% 拒绝，不触 API。
func TestWriteTools_NoTokenConfirm(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)
	tool := NewCardWriteTool(deps)
	ctx := identityCtx("u-notok", "admin")

	res := tool.Execute(ctx, map[string]any{"action": "update", "id": float64(9), "title": "x", "confirm": true})
	if !res.IsError {
		t.Fatal("confirm without token must be rejected")
	}
	if !strings.Contains(res.ForLLM, "两阶段确认") {
		t.Fatalf("denial must explain the two-phase flow, got %s", res.ForLLM)
	}
	if writes := api.writes(); len(writes) != 0 {
		t.Fatalf("no-token confirm must not write, got %d", len(writes))
	}
}

// TestWriteTools_ForgedToken 伪造签名 token：拒绝。
func TestWriteTools_ForgedToken(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)
	tool := NewCardWriteTool(deps)
	ctx := identityCtx("u-forged")

	forged := "9999999999|" + strings.Repeat("ab", 32)
	res := tool.Execute(ctx, map[string]any{"action": "comment", "id": float64(1), "body": "hi", "confirm": true, "confirm_token": forged})
	if !res.IsError || !strings.Contains(res.ForLLM, "令牌无效") {
		t.Fatalf("forged token must be denied, got: %+v %s", res, res.ForLLM)
	}
	if writes := api.writes(); len(writes) != 0 {
		t.Fatalf("forged confirm must not write, got %d", len(writes))
	}
}

// TestWriteTools_TokenBoundToUserAndParams token 绑定身份与参数：跨用户复用
// 与参数篡改均被拒。
func TestWriteTools_TokenBoundToUserAndParams(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)
	tool := NewCardWriteTool(deps)

	argsA := map[string]any{"action": "comment", "id": float64(1), "body": "hello"}
	token, _ := previewToken(t, tool, identityCtx("alice"), argsA)

	// 身份切换：alice 的 token 给 bob 用 → 拒绝。
	res := tool.Execute(identityCtx("bob"), mergeArgs(argsA, map[string]any{"confirm": true, "confirm_token": token}))
	if !res.IsError {
		t.Fatal("token bound to alice must not confirm for bob")
	}

	// 参数篡改：preview 后改 body（canonical args 的 hash 与 token 绑定值
	// 不再匹配）→ 验签失败，同样被拒。
	res = tool.Execute(identityCtx("alice"), mergeArgs(argsA, map[string]any{"body": "evil", "confirm": true, "confirm_token": token}))
	if !res.IsError {
		t.Fatal("tampered params must be denied")
	}

	// 过期：时钟推进超过 TTL → 拒绝。
	deps.SetClock(func() time.Time { return time.Now().Add(deps.Approval.TTL() + time.Minute) })
	res = tool.Execute(identityCtx("alice"), mergeArgs(argsA, map[string]any{"confirm": true, "confirm_token": token}))
	if !res.IsError || !strings.Contains(res.ForLLM, "过期") {
		t.Fatalf("expired token must be denied, got: %s", res.ForLLM)
	}

	if writes := api.writes(); len(writes) != 0 {
		t.Fatalf("failed confirms must not write, got %d", len(writes))
	}
}

// --- happy path ----------------------------------------------------------

// TestCardWriteTool_HappyPath 正常链路：preview → confirm → 真实写 + 审计双写。
func TestCardWriteTool_HappyPath(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)
	tool := NewCardWriteTool(deps)
	ctx := identityCtx("alice")

	args := map[string]any{"action": "create", "title": "新卡片", "list_id": float64(3)}
	token, prev := previewToken(t, tool, ctx, args)
	if !strings.Contains(prev.Action, "创建任务卡") || !strings.Contains(prev.Action, "新卡片") {
		t.Fatalf("preview action description = %q", prev.Action)
	}

	res := tool.Execute(ctx, mergeArgs(args, map[string]any{"confirm": true, "confirm_token": token}))
	if res.IsError {
		t.Fatalf("confirm+execute failed: %s", res.ForLLM)
	}
	if !strings.Contains(res.ForLLM, "[executed]") {
		t.Fatalf("executed marker missing: %s", res.ForLLM)
	}

	created := api.find(http.MethodPost, "/cards")
	if len(created) != 1 {
		t.Fatalf("expected 1 POST /cards, got %d", len(created))
	}
	if created[0].User != "jwt-alice" {
		t.Fatalf("write must carry the user JWT, got %q", created[0].User)
	}
	if created[0].Body["title"] != "新卡片" {
		t.Fatalf("create body = %v", created[0].Body)
	}

	// 审计双写：POST /audit-logs，actor=ai-bot，on_behalf_of=alice。
	audits := api.find(http.MethodPost, "/audit-logs")
	if len(audits) != 1 {
		t.Fatalf("expected 1 audit write, got %d", len(audits))
	}
	rec := audits[0].Body
	if rec["actor"] != "ai-bot" || rec["on_behalf_of"] != "alice" || rec["tool"] != ToolNameCard {
		t.Fatalf("audit record = %v", rec)
	}
	if rec["params_hash"] == "" || rec["params_hash"] != prev.ParamsHash {
		t.Fatalf("audit params_hash mismatch: %v vs %s", rec["params_hash"], prev.ParamsHash)
	}

	// 单次使用：同 token 重放 → 拒绝，且不再产生新的写。
	res = tool.Execute(ctx, mergeArgs(args, map[string]any{"confirm": true, "confirm_token": token}))
	if !res.IsError || !strings.Contains(res.ForLLM, "已被使用") {
		t.Fatalf("replay must be denied, got: %s", res.ForLLM)
	}
	if len(api.find(http.MethodPost, "/cards")) != 1 {
		t.Fatal("replay must not write again")
	}
}

// TestCardWriteTool_AllActions 所有 card 写动作在 confirm 后命中预期端点。
func TestCardWriteTool_AllActions(t *testing.T) {
	type want struct {
		method, path string
		args         map[string]any
		roles        []string
	}
	cases := map[string]want{
		ActionCreate:   {http.MethodPost, "/cards", map[string]any{"action": ActionCreate, "title": "t", "list_id": float64(1)}, nil},
		ActionUpdate:   {http.MethodPut, "/cards/7", map[string]any{"action": ActionUpdate, "id": float64(7), "title": "t"}, nil},
		ActionMove:     {http.MethodPut, "/cards/7", map[string]any{"action": ActionMove, "id": float64(7), "to_list_id": float64(2)}, nil},
		ActionComment:  {http.MethodPost, "/cards/7/comments", map[string]any{"action": ActionComment, "id": float64(7), "body": "b"}, nil},
		ActionFinish:   {http.MethodPost, "/cards/7/finish", map[string]any{"action": ActionFinish, "id": float64(7), "outcome": "done"}, nil},
		ActionRerun:    {http.MethodPost, "/cards/7/rerun", map[string]any{"action": ActionRerun, "id": float64(7), "mode": "resume"}, nil},
		ActionApprove:  {http.MethodPost, "/cards/7/approve", map[string]any{"action": ActionApprove, "id": float64(7)}, []string{"board_approver"}},
		ActionDecision: {http.MethodPost, "/cards/7/reject", map[string]any{"action": ActionDecision, "id": float64(7), "decision": "reject"}, []string{"board_approver"}},
		ActionDelete:   {http.MethodDelete, "/cards/7", map[string]any{"action": ActionDelete, "id": float64(7), "target": "card"}, []string{"admin"}},
	}
	for action, w := range cases {
		c, api := newRecAPI(t, nil)
		deps := newDeps("test-secret", c)
		tool := NewCardWriteTool(deps)
		ctx := identityCtx("u-"+action, w.roles...)

		token, _ := previewToken(t, tool, ctx, w.args)
		res := tool.Execute(ctx, mergeArgs(w.args, map[string]any{"confirm": true, "confirm_token": token}))
		if res.IsError {
			t.Fatalf("%s confirm+execute failed: %s", action, res.ForLLM)
		}
		if len(api.find(w.method, w.path)) != 1 {
			t.Fatalf("%s: expected %s %s once, records = %+v", action, w.method, w.path, api.writes())
		}
	}
}

// TestBoardWriteTool_HappyPath board 写动作：create / update / add_list /
// delete（admin）。
func TestBoardWriteTool_HappyPath(t *testing.T) {
	cases := []struct {
		action, method, path string
		args                 map[string]any
		roles                []string
	}{
		{ActionCreate, http.MethodPost, "/boards", map[string]any{"action": ActionCreate, "name": "新看板"}, nil},
		{ActionUpdate, http.MethodPut, "/boards/2", map[string]any{"action": ActionUpdate, "id": float64(2), "name": "改名"}, nil},
		{ActionAddList, http.MethodPost, "/boards/2/lists", map[string]any{"action": ActionAddList, "id": float64(2), "name": "测试列"}, nil},
		{ActionDelete, http.MethodDelete, "/boards/2", map[string]any{"action": ActionDelete, "id": float64(2), "target": "board"}, []string{"admin"}},
		{ActionDelete, http.MethodDelete, "/lists/5", map[string]any{"action": ActionDelete, "id": float64(5), "target": "list"}, []string{"admin"}},
	}
	for _, tc := range cases {
		c, api := newRecAPI(t, nil)
		deps := newDeps("test-secret", c)
		tool := NewBoardWriteTool(deps)
		ctx := identityCtx("board-user", tc.roles...)

		token, _ := previewToken(t, tool, ctx, tc.args)
		res := tool.Execute(ctx, mergeArgs(tc.args, map[string]any{"confirm": true, "confirm_token": token}))
		if res.IsError {
			t.Fatalf("%s(%s): confirm+execute failed: %s", tc.action, tc.path, res.ForLLM)
		}
		if len(api.find(tc.method, tc.path)) != 1 {
			t.Fatalf("%s: expected %s %s once, records = %+v", tc.action, tc.method, tc.path, api.writes())
		}
	}
}

// TestCardWriteTool_ExecutionsRead workflow_card action=executions 仍走只读。
func TestCardWriteTool_ExecutionsRead(t *testing.T) {
	c, _ := newRecAPI(t, func(method, path string) int { return http.StatusOK })
	deps := newDeps("test-secret", c)
	tool := NewCardWriteTool(deps)

	res := tool.Execute(identityCtx("reader"), map[string]any{"action": "executions", "id": float64(1)})
	if res.IsError {
		t.Fatalf("executions failed: %s", res.ForLLM)
	}
}

// --- fail-closed ---------------------------------------------------------

// TestWriteTools_SecretlessFailClosed 未配置 PICOCLAW_APPROVAL_SECRET：
// preview 可签发（惰性 token），任何 confirm 被拒且 0 次 API 写。
func TestWriteTools_SecretlessFailClosed(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("", c)
	tool := NewCardWriteTool(deps)
	ctx := identityCtx("alice")

	args := map[string]any{"action": "comment", "id": float64(1), "body": "x"}
	token, _ := previewToken(t, tool, ctx, args)
	res := tool.Execute(ctx, mergeArgs(args, map[string]any{"confirm": true, "confirm_token": token}))
	if !res.IsError || !strings.Contains(res.ForLLM, "两阶段确认未配置") {
		t.Fatalf("secretless confirm must fail closed, got: %s", res.ForLLM)
	}
	if writes := api.writes(); len(writes) != 0 {
		t.Fatalf("secretless flow must not write, got %d", len(writes))
	}
}

// TestWriteTools_HighRiskAuditFailClosed 高危动作 + 审计写入失败：拒绝执行
// （fail-closed，留痕红线）。
func TestWriteTools_HighRiskAuditFailClosed(t *testing.T) {
	c, _ := newRecAPI(t, func(method, path string) int {
		if method == http.MethodPost && path == "/audit-logs" {
			return http.StatusInternalServerError
		}
		return http.StatusOK
	})
	deps := newDeps("test-secret", c)
	deps.Approval = workflow.NewApprovalBroker("test-secret", 0, deps.buildAuditSink(), nil)
	tool := NewCardWriteTool(deps)
	ctx := identityCtx("admin-user", "admin")

	args := map[string]any{"action": "delete", "id": float64(9), "target": "card"}
	token, _ := previewToken(t, tool, ctx, args)
	res := tool.Execute(ctx, mergeArgs(args, map[string]any{"confirm": true, "confirm_token": token}))
	if !res.IsError || !strings.Contains(res.ForLLM, "审计写入失败") {
		t.Fatalf("high-risk confirm with failing audit must be denied, got: %s", res.ForLLM)
	}
}

// --- registry integration (P4-2 reviewer 回归) ---------------------------

// TestAllWithDeps_RegistryIntegration 把 AllWithDeps 结果真正 Register 进
// ToolRegistry（同名 last-wins，写工具覆盖只读注册）后：workflow_board 的
// list/get 与 workflow_card 的 executions 只读面仍然可用（写工具内部承接读
// action，委托给只读实现）。
func TestAllWithDeps_RegistryIntegration(t *testing.T) {
	c, _ := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)

	reg := tools.NewToolRegistry()
	for _, tool := range AllWithDeps(deps) {
		reg.Register(tool)
	}

	boardTool, ok := reg.Get(ToolNameBoard)
	if !ok {
		t.Fatal("workflow_board not registered")
	}
	res := boardTool.Execute(authCtx(), map[string]any{"action": "list"})
	if res.IsError || !strings.Contains(res.ForLLM, "Bug 看板") {
		t.Fatalf("workflow_board action=list broken after write registration: %s", res.ForLLM)
	}
	res = boardTool.Execute(authCtx(), map[string]any{"action": "get", "id": float64(1)})
	if res.IsError || !strings.Contains(res.ForLLM, "P2-3: workflow tools") {
		t.Fatalf("workflow_board action=get broken after write registration: %s", res.ForLLM)
	}

	cardTool, ok := reg.Get(ToolNameCard)
	if !ok {
		t.Fatal("workflow_card not registered")
	}
	res = cardTool.Execute(authCtx(), map[string]any{"action": "executions", "id": float64(214)})
	if res.IsError || !strings.Contains(res.ForLLM, "wi-1") {
		t.Fatalf("workflow_card action=executions broken after write registration: %s", res.ForLLM)
	}

	// 写面同样在注册后的同名工具上可用（preview 阶段）。
	res = cardTool.Execute(identityCtx("u-integ"), map[string]any{"action": "comment", "id": float64(1), "body": "x"})
	if res.IsError || !strings.Contains(res.ForLLM, "confirm_token") {
		t.Fatalf("write preview on registered workflow_card failed: %s", res.ForLLM)
	}
}

// TestPreviewDeleteTarget_ConsistentWithExecution 高危 delete 的预览目标与
// 实际执行目标一致：board 工具省略 target → 预览「看板(board)」且执行删除
// /boards/:id；card 工具省略 target → 预览「任务卡(card)」且执行
// DELETE /cards/:id。
func TestPreviewDeleteTarget_ConsistentWithExecution(t *testing.T) {
	c, api := newRecAPI(t, nil)
	deps := newDeps("test-secret", c)

	boardTool := NewBoardWriteTool(deps)
	ctx := identityCtx("admin-b", "admin")
	token, prev := previewToken(t, boardTool, ctx, map[string]any{"action": ActionDelete, "id": float64(5)})
	if !strings.Contains(prev.Action, "看板(board)") {
		t.Fatalf("board delete preview must render the board target, got %q", prev.Action)
	}
	res := boardTool.Execute(ctx, mergeArgs(map[string]any{"action": ActionDelete, "id": float64(5)}, map[string]any{"confirm": true, "confirm_token": token}))
	if res.IsError {
		t.Fatalf("board delete execute failed: %s", res.ForLLM)
	}
	if len(api.find(http.MethodDelete, "/boards/5")) != 1 {
		t.Fatalf("board delete (target omitted) must DELETE /boards/5, records = %+v", api.writes())
	}

	c2, api2 := newRecAPI(t, nil)
	deps2 := newDeps("test-secret", c2)
	cardTool := NewCardWriteTool(deps2)
	ctx2 := identityCtx("admin-c", "admin")
	token2, prev2 := previewToken(t, cardTool, ctx2, map[string]any{"action": ActionDelete, "id": float64(9)})
	if !strings.Contains(prev2.Action, "任务卡(card)") {
		t.Fatalf("card delete preview must render the card target, got %q", prev2.Action)
	}
	res = cardTool.Execute(ctx2, mergeArgs(map[string]any{"action": ActionDelete, "id": float64(9)}, map[string]any{"confirm": true, "confirm_token": token2}))
	if res.IsError {
		t.Fatalf("card delete execute failed: %s", res.ForLLM)
	}
	if len(api2.find(http.MethodDelete, "/cards/9")) != 1 {
		t.Fatalf("card delete (target omitted) must DELETE /cards/9, records = %+v", api2.writes())
	}
}

// TestPreviewAction_BoardSemantics board 工具 create/update 的预览文案使用
// 看板语义（不得渲染成卡片文案）。
func TestPreviewAction_BoardSemantics(t *testing.T) {
	prev := PreviewAction(ToolNameBoard, ActionCreate, map[string]any{"name": "新看板"})
	if !strings.Contains(prev, "创建看板") || strings.Contains(prev, "任务卡") {
		t.Fatalf("board create preview = %q", prev)
	}
	prev = PreviewAction(ToolNameBoard, ActionUpdate, map[string]any{"id": float64(2), "name": "改名"})
	if !strings.Contains(prev, "更新看板 #2") || strings.Contains(prev, "任务卡") {
		t.Fatalf("board update preview = %q", prev)
	}
	prev = PreviewAction(ToolNameCard, ActionCreate, map[string]any{"title": "t", "list_id": float64(3)})
	if !strings.Contains(prev, "创建任务卡") {
		t.Fatalf("card create preview = %q", prev)
	}
}

// --- concurrency (P4-2 reviewer 回归，go test -race) ---------------------

// TestConcurrentConfirms_AuditIsolation 并发 confirm：每个请求的审计投递到
// 自己显式传入的 sink（per-request base-URL override），互不串扰；共享
// broker 的构造期 sink 不受影响。
func TestConcurrentConfirms_AuditIsolation(t *testing.T) {
	const n = 8
	servers := make([]*recAPI, n)
	depsList := make([]*WriteDeps, n)
	for i := 0; i < n; i++ {
		c, api := newRecAPI(t, nil)
		servers[i] = api
		depsList[i] = newDeps("test-secret", c)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			deps := depsList[i]
			tool := NewCardWriteTool(deps)
			ctx := identityCtx("user-a", "admin")
			args := map[string]any{"action": "delete", "id": float64(i + 1), "target": "card"}
			token, _ := previewTokenNoT(t, tool, ctx, args)
			res := tool.Execute(ctx, mergeArgs(args, map[string]any{"confirm": true, "confirm_token": token}))
			if res.IsError {
				t.Errorf("concurrent confirm %d failed: %s", i, res.ForLLM)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		writes := servers[i].writes()
		var audit, del int
		for _, r := range writes {
			switch {
			case r.Method == http.MethodPost && r.Path == "/audit-logs":
				audit++
			case r.Method == http.MethodDelete:
				del++
			}
		}
		if audit != 1 || del != 1 {
			t.Fatalf("server %d cross-talk: audit=%d delete=%d records=%+v", i, audit, del, writes)
		}
	}
}

// previewTokenNoT is previewToken without *testing.T for goroutine use.
func previewTokenNoT(t *testing.T, tool toolsIface, ctx context.Context, args map[string]any) (string, *workflow.Preview) {
	return previewToken(t, tool, ctx, args)
}

// --- helpers -------------------------------------------------------------

func mergeArgs(base map[string]any, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
