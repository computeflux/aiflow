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

// analyst Agent（需求分析 / 拆卡）：复用 pi-web 基础设施，但走只读工具、不写代码。
//
// 与 coder（RunPi）的区别：
//   - 工具白名单只读（read/grep/find/ls），不可能改动仓库；
//   - 提示词要求**只输出 JSON 卡片数组**，不产生任何文件变更；
//   - 结果从 pi-web 会话文件读取，解析成结构化卡片，不收集 git diff、不串流。
//
// 定位：看板 AI 的「产品需求分析」能力，按需调用（像 skill），卡片仍由前端人工确认后落库。

// analystReadOnlyTools pi-web 工具白名单（CODING_TOOL_NAMES 的只读子集）。
// 不含 bash / edit / write / powershell → 分析过程不会改仓库。
var analystReadOnlyTools = []string{"read", "grep", "find", "ls"}

// AnalyzeRequest 需求分析入参（进程内调用）。
type AnalyzeRequest struct {
	Requirement  string // 用户需求原文（本次唯一要分析的目标）
	History      string // 最近对话（压缩，帮助理解追问；可为空）
	BoardContext string // 精简看板上下文（列/已有卡/可用项目，由调用方组装）
	ListID       uint   // 默认落卡列
	Cwd          string // 容器内 cwd（会经 PI_HOST_REPO 翻译成宿主路径）
}

// AnalystCard 分析产出的任务卡（字段与 create_card 工具同构）。
type AnalystCard struct {
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Priority       string   `json:"priority"`
	Assignee       string   `json:"assignee,omitempty"`
	Labels         string   `json:"labels,omitempty"`
	TargetName     string   `json:"target_name"`
	DueDate        string   `json:"due_date,omitempty"`
	Checklist      []string `json:"checklist,omitempty"`
	EstimateHours  float64  `json:"estimate_hours,omitempty"`
	ListID         uint     `json:"list_id,omitempty"`
	DependsOnIndex []int    `json:"depends_on_index,omitempty"`
}

// AnalyzeOutput 需求分析结果：结果（卡片）+ pi 执行结果 + 会话。
type AnalyzeOutput struct {
	OK        bool          `json:"ok"`
	Cards     []AnalystCard `json:"cards"`
	SessionID string        `json:"session_id"`
	Summary   string        `json:"summary,omitempty"`
	Reason    string        `json:"reason,omitempty"`
}

// AnalyzeInput analyst 适配器（CLI 协议）输入。
type AnalyzeInput struct {
	Op           string `json:"op"`
	Requirement  string `json:"requirement"`
	History      string `json:"history,omitempty"`
	BoardContext string `json:"board_context"`
	ListID       uint   `json:"list_id"`
	Cwd          string `json:"cwd"`
}

// RunAnalyze analyst 适配器入口（stdin → stdout JSON），供 Agent 注册表 / driver 调用。
// 失败输出 {"ok":false,"reason":...} + exit 1（与 coder/reviewer 协议一致）。
func RunAnalyze(in io.Reader, stdout, stderr io.Writer) error {
	p := &protoIO{stdout: stdout, stderr: stderr}

	var input AnalyzeInput
	if err := p.readInput(in, &input); err != nil {
		return err
	}
	if op := strings.TrimSpace(input.Op); op != "" && op != "analyze" {
		return p.fail("unsupported op: %s", op)
	}
	if strings.TrimSpace(input.Requirement) == "" {
		return p.fail("missing requirement")
	}

	// provider 归一见 AnalyzeRequirement（env 装配处）。

	out, err := AnalyzeRequirement(context.Background(), AnalyzeRequest{
		Requirement:  input.Requirement,
		History:      input.History,
		BoardContext: input.BoardContext,
		ListID:       input.ListID,
		Cwd:          input.Cwd,
	}, stderr)
	if err != nil {
		return p.fail("%s", tail(err.Error(), 800))
	}
	return p.emit(out)
}

