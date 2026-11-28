// social_invoker_test.go：MIG-1（工作项 wi-1795853890）「skill 网关 x-scan / like 的
// addr 切到 social」的网关侧单测。
//
// 覆盖验收标准里可在单测里跑的部分：
//   - addr 判定：哪些 addr 走 social 转发、哪些仍走旧的 /invoke 直连（切流过程中两版并存）；
//   - 路由映射：x-scan → GET /api/social/x/search?mode=user、like → POST /api/social/x/interact；
//   - 响应归一：social 的 posts[] / interact 结果 → skill output_schema 的字段；
//   - 端到端：httptest 起一个假 social，跑完整 invoke 分派，断言输出满足两份 skill.json
//     的 output_schema（读仓库里的真实文件；核心模块被单独检出时退化到内嵌契约副本）。
//
// 主机名约定：isSocialAddr 认 compose 服务名 social，以及环回 / host.docker.internal
// （本地与冒烟环境起假 social 的形态）。因此本文件的端到端用例必须让 `social` 能解析到
// 环回 —— 由 package TestMain 写入 /etc/hosts（写不进去就显式跳过那两条用例，绝不静默）。
// MySQL / Redis / Qdrant / 浏览器一个都不起。
package skillgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain 让端到端用例要用的两个主机名在测试进程里可解析（见文件头的主机名约定）。
//
// 不写 /etc/hosts：那是进程外的全局状态，测试容器里通常只读挂载、宿主上跑测试更不该被改。
// 正解是注入 transport（SetServiceTransportForTest）：在拨号前把主机名换成环回 + 对应端口。
// 于是「按主机名分派」的判定链（isSocialAddr → socialBaseOf → invokeSocial / invokeService）
// 与线上完全同构 —— addr 里写的就是 social:<port>，请求最终打到哪台服务器由分派代码决定。
func TestMain(m *testing.M) {
	socialSrv = httptest.NewServer(http.HandlerFunc(serveSocialAlias))
	legacySrv = httptest.NewServer(http.HandlerFunc(serveLegacySkill))
	SetServiceTransportForTest(aliasTransport(map[string]string{
		"social":            portOf(socialSrv),
		"legacy-skill-host": portOf(legacySrv),
	}))
	code := m.Run()
	SetServiceTransportForTest(nil)
	socialSrv.Close()
	legacySrv.Close()
	os.Exit(code)
}

var (
	socialSrv *httptest.Server
	legacySrv *httptest.Server

	socialMu      sync.Mutex
	currentSocial *fakeSocial
)

// serveSocialAlias 是 social 主机名背后的分发器：转给当前用例注册的 fakeSocial。
func serveSocialAlias(w http.ResponseWriter, r *http.Request) {
	socialMu.Lock()
	fs := currentSocial
	socialMu.Unlock()
	if fs == nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("本用例未注册假 social"))
		return
	}
	fs.handler().ServeHTTP(w, r)
}

// 旧实现（pycraper / pyreplay 的 skill server）的端口承接：记录请求，返回 x-scan 的原始契约。
var (
	legacyHits  atomic.Int64
	legacyPaths []string
	legacyMu    sync.Mutex
)

func serveLegacySkill(w http.ResponseWriter, r *http.Request) {
	legacyMu.Lock()
	legacyHits.Add(1)
	legacyPaths = append(legacyPaths, r.Method+" "+r.URL.Path)
	legacyMu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "handle": "a", "count": 0, "items": []any{}})
}

func portOf(srv *httptest.Server) string {
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		panic("httptest 地址异常: " + srv.URL)
	}
	return port
}

