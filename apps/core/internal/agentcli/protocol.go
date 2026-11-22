// Package agentcli 实现 Agent adapter 的统一 JSON 协议（stdin → stdout）。
//
// 角色（coder / reviewer）与实现（pi / ocr / ...）分离：evolve-driver 只依赖协议与
// 角色接口，不 import 任何具体 Agent（见 apps/core/agents/README.md §2、§5）。
//
// 历史实现是 bash（adapter.sh）+ 内联 python3（ocr 侧 parse.py），三类稳定性问题促成本包：
//  1. bash 流式读取脚本 → 运行中改脚本会让进程读到**错位内容**（改文件即事故，无法安全热修）；
//  2. JSON 组装/解析靠 heredoc + python3 → 字段漂移**静默丢内容**（2026-10-17 事故：
//     ocr v1.13 评论正文键换成 content，旧解析只认 message → 返工反馈全空，白烧一轮）；
//  3. 超时 / HTTP 重试 / 子进程回收 / 多仓库 diff 靠 shell 拼接，无法单测。
//
// 协议不变（adapter 入口从 *.sh 换成 Go 二进制，driver 只需换路径）：
//
//	coder   : {"op":"fix","task":...,"feedback":...,"workdir":...,"files":[...],
//	           "kind":"bug|requirement","session_id":"..."}
//	          → {"ok":true,"diff":...,"files_changed":[...],"session_id":...}
//	reviewer: {"op":"review","diff":...,"workdir":...,"base":"main"}
//	          → {"ok":true,"comments":[{"file","line","level","message"}]}
//	失败一律 {"ok":false,"reason":"..."} + exit 1（driver 优先取 reason 作为错误原因）。
package agentcli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// FixInput coder（op=fix）输入协议。
type FixInput struct {
	Op        string   `json:"op"`
	Task      string   `json:"task"`
	Feedback  string   `json:"feedback"` // 上一轮反馈（empty diff / reviewer error）：仅续接会话时发送
	Workdir   string   `json:"workdir"`
	Files     []string `json:"files"`
	Kind      string   `json:"kind"`       // bug | requirement：决定提示词策略
	SessionID string   `json:"session_id"` // 非空 = 续接已有会话
}

// FixOutput coder 输出协议（driver 侧 coderSchema 校验：required ok）。
type FixOutput struct {
	OK           bool     `json:"ok"`
	Diff         string   `json:"diff"`
	FilesChanged []string `json:"files_changed"`
	SessionID    string   `json:"session_id"`
}

// ReviewInput reviewer（op=review）输入协议。Diff 仅供协议兼容：ocr 直接评审
// workdir 的 git 状态（工作区变更 / base..HEAD），如需评审外部 diff 应先落进 workdir。
type ReviewInput struct {
	Op        string `json:"op"`
	Diff      string `json:"diff"`
	Workdir   string `json:"workdir"`
	Base      string `json:"base"`
	StartUnix int64  `json:"start_unix"` // 本轮评审启动时刻（秒）：适配器据此识别本次产生的 ocr 会话
}

// Comment 行级评论。协议只认 error|warning|info（driver 对 error 级自动打回 coder 重修）。
type Comment struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// ReviewOutput reviewer 输出协议（driver 侧 reviewerSchema 校验：required ok；
// comments[].required level+message）。
type ReviewOutput struct {
	OK       bool      `json:"ok"`
	Comments []Comment `json:"comments"`
	Sessions []string  `json:"sessions,omitempty"` // 本轮各仓库产生的 ocr 会话 ID（board 查看审核过程用）
}

// failReason 失败输出：driver 的 extractAdapterReason 直接取 reason 当错误原因。
type failReason struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

// protoIO 协议读写 + 失败输出（失败必须同时写 stdout 的 {"ok":false,...} 并以非 0 退出）。
type protoIO struct {
	stdout io.Writer
	stderr io.Writer
}

// fail 打印协议失败输出（stdout）并返回错误，调用方据此 exit 1。
func (p *protoIO) fail(format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	b, _ := json.Marshal(failReason{OK: false, Reason: reason})
	fmt.Fprintln(p.stdout, string(b))
	return fmt.Errorf("%s", reason)
}

// logf 进度写 stderr（driver 只把 stderr 用于错误摘要，不影响协议输出）。
func (p *protoIO) logf(format string, args ...any) {
	fmt.Fprintf(p.stderr, "[agent] "+format+"\n", args...)
}

// emit 输出成功结果（单行 JSON）。
func (p *protoIO) emit(v any) error {
	b, err := jsonCompact(v)
	if err != nil {
		return p.fail("marshal output: %v", err)
	}
	fmt.Fprintln(p.stdout, b)
	return nil
}

// readInput 读 stdin 并解析为 v。
func (p *protoIO) readInput(in io.Reader, v any) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return p.fail("read stdin: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return p.fail("input not JSON: %v", err)
	}
	return nil
}

// ---- 小工具 ----

// jsonCompact 序列化 JSON（不转义 HTML：评论文本含 < > & 时应保持原样可读）。
func jsonCompact(v any) (string, error) {
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// envOr 取环境变量，空则用默认值。
func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envInt 取整型环境变量，非法/空则用默认值。
func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// tail 取字符串末尾 n 个字符（错误摘要用；中文按字节截断可接受）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// truncate 截断到 n 个字符（同 tail 的字节口径）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// text 取字符串值（非字符串返回空）。
func text(v any) string {
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// toInt 容错转 int（JSON number / 数字字符串 / nil）。
func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return 0
		}
		return int(n)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// firstNonEmpty 取第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

// firstNonZero 取第一个非零整数。
func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}
