package agentcli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSession 造一个 pi-web 会话文件（<sessionDir>/sessions/<hash>/<ts>_<id>.jsonl）。
func writeSession(t *testing.T, sessionDir, id string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(sessionDir, "sessions", "cwd-hash")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	path := filepath.Join(dir, "2026-01-01T00-00-00-000Z_"+id+".jsonl")
	content := ""
	for _, l := range lines {
		content += l + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}
	return path
}

const (
	assistantDone   = `{"type":"message","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"done"}]}}`
	assistantTool   = `{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"edit"}]}}`
	assistantNoStop = `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"thinking"}]}}`
	toolResultLine  = `{"type":"message","message":{"role":"toolResult","content":[{"type":"text","text":"ok"}]}}`
)

// TestLastMessageTerminal 完成判据①：最后一条必须是「本轮终结的 assistant」。
func TestLastMessageTerminal(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  bool
	}{
		{"终结 assistant", []string{assistantTool, toolResultLine, assistantDone}, true},
		{"最后是 toolUse（LLM 等下一轮）", []string{assistantDone, assistantTool}, false},
		{"最后是 toolResult（工具执行中）", []string{assistantTool, toolResultLine}, false},
		{"非 message 条目也要跳过", []string{assistantDone, `{"type":"session_meta","foo":1}`}, true},
		{"空文件", nil, false},
		{"只有用户消息", []string{`{"type":"message","message":{"role":"user","content":"hi"}}`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			id := "sess-1"
			if len(tc.lines) > 0 {
				writeSession(t, dir, id, tc.lines...)
			} else {
				if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			file := SessionFileByID(dir, id)
			if file == "" {
				if tc.want {
					t.Fatalf("应找到会话文件")
				}
				return
			}
			if got := LastMessageTerminal(file); got != tc.want {
				t.Errorf("LastMessageTerminal=%v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestLastUserMessage content 为字符串或 [{type:text}] 数组都要能取出文本。
func TestLastUserMessage(t *testing.T) {
	dir := t.TempDir()
	id := "sess-2"
	writeSession(t, dir, id,
		`{"type":"message","message":{"role":"user","content":"第一条"}}`,
		assistantDone,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"要修的"},{"type":"text","text":"问题"}]}}`,
	)
	p := &protoIO{stdout: os.Stdout, stderr: os.Stderr}
	if got := p.lastUserMessage(dir, id); got != "要修的 问题" {
		t.Errorf("lastUserMessage=%q，期望 %q", got, "要修的 问题")
	}
	if got := p.lastUserMessage(dir, "missing"); got != "" {
		t.Errorf("会话不存在应返回空串，得到 %q", got)
	}
}

// TestLoadPiEnvTimeouts 超时默认值回归：
//   - PI_TIMEOUT_SEC = 7200（整轮硬上限 2 小时）
//   - PI_STALL_SEC   = 3600（无进展 1 小时才判停滞）
//
// 停滞阈值取 1 小时而非 30 分钟：本地模型冷启动/大上下文预填阶段只算不输出，
// 会话文件长时间不增长，阈值太短会误杀正常推理（2026-10-29）。
func TestLoadPiEnvTimeouts(t *testing.T) {
	t.Setenv("PI_TIMEOUT_SEC", "")
	t.Setenv("PI_STALL_SEC", "")
	env := LoadPiEnv()
	if env.TimeoutSec != 7200 {
		t.Errorf("TimeoutSec 默认应为 7200，得到 %d", env.TimeoutSec)
	}
	if env.StallSec != 3600 {
		t.Errorf("StallSec 默认应为 3600（1 小时），得到 %d", env.StallSec)
	}
	// 停滞阈值必须明显小于硬超时，否则停滞检测形同虚设
	if env.StallSec >= env.TimeoutSec {
		t.Errorf("StallSec(%d) 应小于 TimeoutSec(%d)", env.StallSec, env.TimeoutSec)
	}
}

// TestWaitForAgent 完成判据②：终结 assistant + 文件大小连续稳定 → 正常返回；不满足则超时报错。
func TestWaitForAgent(t *testing.T) {
	oldPolls, oldRetries := piStablePolls, piFollowUpRetries
	piStablePolls, piFollowUpRetries = 2, 0
	t.Cleanup(func() { piStablePolls, piFollowUpRetries = oldPolls, oldRetries })

	t.Run("完成", func(t *testing.T) {
		dir := t.TempDir()
		id := "sess-ok"
		writeSession(t, dir, id, assistantTool, toolResultLine, assistantDone)
		env := PiEnv{SessionDir: dir, PollSec: 1, TimeoutSec: 10}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var stdout, stderr strings.Builder
		p := &protoIO{stdout: &stdout, stderr: &stderr}

		start := time.Now()
		if err := p.waitForAgent(ctx, env, nil, id, ""); err != nil {
			t.Fatalf("waitForAgent 应成功: %v (stdout=%s)", err, stdout.String())
		}
		if elapsed := time.Since(start); elapsed < 2*time.Second {
			t.Errorf("应至少等 2 个轮询周期，实际 %v", elapsed)
		}
	})

	t.Run("未完成则超时", func(t *testing.T) {
		dir := t.TempDir()
		id := "sess-hang"
		writeSession(t, dir, id, assistantTool) // 最后是 toolUse → 永不判定完成
		env := PiEnv{SessionDir: dir, PollSec: 1, TimeoutSec: 1}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var stdout, stderr strings.Builder
		p := &protoIO{stdout: &stdout, stderr: &stderr}

		err := p.waitForAgent(ctx, env, nil, id, "")
		if err == nil {
			t.Fatalf("未完成的会话应超时失败")
		}
		if !strings.Contains(stdout.String(), `"ok":false`) {
			t.Errorf("失败应输出协议 JSON，得到 %q", stdout.String())
		}
	})
}

// TestWaitForAgentStall 停滞检测：会话文件长期不增长（agent 静默死亡）→ 不等硬超时就失败。
// 2026-10-29 实测：pi-web 会话在 toolResult 后循环停止，无错误无连接，旧逻辑只能等 25h。
func TestWaitForAgentStall(t *testing.T) {
	oldPolls := piStablePolls
	piStablePolls = 2
	t.Cleanup(func() { piStablePolls = oldPolls })

	dir := t.TempDir()
	id := "sess-stall"
	// 最后是 toolResult → 永不判定完成（真实故障形态）
	writeSession(t, dir, id, assistantTool, toolResultLine)
	env := PiEnv{SessionDir: dir, PollSec: 1, TimeoutSec: 600, StallSec: 2}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stdout, stderr strings.Builder
	p := &protoIO{stdout: &stdout, stderr: &stderr}

	start := time.Now()
	err := p.waitForAgent(ctx, env, nil, id, "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("停滞应快速失败，而不是等硬超时")
	}
	if elapsed > 15*time.Second {
		t.Errorf("停滞应在 StallSec 附近失败，实际 %v", elapsed)
	}
	if !strings.Contains(stdout.String(), "stalled") {
		t.Errorf("失败原因应含 stalled，得到 %q", stdout.String())
	}
}

// TestCountToolCalls 只读不写统计：toolCall 计数 + write/edit 识别。
func TestCountToolCalls(t *testing.T) {
	dir := t.TempDir()
	id := "sess-count"
	file := writeSession(t, dir, id,
		`{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"read"},{"type":"toolCall","name":"bash"}]}}`,
		toolResultLine,
		`{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"ls"}]}}`,
		toolResultLine,
	)
	calls, writes := countToolCalls(file)
	if calls != 3 || writes != 0 {
		t.Errorf("只读会话应为 3 调用/0 写，得到 %d/%d", calls, writes)
	}

	file2 := writeSession(t, dir, "sess-write",
		`{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"toolCall","name":"edit"},{"type":"toolCall","name":"write"}]}}`,
	)
	calls2, writes2 := countToolCalls(file2)
	if calls2 != 2 || writes2 != 2 {
		t.Errorf("写会话应为 2 调用/2 写，得到 %d/%d", calls2, writes2)
	}
}