// aliasTransport 构造「主机名 → 环回」的重定向 transport：白名单外的主机名一律拒绝拨号
// （单测里出现真实外联就说明用例写坏了，必须失败在测试里而不是依赖 DNS）。
// 端口原样沿用 addr 里的端口 —— 用例的 addr 端口就是它自己要打的那台服务器。
func aliasTransport(ports map[string]string) http.RoundTripper {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("aliasTransport: %w", err)
			}
			// 端口来自 addr 本身（用例的 addr 端口就是它自己那台服务器的端口），
			// 这里只把主机名收敛到环回；白名单外的主机名一律拒绝 —— 单测里出现真实外联
			// （去解析 pycraper）就说明用例写坏了，必须失败在测试里而不是依赖 DNS。
			if _, ok := ports[host]; !ok {
				return nil, fmt.Errorf("aliasTransport: 测试环境拒绝解析主机名 %q", host)
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
}

func TestIsSocialAddr(t *testing.T) {
	social := []string{
		"http://social:8010",
		"http://social:8010/",
		SocialXScanAddr,
		SocialLikeAddr,
		"SOCIAL:8010/like", // 漏 scheme + 大写主机名（运维手填的典型形态）
	}
	for _, a := range social {
		if !isSocialAddr(a) {
			t.Fatalf("%q 必须判定为 social（切流目标）", a)
		}
	}
	legacy := []string{
		"http://pycraper:9504",
		"http://pyreplay:9500/like",
		"http://skill-like-1.4.1:19501",
		"",
		"not-a-url",
	}
	for _, a := range legacy {
		if isSocialAddr(a) {
			t.Fatalf("%q 必须判定为旧实现（走原 /invoke 直连）", a)
		}
	}
}

func TestCutoverTargetAddrs(t *testing.T) {
	if got := CutoverTargetForSkill(SkillXScan); got != SocialXScanAddr {
		t.Fatalf("x-scan 的切流目标 = %q，want %q", got, SocialXScanAddr)
	}
	if got := CutoverTargetForSkill(SkillLike); got != SocialLikeAddr {
		t.Fatalf("like 的切流目标 = %q，want %q", got, SocialLikeAddr)
	}
	if got := CutoverTargetForSkill("generate"); got != "" {
		t.Fatalf("非收口范围的 skill 不该有切流目标，得到 %q", got)
	}
	if got := LegacyServiceForSkill(SkillXScan); got != "pycraper" {
		t.Fatalf("x-scan 的旧服务 = %q，want pycraper", got)
	}
	if got := LegacyServiceForSkill(SkillLike); got != "pyreplay" {
		t.Fatalf("like 的旧服务 = %q，want pyreplay", got)
	}
	if got := LegacyServiceForSkill("generate"); got != "" {
		t.Fatalf("非收口范围的 skill 不该有旧服务，得到 %q", got)
	}
}

func TestClampScanLimit(t *testing.T) {
	for n, want := range map[int]int{0: 10, -5: 10, 1: 1, 10: 10, 50: 50, 51: 50, 5000: 50} {
		if got := clampScanLimit(n); got != want {
			t.Fatalf("clampScanLimit(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestXExternalID(t *testing.T) {
	for id, want := range map[string]string{
		"123":                         "x_123",
		"x_123":                       "x_123",
		" 456 ":                       "x_456",
		"":                            "",
		"https://x.com/a/status/1234": "x_1234", // 推文 URL 取 /status/<id>（与 social 侧 normalize 同源）
	} {
		if got := xExternalID(id); got != want {
			t.Fatalf("xExternalID(%q) = %q, want %q", id, got, want)
		}
	}
}

// fakeSocial 是一个最小 social 服务：只实现 x-scan / like 两条渠道路由，
// 并按 apps/social 的真实响应形状返回（posts[] / interact 的 Raw）。
type fakeSocial struct {
	hits       atomic.Int64
	lastMethod string
	lastPath   string
	lastQuery  url.Values
	lastBody   map[string]any
	scanErr    int // >0 时搜索端点返回该状态码
}

// socialInvoke 用「主机名 → 环回」的 transport 调 invokeSocial（addr 里的端口就是当前用例
// 那台假 social 的端口，见 socialTestAddr）。
func socialInvoke(t *testing.T, ctx context.Context, skillName, addr string, input map[string]any) (map[string]any, error) {
	t.Helper()
	return invokeSocialWithTransport(ctx, aliasTransport(map[string]string{"social": "any"}), skillName, addr, input)
}

func (f *fakeSocial) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/api/social/", func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimSuffix(r.URL.Path, "/") {
		case "/api/social/x/search":
			f.serveSearch(w, r)
		case "/api/social/x/interact":
			f.serveInteract(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("not found: " + r.URL.Path))
		}
	})
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 转发拼错路径（典型：多拼一段 /invoke）会落到这里 —— 必须显式 404，
		// 让「路由映射写错」在测试里必红，而不是被静默的成功响应掩盖。
		f.lastMethod, f.lastPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such route: " + r.URL.Path))
	})
	return m
}

