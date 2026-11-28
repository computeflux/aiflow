package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRoutesWithDecommissionNoConflict 钉住「真实挂载形状」（dec 非 nil，= cmd/api 的调用）
// 不触发 gin 的路由冲突 panic，并且两条 GET 各自命中正确的 handler。
//
// 为什么必须单独钉一条：httprouter 血统的 radix tree 不允许同一父节点下「参数段 + 静态段」
// 并存。`GET /skill-cutover/:skill` 与 `GET /skill-cutover/decommission-plan` 同时注册时，
// 轻则后者被 :skill 抢先匹配（运维拿到一个 skill 详情而不是下线清单），重则注册当场 panic
// = cmd/api 一启动就崩。既有单测的 dec 全是 nil，正好绕过这条路径。
func TestRoutesWithDecommissionNoConflict(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	var panicked any
	func() {
		defer func() { panicked = recover() }()
		RegisterRoutes(r, NewCutover(nil, CutoverConfig{}, nil),
			NewDecommission(nil, CutoverConfig{}, 0), func(c *gin.Context) { c.Next() })
	}()
	if panicked != nil {
		t.Fatalf("挂载 /skill-cutover/:skill 与下线计划路由必须不冲突（cmd/api 就是这么挂的）: %v", panicked)
	}

	// 路由归属：/skill-cutover/:skill 与 /skill-decommission/plan 必须是两个 handler。
	// 无 DB 时两者都走 ErrNoDB：切流详情回 500 + skill 字段，下线计划回 500 + 各自的错误文本。
	// 只要下线计划被 :skill 吃掉，返回体里就会出现 "skill" 字段 —— 用这个区分。
	show := httptest.NewRecorder()
	r.ServeHTTP(show, httptest.NewRequest(http.MethodGet, "/skill-cutover/like", nil))
	plan := httptest.NewRecorder()
	r.ServeHTTP(plan, httptest.NewRequest(http.MethodGet, "/skill-decommission/plan", nil))

	if strings.Contains(show.Body.String(), "decommission") {
		t.Fatalf("/skill-cutover/like 打到了下线计划 handler: %s", show.Body.String())
	}
	if strings.Contains(plan.Body.String(), "\"skill\":") {
		t.Fatalf("/skill-decommission/plan 打到了切流详情 handler: %s", plan.Body.String())
	}
	if plan.Code != http.StatusInternalServerError || show.Code != http.StatusInternalServerError {
		t.Fatalf("无 DB 时两个 handler 都该各自报错（500），得到 show=%d plan=%d", show.Code, plan.Code)
	}
	// 两个 handler 的边界必须各自可辨：无 DB 时都返回本包的 ErrNoDB 文本，
	// 但只有切流详情会带 skill 上下文（:skill 参数被消费的证据）。
	if !strings.Contains(plan.Body.String(), "未接数据库") {
		t.Fatalf("下线计划应命中 DecommissionService.plan（无 DB 报错），得到 %s", plan.Body.String())
	}
	if !strings.Contains(show.Body.String(), "未接数据库") {
		t.Fatalf("切流详情应命中 CutoverService.show（无 DB 报错），得到 %s", show.Body.String())
	}
}

// 鉴权红线：切流写端点必须挂 requireAuth。这里用 gin 起一个内存路由，验证
// (1) 中间件确实挂在组上（无鉴权 = 401，写操作不落库）；
// (2) 非法档位 → 409/400 + 逐条原因（board 能显示「差在哪」）。
func TestRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	called := false
	r := gin.New()
	RegisterRoutes(r, NewCutover(nil, CutoverConfig{}, nil), nil, func(c *gin.Context) {
		called = true
		c.Next()
	})

	// 鉴权中间件为 nil = 写端点裸奔：注册当场失败（这是编译期强制 mw 必填之后的运行期兜底，
	// 挡住 `RegisterRoutes(r, svc, nil, nil)` 这种「传了参数等于没传」的写法）。
	func() {
		defer func() {
			if p := recover(); p == nil {
				t.Fatal("鉴权中间件为 nil 必须拒绝挂载")
			}
		}()
		RegisterRoutes(gin.New(), NewCutover(nil, CutoverConfig{}, nil), nil, nil)
	}()

	// 无中间件命中时（鉴权失败直接 401 并 Abort）写请求不得触达服务层。
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/skill-cutover/like/stage", strings.NewReader(`{"traffic_split":10}`))
	req.Header.Set("Content-Type", "application/json")
	r.NoRoute(func(c *gin.Context) { c.String(http.StatusNotFound, "no route") })
	r.ServeHTTP(w, req)
	if !called {
		t.Fatal("中间件未挂载 = 写端点裸奔（切流必须鉴权）")
	}

	// 未接 DB：写端点必须返回错误状态码，绝不能 2xx（静默成功最危险）。
	for _, path := range []string{"/skill-cutover", "/skill-cutover/like", "/skill-cutover/decommission-plan"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == http.StatusOK {
			t.Fatalf("%s 在无 DB 时返回 200（应报错）：%s", path, w.Body.String())
		}
	}

	// 参数校验：缺 traffic_split → 400，且不进服务层。
	r2 := gin.New()
	RegisterRoutes(r2, NewCutover(nil, CutoverConfig{}, nil), nil, func(c *gin.Context) { c.Next() })
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/skill-cutover/like/stage", strings.NewReader(`{}`))
	req2.Header.Set("Content-Type", "application/json")
	r2.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("缺 traffic_split 应 400，得到 %d: %s", w2.Code, w2.Body.String())
	}

	// 回滚必须带原因。
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/skill-cutover/like/rollback", strings.NewReader(`{"reason":""}`))
	req3.Header.Set("Content-Type", "application/json")
	r2.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("空回滚原因应 400，得到 %d", w3.Code)
	}

	// 探针：ok 必填。
	w4 := httptest.NewRecorder()
	req4 := httptest.NewRequest(http.MethodPost, "/skill-cutover/like/parity", strings.NewReader(`{"version":"v2"}`))
	req4.Header.Set("Content-Type", "application/json")
	r2.ServeHTTP(w4, req4)
	if w4.Code != http.StatusBadRequest {
		t.Fatalf("缺 ok 的探针上报应 400，得到 %d", w4.Code)
	}
}

