package agentcli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestPiWeb 起一个假 pi-web（记录请求），返回客户端与请求记录。
func newTestPiWeb(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, body string)) (*piWeb, *[]string) {
	t.Helper()
	reqs := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*reqs = append(*reqs, r.Method+" "+r.URL.Path+" "+string(b))
		handler(w, r, string(b))
	}))
	t.Cleanup(srv.Close)
	return &piWeb{base: srv.URL, password: "pw", client: &http.Client{Timeout: 5 * time.Second}}, reqs
}

func okNew(sessionID string) func(http.ResponseWriter, *http.Request, string) {
	return func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"sessionId":"` + sessionID + `"}`))
	}
}

// TestSubmitNewSession 新建会话：完整提示词 + cwd + provider/modelId。
func TestSubmitNewSession(t *testing.T) {
	client, reqs := newTestPiWeb(t, okNew("S-new"))
	env := PiEnv{SessionDir: t.TempDir(), Provider: "local", Model: "Qwen3.8-27B"}
	p := &protoIO{stdout: &strings.Builder{}, stderr: &strings.Builder{}}

	got, err := p.submit(context.Background(), client, env, FixInput{
		Op: "fix", Task: "实现消息中心", Kind: "requirement", Files: []string{"apps/actives/messages.go"},
		Workdir: "/repo/cache/evolve/EV-1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got != "S-new" {
		t.Fatalf("sessionID=%q，期望 S-new", got)
	}
	if len(*reqs) != 1 || !strings.HasPrefix((*reqs)[0], "POST /api/agent/new ") {
		t.Fatalf("应只调用 /api/agent/new: %v", *reqs)
	}
	var body map[string]any
	raw := strings.TrimPrefix((*reqs)[0], "POST /api/agent/new ")
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, raw)
	}
	if body["cwd"] != "/repo/cache/evolve/EV-1" || body["provider"] != "local" || body["modelId"] != "Qwen3.8-27B" {
		t.Fatalf("cwd/provider/modelId 错误: %v", body)
	}
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "正在为一个项目实现一个产品功能") || !strings.Contains(msg, "实现消息中心") {
		t.Fatalf("新建会话应发完整提示词: %q", msg)
	}
}

// TestSubmitFollowUp 续接会话：只发上一轮反馈（followUp 排队语义）。
func TestSubmitFollowUp(t *testing.T) {
	client, reqs := newTestPiWeb(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	env := PiEnv{SessionDir: t.TempDir(), Provider: "local", Model: "m"}
	p := &protoIO{stdout: &strings.Builder{}, stderr: &strings.Builder{}}

	got, err := p.submit(context.Background(), client, env, FixInput{
		Op: "fix", Task: "实现消息中心", SessionID: "S-1", Feedback: "\n\n上一轮 reviewer 的 error 级评论：\n- a.go:1 并发问题",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got != "S-1" {
		t.Fatalf("续接应沿用会话 ID，得到 %q", got)
	}
	if len(*reqs) != 1 || !strings.HasPrefix((*reqs)[0], "POST /api/agent/S-1 ") {
		t.Fatalf("应只调用 /api/agent/S-1: %v", *reqs)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(strings.TrimPrefix((*reqs)[0], "POST /api/agent/S-1 ")), &body)
	if body["streamingBehavior"] != "followUp" {
		t.Fatalf("续接必须用 followUp（agent 在跑时排队而非拒绝）: %v", body)
	}
	msg, _ := body["message"].(string)
	if !strings.HasPrefix(msg, "上一轮 reviewer 的 error 级评论：\n- a.go:1 并发问题") {
		t.Fatalf("续接应以反馈正文开头：%q", msg)
	}
	if !strings.Contains(msg, "工作目录（本轮强制）") {
		t.Fatalf("续接消息必须携带工作目录锚定守卫：%q", msg)
	}
}

// TestSubmitFollowUpDedup 最后一条 user 消息 == 本次反馈（已排队/已处理）→ fail-fast：
// 续接轮的 cwd 锚定靠新发的 followUp 消息携带，去重命中说明本轮无法锚定 → 不能盲等。
func TestSubmitFollowUpDedup(t *testing.T) {
	client, reqs := newTestPiWeb(t, okNew("should-not-happen"))
	dir := t.TempDir()
	feedback := "上一轮没有修改任何文件，请实际修改代码。"
	writeSession(t, dir, "S-dup",
		`{"type":"message","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"done"}]}}`,
		`{"type":"message","message":{"role":"user","content":`+strconv.Quote(BuildContinuePrompt(feedback)+WorkdirGuard(""))+`}}`,
	)
	env := PiEnv{SessionDir: dir, Provider: "local", Model: "m"}
	p := &protoIO{stdout: &strings.Builder{}, stderr: &strings.Builder{}}

	_, err := p.submit(context.Background(), client, env, FixInput{
		Op: "fix", Task: "t", SessionID: "S-dup", Feedback: feedback,
	})
	// 去重基准 = BuildContinuePrompt(feedback) + WorkdirGuard("")，会话里预置同样内容命中去重
	if err == nil || !strings.Contains(err.Error(), "already queued") {
		t.Fatalf("去重命中应 fail-fast，得到 %v", err)
	}
	if len(*reqs) != 0 {
		t.Fatalf("去重时不应发任何请求: %v", *reqs)
	}
}