func (f *fakeSocial) serveSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	f.hits.Add(1)
	f.lastMethod, f.lastPath, f.lastQuery = r.Method, r.URL.Path, r.URL.Query()
	if f.scanErr > 0 {
		w.WriteHeader(f.scanErr)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "boom"})
		return
	}
	limit := f.lastQuery.Get("limit")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keyword":    f.lastQuery.Get("keyword"),
		"count":      2,
		"items":      []map[string]any{{"id": "111", "url": "https://x.com/a/status/111", "title": "hello", "author": "alice"}, {"id": "x_222", "url": "https://x.com/a/status/222", "title": "world", "author": "alice"}},
		"limit_sent": limit,
	})
}

func (f *fakeSocial) serveInteract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	f.hits.Add(1)
	f.lastMethod, f.lastPath = r.Method, r.URL.Path
	body := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.lastBody = body
	_ = json.NewEncoder(w).Encode(map[string]any{
		"channel": "x",
		"action":  "like",
		"id":      body["video_id"],
		"raw": map[string]any{
			"results": []map[string]any{
				{"tweet": body["video_id"], "liked": true},
				{"tweet": body["video_id"], "liked": false, "reason": "like 按钮未出现"},
			},
			"success": 1,
			"total":   2,
		},
	})
}

// TestInvokeSocialXScan 覆盖 x-scan 的转发与归一（含「切流写入的 addr 带能力路径段」
// 这一条：http://social:8010/x-scan 必须打到基址下的 /api/social/x/search）。
func TestInvokeSocialXScan(t *testing.T) {
	fs := &fakeSocial{}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	useSocialServer(t, fs, srv)

	out, err := socialInvoke(t, context.Background(), SkillXScan, socialTestAddr(srv, "x-scan"), map[string]any{
		"handle": "@computefluxAI", "limit": json.Number("3"),
	})
	if err != nil {
		t.Fatalf("invokeSocial(x-scan): %v", err)
	}
	if fs.lastMethod != http.MethodGet || fs.lastPath != "/api/social/x/search" {
		t.Fatalf("x-scan 必须转发到 GET /api/social/x/search，得到 %s %s", fs.lastMethod, fs.lastPath)
	}
	if got := fs.lastQuery.Get("mode"); got != "user" {
		t.Fatalf("x-scan 必须用 mode=user（用户主页），得到 %q", got)
	}
	if got := fs.lastQuery.Get("keyword"); got != "computefluxAI" {
		t.Fatalf("handle 的 @ 前缀必须去掉，得到 %q", got)
	}
	if got := fs.lastQuery.Get("limit"); got != "3" {
		t.Fatalf("limit 必须透传，得到 %q", got)
	}

	assertScanOutput(t, out)
	// 转发目标必须是基址下的路由：残留的能力路径段（/x-scan）会拼出 404 路径。
	if !strings.HasSuffix(fs.lastPath, "/api/social/x/search") || strings.Contains(fs.lastPath, "/x-scan/") {
		t.Fatalf("转发路径必须是基址下的 /api/social/x/search，得到 %q", fs.lastPath)
	}
	if got := out["handle"]; got != "computefluxAI" {
		t.Fatalf("handle 必须回填（pycraper 契约字段），得到 %v", got)
	}
}