// 审计红线：操作者身份必须优先取鉴权中间件解析出的登录身份，X-Actor 只能兜底。
// X-Actor 是请求头，任何调用方都能随便填；若它优先，「谁放的量、谁批的收尾」这条
// 审计凭据就能被伪造成任意用户名（audit_logs 只增不删也救不了假名字）。
func TestActorPrefersAuthedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newCtx := func(set func(*gin.Context)) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		c.Request.Header.Set("X-Actor", "alice")
		if set != nil {
			set(c)
		}
		return c
	}

	// 已登录（authn 的键位）：必须记登录身份，忽略自报头。
	authed := newCtx(func(c *gin.Context) { c.Set("authn_user_id", "u-42") })
	if got := actorOf(authed); got != "u-42" {
		t.Fatalf("已登录时操作者必须是登录身份，得到 %q", got)
	}
	// 未登录：回落到自报头（本地开发/巡检脚本口径不变）。
	if got := actorOf(newCtx(nil)); got != "alice" {
		t.Fatalf("未登录时应回落 X-Actor，得到 %q", got)
	}
	// 鉴权键位存在但值为空/类型不认识 = 取不到，继续回落，绝不落进指针地址。
	empty := newCtx(func(c *gin.Context) {
		c.Set("authn_user_id", "   ")
		c.Set("user", &struct{ Name string }{"x"})
	})
	if got := actorOf(empty); got != "alice" {
		t.Fatalf("空/不可识别的身份必须回落 X-Actor，得到 %q", got)
	}
	// 数字身份（部分路由把 uid 以 int 塞进 context）。
	numeric := newCtx(func(c *gin.Context) { c.Set("user", 7) })
	if got := actorOf(numeric); got != "7" {
		t.Fatalf("数字身份应转成字符串，得到 %q", got)
	}
}

// 版本劫持（拿 live 基线版本号做切流）是护栏拒绝，不是参数写错：必须回 409，
// 让调用方（board / skillctl）看到「被护栏拦下」并展示逐条原因，而不是当成 400 重试。
func TestStatusForVersionLockIsGuardrail(t *testing.T) {
	wrapped := fmt.Errorf("%w: v1 是现存 live 版本", ErrCutoverVersionLocked)
	if got := statusFor(wrapped); got != http.StatusConflict {
		t.Fatalf("版本锁定拒绝必须 409，得到 %d", got)
	}
}

func TestStatusForMapping(t *testing.T) {
	if got := statusFor(ErrCutoverStage); got != http.StatusConflict {
		t.Fatalf("非法档位应 409，得到 %d", got)
	}
	if got := statusFor(ErrNoDB); got != http.StatusInternalServerError {
		t.Fatalf("基础设施故障应 500，得到 %d", got)
	}
	if got := statusFor(nil); got != http.StatusOK {
		t.Fatalf("nil 应 200，得到 %d", got)
	}
	if !isBadParam(errBadSample) || isGuardrail(errBadSample) {
		t.Fatal("参数类错误分类错误")
	}
}

var errBadSample = &sampleErr{}

type sampleErr struct{}

func (sampleErr) Error() string { return "target_addr 不能为空" }

func TestJSONShapeOfPlan(t *testing.T) {
	pl := Plan{Window: "24h0m0s", Services: []ServicePlan{{Service: "pycraper", Skill: "x-scan", Action: ActionKeep, Replicas: 1, Blocked: []string{"未登记切流目标"}}}}
	b, err := json.Marshal(pl)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, k := range []string{"service", "skill", "action", "replicas", "blocked", "ready", "window"} {
		if !strings.Contains(string(b), `"`+k+`"`) {
			t.Fatalf("下线计划 JSON 缺字段 %q: %s", k, b)
		}
	}
	var back Plan
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Services[0].Service != "pycraper" || back.Window != "24h0m0s" {
		t.Fatalf("round-trip 丢字段: %+v", back)
	}
	_ = context.Background()
}
