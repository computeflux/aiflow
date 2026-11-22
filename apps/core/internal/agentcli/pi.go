package agentcli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// PiEnv pi adapter 运行环境（driver 经 env 注入，与旧 adapter.sh 一致）。
type PiEnv struct {
	WebURL     string // PI_WEB_URL（默认 http://pi-web:30141）
	Password   string // PI_WEB_PASSWORD（Basic Auth，用户名固定 pi）
	TimeoutSec int    // PI_TIMEOUT_SEC（默认 7200）：整轮硬上限
	StallSec   int    // PI_STALL_SEC（默认 3600）：会话文件持续不增长 → 判定 agent 静默死亡
	PollSec    int    // PI_POLL_SEC（默认 5）
	// ReadOnlyNudgeCalls 只读催促阈值（默认 60）：工具调用达到该数量且 worktree 零改动 →
	// 发一条 followUp 催促立即落代码（不改状态、不打断 agent）。0 关闭。
	ReadOnlyNudgeCalls int
	Provider           string // PI_PROVIDER（必填）
	Model              string // PI_MODEL（必填）
	SessionDir         string // PI_SESSION_DIR（默认 /host/.pi/agent）
	HostRepo           string // PI_HOST_REPO（默认 /repo）：pi-web 所在主机上的仓库根，用于把容器内 /repo/... 的 cwd 翻译成宿主路径
}

// LoadPiEnv 从环境变量组装配置。
func LoadPiEnv() PiEnv {
	return PiEnv{
		WebURL:             envOr("PI_WEB_URL", "http://pi-web:30141"),
		Password:           os.Getenv("PI_WEB_PASSWORD"),
		TimeoutSec:         envInt("PI_TIMEOUT_SEC", 7200),
		StallSec:           envInt("PI_STALL_SEC", 3600),
		PollSec:            envInt("PI_POLL_SEC", 5),
		ReadOnlyNudgeCalls: envInt("PI_READONLY_NUDGE_CALLS", 60),
		Provider:           os.Getenv("PI_PROVIDER"),
		Model:              os.Getenv("PI_MODEL"),
		SessionDir:         envOr("PI_SESSION_DIR", "/host/.pi/agent"),
		HostRepo:           envOr("PI_HOST_REPO", "/repo"),
	}
}

// hostCwd 把容器内仓库路径（/repo/...）翻译成 pi-web 所在主机的路径。
// pi-web 容器化时两者相同（都挂 /repo）；宿主运行时 pi-web 看不到 /repo，
// 必须换成宿主真实仓库根（PI_HOST_REPO）。
func (e PiEnv) hostCwd(cwd string) string {
	if e.HostRepo == "/repo" || e.HostRepo == "" {
		return cwd
	}
	if cwd == "/repo" {
		return e.HostRepo
	}
	if strings.HasPrefix(cwd, "/repo/") {
		return e.HostRepo + cwd[len("/repo"):]
	}
	return cwd
}

// piStablePolls 完成判据的文件稳定次数（× PollSec 秒）。
// var 而非 const：单测需要把它调小，避免每个用例真等 6×pollSec。
var piStablePolls = 6

// piFollowUpRetries "already processing" 时的重试次数（每 15s 一次，共 ~5 分钟）。
var piFollowUpRetries = 20

// piReadOnlyNudges 单次 coder 运行内最多发几次只读催促（防刷屏）。
var piReadOnlyNudges = 2

// writeToolNames 视为「写操作」的工具名（pi 的写文件工具）。
var writeToolNames = map[string]bool{
	"write": true, "edit": true, "patch": true, "apply_patch": true,
	"str_replace": true, "multiedit": true, "notebook_edit": true,
}