func TestInvokeSocialXScanLimitClamped(t *testing.T) {
	fs := &fakeSocial{}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	useSocialServer(t, fs, srv)

	if _, err := socialInvoke(t, context.Background(), SkillXScan, socialTestAddr(srv, "x-scan"), map[string]any{"handle": "a"}); err != nil {
		t.Fatalf("invokeSocial: %v", err)
	}
	if got := fs.lastQuery.Get("limit"); got != "10" {
		t.Fatalf("缺省 limit 必须是 10（pycraper 口径），得到 %q", got)
	}
}

// socialTestAddr 把 httptest 的环回基址伪装成 social 的能力 addr：
// http://127.0.0.1:PORT/x-scan → http://social:PORT/x-scan（主机名换成 compose 服务名，
// 端口沿用 httptest 分配的）。生产判定只认主机名 social（见 socialBaseOf），端口不参与判定，
// 所以这样构造出来的 addr 与线上切流写入的形态完全同构。
func socialTestAddr(srv *httptest.Server, capability string) string {
	return hostAddr(srv, "social", capability)
}

// useSocialServer 把 TestMain 起的假 social 接到当前用例上（transport 把 social:<port>
// 拨到环回，端口取自 addr 本身，所以每个用例都能指向自己那台服务器）。
func useSocialServer(t *testing.T, fs *fakeSocial, srv *httptest.Server) {
	t.Helper()
	socialMu.Lock()
	currentSocial = fs
	socialMu.Unlock()
	t.Cleanup(func() {
		socialMu.Lock()
		currentSocial = nil
		socialMu.Unlock()
	})
}

// hostAddr 把 httptest 服务器的端口与指定主机名拼成 addr（+ 能力路径段），
// 形状与切流写入 skill_versions.config.addr 的完全一致。
func hostAddr(srv *httptest.Server, host, capability string) string {
	return "http://" + host + ":" + portOf(srv) + "/" + capability
}

func TestInvokeSocialLike(t *testing.T) {
	fs := &fakeSocial{}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	useSocialServer(t, fs, srv)

	out, err := socialInvoke(t, context.Background(), SkillLike, socialTestAddr(srv, "like"), map[string]any{
		"post_url": "https://x.com/a/status/1234", "like_count": json.Number("2"),
	})
	if err != nil {
		t.Fatalf("invokeSocial(like): %v", err)
	}
	if fs.lastMethod != http.MethodPost || fs.lastPath != "/api/social/x/interact" {
		t.Fatalf("like 必须转发到 POST /api/social/x/interact，得到 %s %s", fs.lastMethod, fs.lastPath)
	}
	if got := fs.lastBody["action"]; got != "like" {
		t.Fatalf("互动动作必须是 like，得到 %v", got)
	}
	if got := fs.lastBody["video_id"]; got != "https://x.com/a/status/1234" {
		t.Fatalf("post_url 必须作为推文目标传下去，得到 %v", got)
	}

	assertLikeOutput(t, out)
	if got := out["success_count"]; got != 1 {
		t.Fatalf("success_count 必须来自 social 的计数，得到 %v", out["success_count"])
	}
}

func TestInvokeSocialErrors(t *testing.T) {
	fs := &fakeSocial{scanErr: http.StatusBadGateway}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	useSocialServer(t, fs, srv)

	if _, err := socialInvoke(t, context.Background(), SkillXScan, socialTestAddr(srv, "x-scan"), map[string]any{"handle": "a"}); err == nil ||
		!strings.Contains(err.Error(), "502") {
		t.Fatalf("social 非 200 必须上抛带状态码的错误，得到 %v", err)
	}
	if _, err := socialInvoke(t, context.Background(), "generate", "http://social:8010", nil); err == nil {
		t.Fatal("非收口范围的 skill 不该有 social 路由映射")
	}
	if _, err := socialInvoke(t, context.Background(), SkillLike, "  ", nil); err == nil {
		t.Fatal("addr 为空必须报错（不能拼出相对路径打到网关自己身上）")
	}
}