// AnalyzeRequirement 经 pi-web 做需求分析与拆卡，返回结构化卡片 + 会话 ID。
//
// 同步、非流式：创建会话 → 等 pi 跑完 → 读最终消息 → 解析 JSON。
// logw 用于进度日志（可传 nil）。
func AnalyzeRequirement(ctx context.Context, req AnalyzeRequest, logw io.Writer) (AnalyzeOutput, error) {
	if strings.TrimSpace(req.Requirement) == "" {
		return AnalyzeOutput{}, fmt.Errorf("missing requirement")
	}

	env := LoadPiEnv()
	if env.Provider == "" {
		return AnalyzeOutput{}, fmt.Errorf("analyst provider not configured: set PI_PROVIDER env")
	}
	if env.Model == "" {
		return AnalyzeOutput{}, fmt.Errorf("analyst model not configured: set PI_MODEL env")
	}
	// provider 归一（与 evolve-driver 同源规则）：PI_MODEL + PI_BASE_URL 命中
	// model_profiles 的 sync_agents 档案时，provider 归一到 "model-managed"
	//（S8.1 同步只把档案写进宿主 pi-web models.json 的该 provider）；未命中
	// 保持 PI_PROVIDER 原值（宿主 models.json 手工维护的 provider，如 local）。
	normalizePiProvider(logw)
	env.Provider = os.Getenv("PI_PROVIDER")
	env.Model = os.Getenv("PI_MODEL")
	// analyst 只读不写：关掉「只读催促」（那条催促会要求 agent 立刻落代码）。
	env.ReadOnlyNudgeCalls = 0
	// 需求分析比实现短：默认 30 分钟硬上限 / 15 分钟停滞（可用专用 env 覆盖）。
	env.TimeoutSec = envInt("PI_ANALYST_TIMEOUT_SEC", envInt("PI_TIMEOUT_SEC", 1800))
	env.StallSec = envInt("PI_ANALYST_STALL_SEC", 900)
	if env.PollSec < 1 || env.PollSec > 5 {
		env.PollSec = 3
	}

	p := &protoIO{stdout: io.Discard, stderr: logw}
	if p.stderr == nil {
		p.stderr = io.Discard
	}

	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" {
		cwd = "/repo"
	}

	client := newPiWeb(env)
	prompt := buildAnalystPrompt(req.Requirement, req.History, req.BoardContext, req.ListID)
	p.logf("analyst: prompt %d 字符（需求 %d，对话 %d，看板 %d）",
		len([]rune(prompt)), len([]rune(req.Requirement)), len([]rune(req.History)), len([]rune(req.BoardContext)))
	p.logf("analyst: creating pi-web session (cwd=%s, provider=%s, model=%s)",
		env.hostCwd(cwd), env.Provider, env.Model)
	sessionID, err := client.createSessionWithTools(
		ctx, env.hostCwd(cwd), prompt, env.Provider, env.Model, analystReadOnlyTools)
	if err != nil {
		return AnalyzeOutput{}, fmt.Errorf("create pi-web session: %w", err)
	}
	p.logf("analyst: session=%s, waiting for agent...", sessionID)

	if err := p.waitForAnalyst(ctx, env, client, sessionID); err != nil {
		return AnalyzeOutput{OK: false, SessionID: sessionID, Reason: err.Error()}, err
	}

	file := SessionFileByID(env.SessionDir, sessionID)
	if file == "" {
		return AnalyzeOutput{OK: false, SessionID: sessionID, Reason: "session file not found"},
			fmt.Errorf("session file not found (session=%s)", sessionID)
	}
	text := lastAssistantText(file)
	if strings.TrimSpace(text) == "" {
		return AnalyzeOutput{OK: false, SessionID: sessionID, Reason: "empty assistant output"},
			fmt.Errorf("pi returned empty assistant output (session=%s)", sessionID)
	}

	cards, summary, err := parseAnalystCards(text)
	if err != nil {
		return AnalyzeOutput{OK: false, SessionID: sessionID, Reason: err.Error()},
			fmt.Errorf("parse analyst cards: %w (raw: %s)", err, tail(strings.TrimSpace(text), 400))
	}
	if len(cards) == 0 {
		return AnalyzeOutput{OK: false, SessionID: sessionID, Reason: "no cards produced"},
			fmt.Errorf("analyst produced no cards (session=%s)", sessionID)
	}
	p.logf("analyst: session=%s done, %d card(s)", sessionID, len(cards))
	return AnalyzeOutput{OK: true, Cards: cards, SessionID: sessionID, Summary: summary}, nil
}

// analystStopNudge 收尾指令：工具调用达预算后逼模型直接输出 JSON。
const analystStopNudge = "立即停止一切工具调用。你已经读取了足够的信息。现在不要再看任何文件，直接根据已读内容只输出最终 JSON 数组（严格符合上文输出契约），不要输出任何解释文字。"