// RunPi coder adapter（op=fix）：经 pi-web HTTP API 驱动 agent 改代码并上报 worktree 累计 diff。
// 走 pi-web 而非 pi CLI：对话记录在 pi-web UI 可见（人工可直接看 coder 每一步）。
// RunPi coder adapter（op=fix）：经 pi-web HTTP API 驱动 agent 改代码并上报 worktree 累计 diff。
// 走 pi-web 而非 pi CLI：对话记录在 pi-web UI 可见（人工可直接看 coder 每一步）。
func RunPi(in io.Reader, stdout, stderr io.Writer) error {
	p := &protoIO{stdout: stdout, stderr: stderr}

	var input FixInput
	if err := p.readInput(in, &input); err != nil {
		return err
	}
	if input.Op != "fix" {
		return p.fail("unsupported op: %s", input.Op)
	}
	if strings.TrimSpace(input.Task) == "" {
		return p.fail("missing task")
	}
	if input.Workdir == "" {
		return p.fail("missing workdir")
	}
	if st, err := os.Stat(input.Workdir); err != nil || !st.IsDir() {
		return p.fail("workdir not found: %s", input.Workdir)
	}

	env := LoadPiEnv()
	if env.Provider == "" {
		return p.fail("coder provider not configured: set PI_PROVIDER env")
	}
	if env.Model == "" {
		return p.fail("coder model not configured: set PI_MODEL env")
	}
	EnsureSafeDirectory(input.Workdir)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(env.TimeoutSec)*time.Second)
	defer cancel()

	client := newPiWeb(env)
	sessionID, err := p.submit(ctx, client, env, input)
	if err != nil {
		return err
	}
	p.logf("session=%s, waiting for agent to finish...", sessionID)

	if err := p.waitForAgent(ctx, env, client, sessionID, input.Workdir); err != nil {
		return err
	}

	diff := ""
	files := []string{}
	if GitAvailable(input.Workdir) {
		diff, files = CollectChanges(input.Workdir)
	}
	return p.emit(FixOutput{OK: true, Diff: diff, FilesChanged: files, SessionID: sessionID})
}

// submit 提交本轮 prompt：续接已有会话（followUp）或新建会话。
// 返回本轮使用的 session ID。
func (p *protoIO) submit(ctx context.Context, client *piWeb, env PiEnv, input FixInput) (string, error) {
	// 新建会话用完整提示词；续接只发上一轮反馈（session 已保留任务上下文）。
	full := BuildFixPrompt(input.Kind, input.Task, input.Files) + WorkdirGuard(input.Workdir)
	cont := BuildContinuePrompt(input.Feedback) + WorkdirGuard(input.Workdir)

	if input.SessionID != "" {
		// 去重：若 session 里最后一条 user 消息 == 本次要发的反馈，说明该反馈已排队/已处理
		// （LLM 慢时 followUp 会堆积；driver 重启复用 session 也会重发同一条）→ 跳过发送，
		// 直接进入等待循环（agent 跑完/文件稳定后检查 diff），避免同一反馈重复入队。
		if p.lastUserMessage(env.SessionDir, input.SessionID) == cont {
			// ⚠️ 去重命中 ≠ 已锚定：历史消息可能是旧版无工作目录守卫的纯反馈，而续接轮的
			// pi-web agent 进程 cwd 不可控（实测沿宿主仓库写盘）→ 不能盲等，必须 fail-fast。
			return "", p.fail("feedback %q already queued in session %s (agent busy?)", tail(cont, 120), input.SessionID)
		}
		// streamingBehavior=followUp：agent 还在跑时消息排队（不拒绝），跑完自动处理。
		body, err := client.sendFollowUp(ctx, input.SessionID, cont)
		if err != nil {
			p.logf("followUp request failed (%v), creating new session...", err)
		} else {
			for i := 0; strings.Contains(body, "already processing") && i < piFollowUpRetries; i++ {
				p.logf("agent still processing, retrying in 15s...")
				select {
				case <-ctx.Done():
					return "", p.fail("pi-web agent timeout after %ds (session=%s)", env.TimeoutSec, input.SessionID)
				case <-time.After(15 * time.Second):
				}
				if body, err = client.sendFollowUp(ctx, input.SessionID, cont); err != nil {
					break
				}
			}
			// agent 持续在处理（5 分钟仍未排队）→ 反馈实际已入队，直接等这一轮跑完，不新建会话
			// （新建会丢上下文且同一个任务出现两个 pi-web 会话）。
			if err == nil && strings.Contains(body, "already processing") {
				p.logf("agent still processing after retries, keeping session %s", input.SessionID)
				return input.SessionID, nil
			}
			// session 不存在（driver 预生成的 id 在 pi-web 里还没创建）→ 回退新建（完整提示词）。
			if err == nil && !strings.Contains(body, "Session not found") {
				if id := sessionIDFromBody(body, input.SessionID); id != "" {
					return id, nil
				}
			}
			p.logf("session %s not found in pi-web, creating new...", input.SessionID)
		}
	}

	sessionID, err := client.createSession(ctx, env.hostCwd(input.Workdir), full, env.Provider, env.Model)
	if err != nil {
		// 错误文本已含上下文（如 "pi-web API returned error: {...}" / 网络错误）
		return "", p.fail("%s", tail(err.Error(), 500))
	}
	return sessionID, nil
}