// TestInvokeDispatchesByAddr 钉住 runner 的分派：service 型 + social addr → social 转发；
// service 型 + 旧 addr → 原 /invoke 直连。切流期间两版并存，分派错一支就整档断流。
func TestInvokeDispatchesByAddr(t *testing.T) {
	fs := &fakeSocial{}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	useSocialServer(t, fs, srv)

	if _, err := invoke(context.Background(),
		&SkillSpec{Name: SkillXScan, Type: "service", Addr: socialTestAddr(srv, "x-scan")},
		map[string]any{"handle": "a"}, aliasTransport(map[string]string{"social": "any"})); err != nil {
		t.Fatalf("social addr 必须走 social 转发: %v", err)
	}
	if fs.hits.Load() == 0 {
		t.Fatal("social 转发未发生")
	}

	legacyBefore := legacyHits.Load()
	if _, err := invoke(context.Background(),
		&SkillSpec{Name: SkillXScan, Type: "service", Addr: hostAddr(legacySrv, "legacy-skill-host", "scan")},
		map[string]any{"handle": "a"}, aliasTransport(map[string]string{"legacy-skill-host": "any"})); err != nil {
		t.Fatalf("旧 addr 必须走 /invoke 直连: %v", err)
	}
	if legacyHits.Load() != legacyBefore+1 {
		t.Fatalf("旧实现应被调用一次，得到 %d", legacyHits.Load()-legacyBefore)
	}
	legacyMu.Lock()
	paths := append([]string(nil), legacyPaths...)
	legacyMu.Unlock()
	// 旧链路 = invokeService：往 addr 原样拼 /invoke（部署时 addr = http://pycraper:9504/scan，
	// 所以线上真实路径是 /scan/invoke；S5 灰度版本实例同理）。
	if len(paths) != 1 || !strings.HasSuffix(paths[0], "/invoke") || !strings.HasPrefix(paths[0], "POST ") {
		t.Fatalf("旧实现必须打 POST <addr 路径>/invoke，得到 %v", paths)
	}
}

// TestSocialOutputMatchesSkillSchemas 用仓库里真实的两份 skill.json 校验归一后的输出：
// schema 与归一逻辑的口径一旦漂移（改了 skill.json 没改归一，或反之），这里立刻红。
// schema 与归一逻辑的口径一旦漂移（改了 skill.json 没改归一，或反之），这里立刻红。
func TestSocialOutputMatchesSkillSchemas(t *testing.T) {
	scanRaw, likeRaw := loadLegacySkillSpecs(t)

	scanSpec := parseSchemaOf(t, scanRaw, "output_schema")
	likeSpec := parseSchemaOf(t, likeRaw, "output_schema")

	scanOut := normalizeScanOutput(map[string]any{
		"keyword": "computefluxAI",
		"count":   json.Number("2"),
		"items": []any{
			map[string]any{"id": "111", "url": "u1", "title": "hello", "author": "alice"},
		},
	})
	if err := validateAgainst(schemaURLOutput, scanSpec, scanOut); err != nil {
		t.Fatalf("x-scan 归一输出不符合 apps/pycraper/skill.json 的 output_schema: %v\n输出: %+v", err, scanOut)
	}

	likeOut := normalizeLikeOutput(map[string]any{
		"channel": "x", "action": "like", "id": "https://x.com/a/status/1234",
		"raw": map[string]any{
			"results": []any{map[string]any{"tweet": "1234", "liked": true}},
			"success": json.Number("1"), "total": json.Number("1"),
		},
	})
	if err := validateAgainst(schemaURLOutput, likeSpec, likeOut); err != nil {
		t.Fatalf("like 归一输出不符合 apps/pyreplay/skill.json 的 output_schema: %v\n输出: %+v", err, likeOut)
	}
}

