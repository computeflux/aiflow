// Package notify 是统一告警/审批通知接口（no-op 默认实现；S4 接 Matrix）。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Matrix 是 Matrix（Synapse）通知器（S4，docs/plan/S4-协作底座.md §3.3）：
//
//	SendAlert    → #ops-alerts（前缀 [level]）
//	SendApproval → 新建/决策 → #approvals（approval.request 协议，transform-plan §10.2）
//	              过期升级 → #ops-alerts
//	SendAgentLog → #agent-log
//
// 只依赖 Client-Server API（/sync 不需要；纯发消息），stdlib only。
// 房间用 alias（#ops-alerts:<server>）首次解析为 room ID 后缓存。
type Matrix struct {
	homeserver string
	token      string
	serverName string // 从 homeserver URL host 推导（alias 需要）
	client     *http.Client

	mu        sync.Mutex
	roomCache map[string]string // alias → !roomID
}

// NewMatrix 按 MATRIX_* 环境变量构造 Matrix 通知器；配置不全返回 nil（调用方回退 no-op）。
func NewMatrix() *Matrix {
	hs := strings.TrimRight(os.Getenv("MATRIX_HOMESERVER"), "/")
	token := os.Getenv("MATRIX_BOT_TOKEN")
	if hs == "" || token == "" {
		return nil
	}
	u, err := url.Parse(hs)
	if err != nil || u.Host == "" {
		log.Printf("notify: invalid MATRIX_HOMESERVER %q", hs)
		return nil
	}
	serverName := os.Getenv("SYNAPSE_SERVER_NAME") // 规范 server name（不带端口）
	if serverName == "" {
		serverName = u.Hostname()
	}
	return &Matrix{
		homeserver: hs,
		token:      token,
		serverName: serverName,
		client:     &http.Client{Timeout: 10 * time.Second},
		roomCache:  map[string]string{},
	}
}

func (m *Matrix) roomName(which string) string {
	switch which {
	case "alerts":
		return firstNonEmpty(os.Getenv("MATRIX_ROOM_ALERTS"), "ops-alerts")
	case "approvals":
		return firstNonEmpty(os.Getenv("MATRIX_ROOM_APPROVALS"), "approvals")
	default: // agent-log
		return firstNonEmpty(os.Getenv("MATRIX_ROOM_AGENT_LOG"), "agent-log")
	}
}

// alias 形如 #ops-alerts:dev-workflow.computeflux.ai（server name 取 homeserver host）。
func (m *Matrix) alias(which string) string {
	return "#" + m.roomName(which) + ":" + m.serverName
}

// lookupRoomID 解析 alias → room ID（缓存；解析失败返回错误，发送失败不静默吞掉）。
func (m *Matrix) lookupRoomID(ctx context.Context, which string) (string, error) {
	alias := m.alias(which)
	m.mu.Lock()
	id, ok := m.roomCache[alias]
	m.mu.Unlock()
	if ok {
		return id, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.homeserver+"/_matrix/client/v3/directory/room/"+url.PathEscape(alias), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("matrix room lookup %s: %w", alias, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("matrix room lookup %s: HTTP %d", alias, resp.StatusCode)
	}
	var out struct {
		RoomID string `json:"room_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.RoomID == "" {
		return "", fmt.Errorf("matrix room lookup %s: empty room_id (房间不存在？)", alias)
	}
	m.mu.Lock()
	m.roomCache[alias] = out.RoomID
	m.mu.Unlock()
	return out.RoomID, nil
}

// send 发一条 m.text 消息（txn id 时间戳+随机，保证幂等重试可用）。
func (m *Matrix) send(ctx context.Context, which, body string) error {
	id, err := m.lookupRoomID(ctx, which)
	if err != nil {
		return err
	}
	payload := map[string]any{"msgtype": "m.text", "body": body}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		m.homeserver+"/_matrix/client/v3/rooms/"+url.PathEscape(id)+
			"/send/m.room.message/"+fmt.Sprintf("%d-%d", time.Now().UnixNano(), randUint32()),
		bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("matrix send: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("matrix send: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (m *Matrix) SendAlert(ctx context.Context, a Alert) error {
	body := fmt.Sprintf("[%s] %s\n%s", strings.ToUpper(a.Level), a.Title, a.Body)
	return m.send(ctx, "alerts", body)
}

func (m *Matrix) SendAgentLog(ctx context.Context, msg string) error {
	return m.send(ctx, "agent-log", msg)
}

// SendApproval：Expired → #ops-alerts 升级；Status 为空 → #approvals approval.request 协议；
// approved/rejected → #approvals 决策确认。
func (m *Matrix) SendApproval(ctx context.Context, n ApprovalNotice) error {
	if n.Expired {
		return m.send(ctx, "alerts", fmt.Sprintf(
			"[warning] 审批单超时失效 %s（%s）：%s —— 无默认通过，需重新发起",
			n.ApprovalNo, n.Kind, n.Subject))
	}
	if n.Status == "" {
		req := map[string]any{
			"type":        "approval.request",
			"approval_id": n.ID,
			"title":       n.Subject,
			"actions":     []string{"approve", "reject"},
			"expires_at":  n.ExpiresAt.Format(time.RFC3339),
		}
		if n.Summary != "" {
			req["diff"] = n.Summary
		}
		b, _ := json.Marshal(req)
		return m.send(ctx, "approvals", string(b))
	}
	mark := "⛔"
	if n.Status == "approved" {
		mark = "✅"
	}
	return m.send(ctx, "approvals", fmt.Sprintf(
		"%s 审批单 %s（%s）%s：%s（approver=%s）",
		mark, n.ApprovalNo, n.Kind, n.Status, n.Subject, n.Approver))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func randUint32() uint64 {
	// 足够用的伪随机（txn id 只需唯一，不需要密码学强度）
	return uint64(time.Now().UnixNano()) % 0xFFFFFF
}