// waitForAnalyst 等待 analyst 跑完。
//
// 与 waitForAgent（coder）的差异：
//   - 不做「只读催促落代码」（analyst 本来就不写代码）；
//   - 反过来要防「只读不停」：pi 会反复说“我已经有结论了”却继续 verify
//     （实测 113+ 条消息 / 13 分钟仍未收尾）。故每 4 个轮询检查工具调用数，
//     达预算就发一条强中止指令逼它直接输出 JSON（最多 2 次）。
func (p *protoIO) waitForAnalyst(ctx context.Context, env PiEnv, client *piWeb, sessionID string) error {
	pollSec := env.PollSec
	if pollSec < 1 {
		pollSec = 1
	}
	deadline := time.Now().Add(time.Duration(env.TimeoutSec) * time.Second)
	stable := 0
	var lastSize int64 = -1
	lastProgress := time.Now()
	budget := envInt("PI_ANALYST_TOOL_BUDGET", 12)
	nudges := 0
	const maxNudges = 2
	tick := 0

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("analyst cancelled")
		case <-time.After(time.Duration(pollSec) * time.Second):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("analyst timeout after %ds (session=%s)", env.TimeoutSec, sessionID)
		}
		if env.StallSec > 0 && time.Since(lastProgress) > time.Duration(env.StallSec)*time.Second {
			p.logf("stall detected: no progress for %ds (session=%s)", env.StallSec, sessionID)
			return fmt.Errorf("analyst stalled: no progress for %ds (session=%s)", env.StallSec, sessionID)
		}
		file := SessionFileByID(env.SessionDir, sessionID)
		if file == "" {
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
				p.logf("analyst finished (session=%s, size stable %dB)", sessionID, size)
				return nil
			}
		} else {
			stable = 0
		}

		// 防「只读不停」：每 4 个轮询解析一次，工具调用达预算就催它收尾。
		tick++
		if client != nil && nudges < maxNudges && tick%4 == 0 {
			if calls, _ := countToolCalls(file); calls >= budget*(nudges+1) {
				nudges++
				p.logf("analyst: %d tool calls → forcing final answer (nudge %d/%d, session=%s)",
					calls, nudges, maxNudges, sessionID)
				if _, err := client.sendFollowUp(ctx, sessionID, analystStopNudge); err != nil {
					p.logf("analyst: stop nudge failed: %v", err)
				}
			}
		}
	}
}

// createSessionWithTools 新建会话并限定 pi 的激活工具（只读白名单）。
func (c *piWeb) createSessionWithTools(ctx context.Context, cwd, message, provider, model string, toolNames []string) (string, error) {
	payload := map[string]any{
		"cwd":      cwd,
		"type":     "prompt",
		"message":  message,
		"provider": provider,
		"modelId":  model,
	}
	if len(toolNames) > 0 {
		payload["toolNames"] = toolNames
	}
	body, err := c.post(ctx, "/api/agent/new", payload)
	if err != nil {
		return "", err
	}
	var resp struct {
		Success   bool   `json:"success"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal([]byte(body), &resp) != nil || !resp.Success || resp.SessionID == "" {
		return "", fmt.Errorf("pi-web API returned error: %s", tail(strings.TrimSpace(body), 500))
	}
	return resp.SessionID, nil
}

// lastAssistantText 会话文件里最后一条 assistant 消息的纯文本。
func lastAssistantText(path string) string {
	last := ""
	for _, line := range tailLines(path, 80) {
		var entry struct {
			Message *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Message == nil {
			continue
		}
		if entry.Message.Role == "assistant" {
			if t := strings.TrimSpace(messageText(entry.Message.Content)); t != "" {
				last = t
			}
		}
	}
	return last
}

// parseAnalystCards 从 pi 输出里提取卡片 JSON 数组。
// 容错：剥离 ```json 围栏、截取首个 '[' 到末个 ']'；也接受 {"cards":[...]}。
func parseAnalystCards(text string) ([]AnalystCard, string, error) {
	raw := strings.TrimSpace(text)

	// 剥 markdown 代码围栏（可能出现在首尾）。
	if i := strings.Index(raw, "```"); i >= 0 {
		rest := raw[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		rest = strings.TrimPrefix(rest, "JSON")
		if j := strings.Index(rest, "```"); j >= 0 {
			raw = rest[:j]
		} else {
			raw = rest
		}
	}

	// 先尝试直接解析（对象或数组）。
	if cards, ok := decodeCards(raw); ok {
		return cards, "", nil
	}

	// 截取首个 '[' 到末个 ']'。
	if i, j := strings.Index(raw, "["), strings.LastIndex(raw, "]"); i >= 0 && j > i {
		if cards, ok := decodeCards(raw[i : j+1]); ok {
			return cards, strings.TrimSpace(raw[:i]), nil
		}
	}
	// 截取首个 '{' 到末个 '}'（{"cards":[...]} 形式）。
	if i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}"); i >= 0 && j > i {
		if cards, ok := decodeCards(raw[i : j+1]); ok {
			return cards, strings.TrimSpace(raw[:i]), nil
		}
	}
	return nil, "", fmt.Errorf("no JSON card array found in output")
}

// decodeCards 解析数组或 {"cards":[...]}；失败返回 ok=false。
func decodeCards(s string) ([]AnalystCard, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	if strings.HasPrefix(s, "[") {
		var cards []AnalystCard
		if json.Unmarshal([]byte(s), &cards) == nil {
			return sanitizeCards(cards), true
		}
		return nil, false
	}
	var wrap struct {
		Cards []AnalystCard `json:"cards"`
	}
	if json.Unmarshal([]byte(s), &wrap) == nil && wrap.Cards != nil {
		return sanitizeCards(wrap.Cards), true
	}
	return nil, false
}

// sanitizeCards 去掉空标题卡，收敛 priority 取值。
func sanitizeCards(in []AnalystCard) []AnalystCard {
	out := make([]AnalystCard, 0, len(in))
	for _, c := range in {
		c.Title = strings.TrimSpace(c.Title)
		if c.Title == "" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(c.Priority)) {
		case "low", "medium", "high", "urgent":
			c.Priority = strings.ToLower(strings.TrimSpace(c.Priority))
		case "高":
			c.Priority = "high"
		case "低":
			c.Priority = "low"
		case "紧急":
			c.Priority = "urgent"
		default:
			c.Priority = "medium"
		}
		out = append(out, c)
	}
	return out
}