func assertScanOutput(t *testing.T, out map[string]any) {
	t.Helper()
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("x-scan 归一后顶层 ok 必须为 true，得到 %v", out["ok"])
	}
	itemsRaw, ok := out["items"].([]any)
	if !ok || len(itemsRaw) != 2 {
		t.Fatalf("items 必须归一成 2 条，得到 %T %v", out["items"], out["items"])
	}
	items := toMaps(t, itemsRaw)
	if !ok || len(items) != 2 {
		t.Fatalf("items 必须归一成 2 条，得到 %T %v", out["items"], out["items"])
	}
	if got := items[0]["external_id"]; got != "x_111" {
		t.Fatalf("external_id 必须是 pycraper 的 x_<id> 口径，得到 %v", got)
	}
	if got := items[1]["external_id"]; got != "x_222" {
		t.Fatalf("已带前缀的 id 不能重复加前缀，得到 %v", got)
	}
	if got := items[0]["content"]; got != "hello" {
		t.Fatalf("正文必须从 social 的 title/text 映射到 content，得到 %v", got)
	}
	if got := out["count"]; got != 2 {
		t.Fatalf("count 必须是整数 2，得到 %T %v", out["count"], out["count"])
	}
}

func assertLikeOutput(t *testing.T, out map[string]any) {
	t.Helper()
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("like 归一后顶层 ok 必须为 true，得到 %v", out["ok"])
	}
	resultsRaw, ok := out["results"].([]any)
	if !ok || len(resultsRaw) != 2 {
		t.Fatalf("results 必须归一成 2 条，得到 %T %v", out["results"], out["results"])
	}
	results := toMaps(t, resultsRaw)
	if results[0]["success"] != true || results[1]["success"] != false {
		t.Fatalf("liked 必须映射成 success，得到 %+v / %+v", results[0], results[1])
	}
	if got := results[1]["error"]; got != "like 按钮未出现" {
		t.Fatalf("失败原因必须落到 error 字段，得到 %v", got)
	}
	if got := results[0]["tweet_id"]; got != "x_1234" {
		t.Fatalf("tweet_id 必须是 x_<id> 口径（推文 URL 取尾部 ID 由 social 侧完成），得到 %v", got)
	}
	if got := out["attempts"]; got != 2 {
		t.Fatalf("attempts 必须来自 social 的 total，得到 %v", got)
	}
}

// repoRoot 从测试文件位置回溯到仓库根（apps/core/internal/skillgateway → 仓库根）。
//
// 必须从测试文件位置回溯（os.Getwd 在 CI 的 ci-runner 里是容器内的模块根 /src，向上三级
// 会跑到 /，两份 skill.json 一律读不到；runtime.Caller 给出源文件的真实绝对路径）。
//
// 必须从测试文件位置回溯（os.Getwd 在 CI 的 ci-runner 里是容器内的模块根 /src，向上三级
// 会跑到 /）。本仓库的目录深度固定（apps/core/internal/skillgateway → 仓库根），所以直接
// 上跳三级；找不到时由调用方退化到内嵌契约副本（不 fatal）：ci-runner 只把 apps/core 挂进
// 容器，完整仓库（本地 / evolve worktree / 全仓 CI）能读到真实 skill.json。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 取不到测试文件路径")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
}

// skillJSONPath 返回某旧服务的 skill.json（收口前的契约定义所在）。
func skillJSONPath(t *testing.T, service string) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "apps", service, "skill.json")
}

// loadLegacySkillSpecs 读 pycraper / pyreplay 两份 skill.json 原文。
//
// 优先读仓库里的真实文件（改了 skill.json 忘了改归一 → 这里立刻红）；文件不在（CI 的
// ci-runner 只把 apps/core 挂进容器）时退化为内嵌副本，并显式打印退化事实（不静默）。
func loadLegacySkillSpecs(t *testing.T) (scan []byte, like []byte) {
	t.Helper()
	scan = readSkillSpec(t, "pycraper", embeddedScanSkillJSON)
	like = readSkillSpec(t, "pyreplay", embeddedLikeSkillJSON)
	return scan, like
}

func readSkillSpec(t *testing.T, service, fallback string) []byte {
	t.Helper()
	path := skillJSONPath(t, service)
	if b, err := os.ReadFile(path); err == nil {
		return b
	}
	t.Logf("读不到 %s，退化用内嵌契约副本（完整仓库里跑会读到真实文件）", path)
	return []byte(fallback)
}

