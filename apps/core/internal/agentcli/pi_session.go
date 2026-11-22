package agentcli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pi_session.go：pi-web 会话 HTTP 客户端与会话文件解析（从 pi.go 拆出，纯机械搬迁）。

// SessionFileByID 按会话 ID 找 pi-web 会话文件（找不到返回空串）。
func SessionFileByID(sessionDir, id string) string {
	root := filepath.Join(sessionDir, "sessions")
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.Contains(d.Name(), id) {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// LastMessageTerminal 判断会话文件最后一条 message 是否为「本轮终结的 assistant」
// （扫尾部若干条，跳过非 message 条目）。
func LastMessageTerminal(path string) bool {
	role, stop := "", ""
	hasToolCall := false
	for _, line := range tailLines(path, 10) {
		var entry struct {
			Message *struct {
				Role       string          `json:"role"`
				StopReason string          `json:"stopReason"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Message == nil {
			continue
		}
		role = entry.Message.Role
		stop = entry.Message.StopReason
		hasToolCall = contentHasToolCall(entry.Message.Content)
	}
	return role == "assistant" && stop != "toolUse" && !hasToolCall
}

// contentHasToolCall content 为数组且含 type=toolCall 的条目。
func contentHasToolCall(content json.RawMessage) bool {
	if len(content) == 0 {
		return false
	}
	var parts []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return false
	}
	for _, part := range parts {
		if part.Type == "toolCall" {
			return true
		}
	}
	return false
}

// countToolCalls 统计会话尾窗内的工具调用总数与写操作次数（用于「只读不写」判定）。
// 只读尾部 1MiB（会话文件可能很大）；文件只追加，若写操作发生在窗口外，
// 调用方还有 HasChanges(workdir) 兜底，不会误判。
func countToolCalls(path string) (calls, writes int) {
	for _, line := range tailLines(path, 4000) {
		var entry struct {
			Message *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &entry) != nil || entry.Message == nil {
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if json.Unmarshal(entry.Message.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if part.Type != "toolCall" {
				continue
			}
			calls++
			if writeToolNames[strings.ToLower(part.Name)] {
				writes++
			}
		}
	}
	return calls, writes
}

// messageText content → 纯文本（字符串直取；数组拼接 text 片段）。
func messageText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var texts []string
	for _, part := range parts {
		if part.Type == "text" && part.Text != "" {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, " ")
}

// tailLines 读文件末尾最多 n 行（只读尾部 1MiB，避免大会话文件全量读取）。
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	const window = 1 << 20
	size := st.Size()
	offset := int64(0)
	if size > window {
		offset = size - window
	}
	buf := make([]byte, size-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil
	}
	lines := strings.Split(string(buf), "\n")
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:] // 丢掉窗口起点处的半行
	}
	out := make([]string, 0, n)
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// fileSize 文件大小（读不到返回 0）。
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// ---- pi-web HTTP 客户端 ----

// piWeb pi-web API 客户端（Basic Auth，用户名固定 pi）。
type piWeb struct {
	base     string
	password string
	client   *http.Client
}

func newPiWeb(env PiEnv) *piWeb {
	return &piWeb{
		base:     strings.TrimRight(env.WebURL, "/"),
		password: env.Password,
		// 单请求超时：接口本身是非阻塞的（提交即返回），留足余量防网络抖动挂死。
		client: &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *piWeb) post(ctx context.Context, path string, payload any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.password != "" {
		req.SetBasicAuth("pi", c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// HTTP 状态码不进 error：错误体（如 "Session not found"）由调用方按内容判断，
	// 与旧实现（curl 不带 -f）行为一致。
	return string(body), nil
}

// get GET pi-web API（带 Basic Auth），返回响应体。状态码不进 error（同 post）。
func (c *piWeb) get(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	if c.password != "" {
		req.SetBasicAuth("pi", c.password)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(body), nil
}

// queueFollowUpCount 查 pi-web 会话的 followUp 排队消息数（GET /api/agent/:id →
// data.queuedMessages.followUp）。任何失败返回 -1（调用方按「无队列」处理，催促照发）。
func (c *piWeb) queueFollowUpCount(ctx context.Context, sessionID string) int {
	body, err := c.get(ctx, "/api/agent/"+url.PathEscape(sessionID))
	if err != nil {
		return -1
	}
	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			QueuedMessages struct {
				FollowUp []string `json:"followUp"`
			} `json:"queuedMessages"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &resp) != nil || !resp.Success {
		return -1
	}
	return len(resp.Data.QueuedMessages.FollowUp)
}

// sendFollowUp 向已有会话追加消息（排队，不拒绝）。
func (c *piWeb) sendFollowUp(ctx context.Context, sessionID, message string) (string, error) {
	return c.post(ctx, "/api/agent/"+url.PathEscape(sessionID), map[string]any{
		"type":              "prompt",
		"message":           message,
		"streamingBehavior": "followUp",
	})
}

// createSession 新建会话（完整提示词 + cwd + provider/model），返回 session ID。
func (c *piWeb) createSession(ctx context.Context, cwd, message, provider, model string) (string, error) {
	body, err := c.post(ctx, "/api/agent/new", map[string]any{
		"cwd":      cwd,
		"type":     "prompt",
		"message":  message,
		"provider": provider,
		"modelId":  model,
	})
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

// sessionIDFromBody 解析响应里的会话 ID（success=true 且无 sessionId 的续接响应回退用 fallback）。
func sessionIDFromBody(body, fallback string) string {
	var resp struct {
		Success   bool   `json:"success"`
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal([]byte(body), &resp) != nil || !resp.Success {
		return ""
	}
	if resp.SessionID != "" {
		return resp.SessionID
	}
	return fallback
}
