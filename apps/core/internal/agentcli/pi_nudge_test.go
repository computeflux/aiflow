package agentcli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestQueueFollowUpCount 真实队列查询：GET /api/agent/:id → data.queuedMessages.followUp 长度；
// 响应异常 / success=false → -1（调用方按「无队列」处理，催促照发）。
func TestQueueFollowUpCount(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"空队列", `{"success":true,"data":{"queuedMessages":{"steering":[],"followUp":[]}}}`, 0},
		{"1 条排队", `{"success":true,"data":{"queuedMessages":{"steering":[],"followUp":["a"]}}}`, 1},
		{"2 条排队", `{"success":true,"data":{"queuedMessages":{"steering":["s"],"followUp":["a","b"]}}}`, 2},
		{"success=false", `{"success":false,"error":"boom"}`, -1},
		{"非 JSON", `not-json`, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httpServer(t, tc.body)
			client := &piWeb{base: srv, password: "pw", client: newTestHTTPClient()}
			if got := client.queueFollowUpCount(context.Background(), "S1"); got != tc.want {
				t.Fatalf("want %d, got %d (body=%s)", tc.want, got, tc.body)
			}
		})
	}
}

func httpServer(t *testing.T, body string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
	_, url := startTestServer(t, mux)
	return url
}

func newTestHTTPClient() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func startTestServer(t *testing.T, h http.Handler) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.URL
}

// TestNudgeDedup nudge 去重：上一条催促未消费 → 不再发新催促（会话文件比对命中，
// 不查 pi-web 队列）；真实队列非空 → 同样跳过。催促条件 = 只读调用达阈值且 worktree 零改动。
func TestNudgeDedup(t *testing.T) {
	oldPolls, oldNudges := piStablePolls, piReadOnlyNudges
	piStablePolls, piReadOnlyNudges = 100, 10 // 不触发完成判定，只测 nudge 分支
	t.Cleanup(func() { piStablePolls, piReadOnlyNudges = oldPolls, oldNudges })

	// 会话：大量只读 toolCall（calls ≥ ReadOnlyNudgeCalls=1 → 触发催促条件）
	dir := t.TempDir()
	id := "sess-nudge"

	nudgeMsg := readOnlyNudgeMessage(2) // countToolCalls=2（calls=2, writes=0）

	t.Run("会话末条是未消费的催促 → 跳过", func(t *testing.T) {
		// 把会话末条改写成催促消息（模拟上一条已入队未消费）
		writeSession(t, dir, id,
			`{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"read"},{"type":"toolCall","name":"bash"}]}}`,
			`{"type":"message","message":{"role":"user","content":`+strconv.Quote(nudgeMsg)+`}}`,
		)
		var nudges atomic.Int64
		client := nudgeTestClient(t, &nudges, `{"success":true,"data":{"queuedMessages":{"steering":[],"followUp":["x"]}}}`)
		env := PiEnv{SessionDir: dir, PollSec: 1, TimeoutSec: 8, ReadOnlyNudgeCalls: 1}

		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		var stdout, stderr strings.Builder
		p := &protoIO{stdout: &stdout, stderr: &stderr}
		start := time.Now()
		_ = p.waitForAgent(ctx, env, client, id, "") // 完成判定被隔离，超时退出即可

		if got := nudges.Load(); got != 0 {
			t.Fatalf("会话末条已是催促 → 不应再发 nudge，得到 %d 次", got)
		}
		if !strings.Contains(stderr.String(), "already queued") {
			t.Errorf("应记录去重日志，stderr=%s", stderr.String())
		}
		_ = start
	})

	t.Run("真实队列非空 → 跳过", func(t *testing.T) {
		writeSession(t, dir, id,
			`{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"read"},{"type":"toolCall","name":"bash"}]}}`,
		)
		var nudges atomic.Int64
		client := nudgeTestClient(t, &nudges, `{"success":true,"data":{"queuedMessages":{"steering":[],"followUp":["q1","q2"]}}}`)
		env := PiEnv{SessionDir: dir, PollSec: 1, TimeoutSec: 8, ReadOnlyNudgeCalls: 1}

		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		var stdout, stderr strings.Builder
		p := &protoIO{stdout: &stdout, stderr: &stderr}
		_ = p.waitForAgent(ctx, env, client, id, "")

		if got := nudges.Load(); got != 0 {
			t.Fatalf("队列非空 → 不应发 nudge，得到 %d 次", got)
		}
		if !strings.Contains(stderr.String(), "queue not drained") {
			t.Errorf("应记录队列去重日志，stderr=%s", stderr.String())
		}
	})

	t.Run("会话空且队列空 → 发一次", func(t *testing.T) {
		writeSession(t, dir, id,
			`{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"read"},{"type":"toolCall","name":"bash"}]}}`,
		)
		var nudges atomic.Int64
		client := nudgeTestClient(t, &nudges, `{"success":true,"data":{"queuedMessages":{"steering":[],"followUp":[]}}}`)
		env := PiEnv{SessionDir: dir, PollSec: 1, TimeoutSec: 8, ReadOnlyNudgeCalls: 1}

		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		var stdout, stderr strings.Builder
		p := &protoIO{stdout: &stdout, stderr: &stderr}
		_ = p.waitForAgent(ctx, env, client, id, "")

		if got := nudges.Load(); got < 1 {
			t.Fatalf("队列空 → 应发 nudge，得到 %d 次", got)
		}
	})
}

// nudgeTestClient POST /api/agent/:id（followUp）计数并应答；GET /api/agent/:id 返回 state。
func nudgeTestClient(t *testing.T, nudges *atomic.Int64, state string) *piWeb {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			nudges.Add(1)
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = w.Write([]byte(state))
	})
	_, url := startTestServer(t, mux)
	return &piWeb{base: url, password: "pw", client: newTestHTTPClient()}
}

var _ = json.Marshal