// buildAnalystPrompt 组装 analyst 提示词：卡片规则 + 看板上下文 + 用户需求 + 严格 JSON 契约。
func buildAnalystPrompt(requirement, history, boardContext string, listID uint) string {
	var sb strings.Builder

	// 需求放**最前面**（2026-11-09 修正）：之前把需求放在 8–9KB 看板/仓库上下文之后，
	// 模型会“不分析当前需求”。现在顺序：角色 → 当前需求 → 最近对话 → 精简看板上下文 → 输出契约。
	sb.WriteString("你是一名资深产品经理兼系统架构师，负责把产品需求分析并拆解成可直接执行的任务卡。\n")
	sb.WriteString("这是**分析任务**：禁止写代码、禁止创建或修改任何文件。\n")

	sb.WriteString("\n## 当前需求（唯一要分析的目标）\n")
	sb.WriteString(strings.TrimSpace(requirement))
	sb.WriteString("\n")

	sb.WriteString("\n## 最近对话（仅用于理解追问，可能为空）\n")
	if h := strings.TrimSpace(history); h != "" {
		sb.WriteString(h)
		sb.WriteString("\n")
	} else {
		sb.WriteString("（无）\n")
	}

	sb.WriteString("\n## 看板上下文（落卡用：列 id / 已有卡 / 可用项目）\n")
	if c := strings.TrimSpace(boardContext); c != "" {
		sb.WriteString(c)
		sb.WriteString("\n")
	} else {
		sb.WriteString("（无）\n")
	}

	sb.WriteString(`
## 工作方式
- 先用只读工具（read / grep / find / ls）读取与**当前需求**相关的真实代码，确认模块、真实文件路径、接口/类型名；不要猜路径。
- 只读与当前需求相关的部分，不要通读无关模块。
- **工具调用控制在 ~12 次以内**；读够就立即输出 JSON，不要反复 verify。

## 输出契约（必须严格遵守）
- 只输出一个 JSON 数组：不要解释文字、不要前后缀、不要 markdown 代码围栏。
- 数组每个元素是一张任务卡，字段：
  {"title": string(必填，简洁明确), "description": string(必填), "priority": "low"|"medium"|"high"|"urgent", "target_name": string(必须来自上面「可用项目」列表，不得编造), "checklist": string[](3-8 条可验证步骤), "estimate_hours": number, "list_id": number, "depends_on_index": number[]}
- description 必须具体可执行：目标 + 涉及的真实文件/模块 + 验收标准 + 约束。用中文。
- 一张卡 = 一个可独立评审/合并/部署的交付物，预计工时 ≤ 3 小时；更大的需求必须拆成多张卡。
- 不要过度拆分：每张卡都必须有可测试的价值。
- depends_on_index 是本批卡片内 1-based 序号（如 [1,2]），表示必须先完成哪些卡。
`)
	if listID > 0 {
		sb.WriteString(fmt.Sprintf("- 未指定 list_id 时默认落到看板列 id=%d。\n", listID))
	}
	sb.WriteString("\n现在开始：先按需读取真实代码，然后只输出 JSON 数组。\n")
	return sb.String()
}