// waitForAgent 等待 agent 完成。
//
// pi-web 的 RPC session 进程在 agent 完成后不退出（running 永远 true），不能靠
// GET /api/agent/[id] 的 running 字段判断完成。完成判据（双条件同时满足，避免两类误判）：
//   - ① 最后一条是「本轮终结的 assistant」：role=assistant 且 stopReason != toolUse
//     且 content 无 toolCall —— 排除 LLM 长思考时（最后一条是 toolResult，等下一轮）的误判，
//     否则会在首条消息未完成时就发下一轮 → follow-up 堆积。
//   - ② 会话文件大小连续 piStablePolls 次（默认 6×5s）不变 —— 排除空 assistant
//     （LLM 瞬态错误）造成的过早判定。
//
// 另有两道防线（2026-10-29 实测：pi-web 会话在 toolResult 之后循环静默停止，
// 无错误日志、无连接、无子进程，而上面两个判据永不满足 → 只能等 25h 硬超时）：
//   - 停滞检测：会话文件连续 StallSec（默认 3600s / 1 小时）不增长 → 立即失败，
//     让 driver 取消/重跑，而不是干等。
//     取 1 小时而非 30 分钟：本地模型（Qwen3.8-27B @ llama.cpp）冷启动/大上下文预填
//     阶段只算不输出，会话文件长时间不增长，阈值太短会误杀正常推理（2026-10-29）。
//   - 只读催促：工具调用已达 ReadOnlyNudgeCalls 且 worktree 零改动 → 发 followUp
//     催促立即落代码（不打断 agent，只排队一条消息）。
func (p *protoIO) waitForAgent(ctx context.Context, env PiEnv, client *piWeb, sessionID, workdir string) error {
	pollSec := env.PollSec
	if pollSec < 1 {
		pollSec = 1
	}
	deadline := time.Now().Add(time.Duration(env.TimeoutSec) * time.Second)
	stable := 0
	var lastSize int64 = -1
	lastProgress := time.Now() // 会话文件最后一次变化的时间（停滞检测基准）
	nudgeAt := env.ReadOnlyNudgeCalls
	nudges := 0
	tick := 0

	timer := time.NewTimer(time.Duration(pollSec) * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return p.fail("pi-web agent timeout after %ds (session=%s)", env.TimeoutSec, sessionID)
		case <-timer.C:
		}
		if time.Now().After(deadline) {
			return p.fail("pi-web agent timeout after %ds (session=%s)", env.TimeoutSec, sessionID)
		}
		// 停滞检测：文件不增长超过 StallSec → agent 已静默死亡，快速失败。
		// 放在会话文件存在性判断之前：会话文件一直不出现也算停滞（旧逻辑会干等硬超时）。
		if env.StallSec > 0 && time.Since(lastProgress) > time.Duration(env.StallSec)*time.Second {
			p.logf("stall detected: no session progress for %ds (session=%s)", env.StallSec, sessionID)
			return p.fail("pi-web agent stalled: no session progress for %ds (session=%s)",
				env.StallSec, sessionID)
		}
		file := SessionFileByID(env.SessionDir, sessionID)
		if file == "" {
			// 会话文件还没出现（pi-web 还在初始化）→ 继续等
			timer.Reset(time.Duration(pollSec) * time.Second)
			continue
		}
		size := fileSize(file)
		if size != lastSize {
			lastSize = size
			lastProgress = time.Now()
			stable = 0
		} else if LastMessageTerminal(file) && size > 0 {
			stable++
			if stable >= piStablePolls {
				p.logf("agent finished (session=%s, last=assistant no toolCall, size stable %dB for %ds)",
					sessionID, size, piStablePolls*pollSec)
				return nil
			}
		} else {
			stable = 0
		}

		// 只读催促：每 6 个轮询（30s）才解析一次会话，避免大会话文件被频繁扫描。
		tick++
		if client != nil && env.ReadOnlyNudgeCalls > 0 && tick%6 == 0 && nudges < piReadOnlyNudges {
			calls, writes := countToolCalls(file)
			if calls >= nudgeAt && writes == 0 && !HasChanges(workdir) {
				nudges++
				nudgeAt += env.ReadOnlyNudgeCalls
				// 去重：上一条催促还在 pi-web 队列里未消费（agent 慢，催促堆积）→ 跳过本次。
				// 否则同一条「只读不写」催促会在会话里排队 N 份，LLM 恢复后被重复提示刷屏
				//（2026-11-11 实测：62 次调用后连续多条同文案 followUp）。
				// 优先查 pi-web 真实队列（get_state.queuedMessages.followUp，含正在处理中的那条）；
				// 查询失败回退会话文件比对（只能覆盖「最后一条是催促且未消费」的情形）。
				if p.lastUserMessage(env.SessionDir, sessionID) == readOnlyNudgeMessage(calls) {
					p.logf("read-only nudge already queued (session=%s), skipping", sessionID)
					continue
				}
				if client.queueFollowUpCount(ctx, sessionID) > 0 {
					p.logf("followUp queue not drained (session=%s), skipping nudge", sessionID)
					continue
				}
				p.logf("read-only spiral: %d tool calls, 0 writes → nudging agent (session=%s)", calls, sessionID)
				body, err := client.sendFollowUp(ctx, sessionID, readOnlyNudgeMessage(calls))
				if err != nil {
					p.logf("nudge failed: %v", err)
				} else {
					p.logf("nudge sent: %s", tail(strings.TrimSpace(body), 120))
				}
			}
		}
		timer.Reset(time.Duration(pollSec) * time.Second)
	}
}

// ---- 会话文件（pi-web 写在 $PI_SESSION_DIR/sessions/<cwd-hash>/<ts>_<id>.jsonl）----

// readOnlyNudgeMessage 只读催促文案：明确要求立即落代码（先骨架后完善）。
func readOnlyNudgeMessage(calls int) string {
	return fmt.Sprintf("你已经执行了 %d 次工具调用，但 worktree 里没有任何文件改动（只读不写）。"+
		"请立即停止探索，用 write/edit 创建或修改文件：先落一个最小可运行骨架，再迭代完善。"+
		"继续只读不写本轮将判定失败。", calls)
}

// lastUserMessage 会话里最后一条 user 消息文本（content 为 string 或 [{type:text,text:...}]）。
func (p *protoIO) lastUserMessage(sessionDir, id string) string {
	file := SessionFileByID(sessionDir, id)
	if file == "" {
		return ""
	}
	last := ""
	for _, line := range tailLines(file, 80) {
		var entry struct {
			Message *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Message == nil {
			continue
		}
		if entry.Message.Role == "user" {
			last = messageText(entry.Message.Content)
		}
	}
	return strings.TrimSpace(last)
}