// embeddedScanSkillJSON / embeddedLikeSkillJSON 是两份 skill.json 的 output_schema 副本，
// 只在「核心模块被单独检出运行测试」的环境里兜底。改任一真实 skill.json 的 output_schema
// 时，必须同步这里 —— 完整仓库跑测试时读的是真实文件，不一致会在上一行的读取路径上暴露。
const embeddedScanSkillJSON = `{
  "name": "x-scan",
  "version": "1.0.0",
  "owner": "workflow",
  "runtime": "python",
  "type": "service",
  "addr": "http://pycraper:9504",
  "input_schema": {"type": "object", "required": ["handle"], "properties": {"handle": {"type": "string"}}},
  "output_schema": {
    "type": "object",
    "required": ["ok"],
    "properties": {
      "ok": {"type": "boolean"},
      "reason": {"type": "string"},
      "handle": {"type": "string"},
      "count": {"type": "integer"},
      "items": {"type": "array", "items": {"type": "object"}}
    }
  },
  "limits": {"timeout_sec": 300, "rate_per_min": 2, "max_concurrency": 1},
  "permissions": null
}`

const embeddedLikeSkillJSON = `{
  "name": "like",
  "version": "1.0.0",
  "owner": "workflow",
  "runtime": "python",
  "type": "service",
  "addr": "http://pyreplay:9500",
  "input_schema": {"type": "object", "required": ["post_url"], "properties": {"post_url": {"type": "string"}}},
  "output_schema": {
    "type": "object",
    "required": ["ok"],
    "properties": {
      "ok": {"type": "boolean"},
      "reason": {"type": "string"},
      "tweet_id": {"type": "string"},
      "attempts": {"type": "integer"},
      "success_count": {"type": "integer"},
      "results": {"type": "array", "items": {"type": "object"}}
    }
  },
  "limits": {"timeout_sec": 180, "rate_per_min": 2, "max_concurrency": 1},
  "permissions": null
}`

// toMaps 把 []any 收成 []map[string]any（断言用；元素类型不对直接失败）。
func toMaps(t *testing.T, rows []any) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("归一后的元素必须是 JSON object，得到 %T", r)
		}
		out = append(out, m)
	}
	return out
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s: %v", path, err)
	}
	return b
}

// parseSchemaOf 从 skill.json 原文里取一份 schema（原样 RawMessage，交给同一套编译器校验）。
func parseSchemaOf(t *testing.T, raw []byte, field string) json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("skill.json 不是合法 JSON: %v", err)
	}
	sch, ok := doc[field]
	if !ok {
		t.Fatalf("skill.json 缺少 %s 字段", field)
	}
	return sch
}

// TestSocialBaseOf 钉住「social 基址收敛」：切流写入的 addr 带能力路径段
// （http://social:8010/x-scan），转发目标必须落在这个基址之下。
//
// 这条判定是切流放量的第一道关：基址算错（多一段 /x-scan）会让每一笔调用打向不存在的
// 路径，整档流量全挂；把旧实现的 addr 误判成 social 则同理。
func TestSocialBaseOf(t *testing.T) {
	cases := map[string]string{
		SocialXScanAddr:       SocialAddr,
		SocialLikeAddr:        SocialAddr,
		"http://social:8010/": SocialAddr,
		"http://social:8010":  SocialAddr,
		"SOCIAL:8010/like":    "http://SOCIAL:8010",
		// 旧实现与灰度版本实例：一律不参与 social 转发（走原 /invoke 直连）。
		"http://pycraper:9504/scan":     "",
		"http://pyreplay:9500/like":     "",
		"http://skill-like-1.4.1:19501": "",
		"http://127.0.0.1:9500/like":    "http://127.0.0.1:9500",
		"not-a-url":                     "",
		"":                              "",
	}
	for in, want := range cases {
		if got := socialBaseOf(in); got != want {
			t.Fatalf("socialBaseOf(%q) = %q, want %q", in, got, want)
		}
	}
}