// TestSubmitFollowUpProcessing 持续 "already processing"（重试次数用尽）→ 沿用会话，不新建
// （新建会丢上下文，且同一任务出现两个 pi-web 会话）。
func TestSubmitFollowUpProcessing(t *testing.T) {
	oldRetries := piFollowUpRetries
	piFollowUpRetries = 0
	t.Cleanup(func() { piFollowUpRetries = oldRetries })

	client, reqs := newTestPiWeb(t, func(w http.ResponseWriter, _ *http.Request, _ string) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"error":"agent already processing"}`))
	})
	env := PiEnv{SessionDir: t.TempDir(), Provider: "local", Model: "m"}
	p := &protoIO{stdout: &strings.Builder{}, stderr: &strings.Builder{}}

	got, err := p.submit(context.Background(), client, env, FixInput{Op: "fix", Task: "t", SessionID: "S-busy", Feedback: "继续"})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got != "S-busy" {
		t.Fatalf("应沿用会话 ID，得到 %q", got)
	}
	if len(*reqs) != 1 {
		t.Fatalf("不应回退新建会话: %v", *reqs)
	}
}

// TestSubmitFollowUpFallback session 在 pi-web 不存在（driver 预生成 id）→ 回退新建（完整提示词）。
func TestSubmitFollowUpFallback(t *testing.T) {
	client, reqs := newTestPiWeb(t, func(w http.ResponseWriter, r *http.Request, _ string) {
		if r.URL.Path == "/api/agent/new" {
			okNew("S-fallback")(w, r, "")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"error":"Session not found"}`))
	})
	env := PiEnv{SessionDir: t.TempDir(), Provider: "local", Model: "m"}
	p := &protoIO{stdout: &strings.Builder{}, stderr: &strings.Builder{}}

	got, err := p.submit(context.Background(), client, env, FixInput{
		Op: "fix", Task: "实现功能", SessionID: "S-stale", Feedback: "继续", Workdir: "/repo/cache/evolve/EV-1",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if got != "S-fallback" {
		t.Fatalf("应回退新建会话，得到 %q", got)
	}
	if len(*reqs) != 2 || !strings.Contains((*reqs)[0], "/api/agent/S-stale") || !strings.Contains((*reqs)[1], "/api/agent/new") {
		t.Fatalf("请求序列错误: %v", *reqs)
	}
	if !strings.Contains((*reqs)[1], "正在修复一个生产 bug") || !strings.Contains((*reqs)[1], "实现功能") {
		t.Fatalf("回退新建应发完整提示词: %v", (*reqs)[1])
	}
}

// TestRunPiValidations 参数校验快速失败（协议 JSON + 错误），不触网。
func TestRunPiValidations(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"op 不支持", `{"op":"review","workdir":"/tmp"}`, "unsupported op"},
		{"缺 task", `{"op":"fix","workdir":"/tmp"}`, "missing task"},
		{"缺 workdir", `{"op":"fix","task":"t"}`, "missing workdir"},
		{"workdir 不存在", `{"op":"fix","task":"t","workdir":"/does/not/exist"}`, "workdir not found"},
		{"输入非 JSON", `not json`, "input not JSON"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			err := RunPi(strings.NewReader(tc.input), &stdout, &stderr)
			if err == nil {
				t.Fatalf("应失败")
			}
			if !strings.Contains(stdout.String(), tc.want) {
				t.Errorf("stdout=%q 应含 %q", stdout.String(), tc.want)
			}
			if !strings.HasPrefix(strings.TrimSpace(stdout.String()), `{"ok":false`) {
				t.Errorf("失败输出应是协议 JSON: %q", stdout.String())
			}
		})
	}
}

// TestRunPiProviderRequired 缺 PI_PROVIDER/PI_MODEL 快速失败（确定性运行，不猜默认值）。
func TestRunPiProviderRequired(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_PROVIDER", "")
	t.Setenv("PI_MODEL", "")
	var stdout, stderr strings.Builder
	err := RunPi(strings.NewReader(`{"op":"fix","task":"t","workdir":"`+dir+`"}`), &stdout, &stderr)
	if err == nil || !strings.Contains(stdout.String(), "coder provider not configured") {
		t.Fatalf("缺 provider 应快速失败: err=%v stdout=%s", err, stdout.String())
	}
}
