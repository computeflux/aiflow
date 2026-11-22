// Package main is the matrix-bot binary.
//
// matrix-bot：S4 审批 Bot（docs/plan/S4-协作底座.md §3.2 / §3.3）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type config struct {
	Homeserver string // MATRIX_HOMESERVER，如 http://synapse:8008
	BotUser    string // MATRIX_BOT_USER，如 @approval-bot:dev-workflow.computeflux.ai
	BotToken   string // MATRIX_BOT_TOKEN
	RoomAlias  string // MATRIX_ROOM_APPROVALS，如 approvals
	APIURL     string // MATRIX_API_URL，如 http://workflow-api:8001
	HealthPort string // MATRIX_BOT_HEALTH_PORT，默认 8126
	SyncMS     int    // MATRIX_BOT_SYNC_TIMEOUT_MS，默认 30000
}

func loadConfig() *config {
	return &config{
		Homeserver: strings.TrimRight(os.Getenv("MATRIX_HOMESERVER"), "/"),
		BotUser:    os.Getenv("MATRIX_BOT_USER"),
		BotToken:   os.Getenv("MATRIX_BOT_TOKEN"),
		RoomAlias:  envDefault("MATRIX_ROOM_APPROVALS", "approvals"),
		APIURL:     strings.TrimRight(os.Getenv("MATRIX_API_URL"), "/"),
		HealthPort: envDefault("MATRIX_BOT_HEALTH_PORT", "8126"),
		SyncMS:     envIntDefault("MATRIX_BOT_SYNC_TIMEOUT_MS", 30000),
	}
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envIntDefault(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// bot 是审批 Bot 主体。
type bot struct {
	cfg       *config
	client    *http.Client
	api       *http.Client
	roomID    string              // #approvals 解析后的 room ID
	nextBatch string              // sync 游标
	firstSync bool                // 首次 sync 只取游标，不处理存量事件
	seen      map[string]struct{} // event_id 去重
	seenMu    sync.Mutex
	seenCap   int
}

var cmdRe = regexp.MustCompile(`^\s*(approve|reject)\s+(\d+)(?:\s+(.*))?$`)

func main() {
	_ = godotenvSafeLoad()
	cfg := loadConfig()
	if cfg.Homeserver == "" || cfg.BotUser == "" || cfg.BotToken == "" || cfg.APIURL == "" {
		log.Fatalf("matrix-bot: MATRIX_HOMESERVER / MATRIX_BOT_USER / MATRIX_BOT_TOKEN / MATRIX_API_URL 必填")
	}

	// health 探针（compose healthcheck）
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintln(w, "ok")
		})
		log.Printf("matrix-bot health on 127.0.0.1:%s", cfg.HealthPort)
		log.Fatal(http.ListenAndServe("127.0.0.1:"+cfg.HealthPort, mux))
	}()

	b := &bot{
		cfg:       cfg,
		client:    &http.Client{Timeout: time.Duration(cfg.SyncMS+15000) * time.Millisecond},
		api:       &http.Client{Timeout: 15 * time.Second},
		firstSync: true,
		seen:      map[string]struct{}{},
		seenCap:   2048,
	}
	if err := b.resolveRoom(); err != nil {
		log.Fatalf("resolve approvals room: %v", err)
	}
	log.Printf("matrix-bot started: bot=%s room=%s api=%s", cfg.BotUser, b.roomID, cfg.APIURL)
	b.run()
}

// godotenvSafeLoad 加载 .env（容器内 /app/.env 挂载；失败不致命，env 已由 compose 注入）。
func godotenvSafeLoad() error {
	if data, err := os.ReadFile(".env"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if k != "" {
				_ = os.Setenv(k, v)
			}
		}
	}
	return nil
}

// resolveRoom 把房间 alias 解析为 room ID。
func (b *bot) resolveRoom() error {
	u, err := url.Parse(b.cfg.Homeserver)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid homeserver %q", b.cfg.Homeserver)
	}
	serverName := os.Getenv("SYNAPSE_SERVER_NAME") // 规范 server name（不带端口）
	if serverName == "" {
		serverName = u.Hostname()
	}
	alias := "#" + b.cfg.RoomAlias + ":" + serverName
	resp, err := b.httpGet("/_matrix/client/v3/directory/room/" + url.PathEscape(alias))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("room lookup %s: HTTP %d（房间不存在？先建房间）", alias, resp.StatusCode)
	}
	var out struct {
		RoomID string `json:"room_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.RoomID == "" {
		return fmt.Errorf("room lookup %s: empty room_id", alias)
	}
	b.roomID = out.RoomID
	return nil
}

// run 长轮询 /sync 主循环（401/5xx/网络错误 → 退避 30s 重试）。
func (b *bot) run() {
	for {
		err := b.syncOnce()
		if err == nil {
			continue
		}
		log.Printf("matrix-bot sync error: %v（30s 后重试）", err)
		time.Sleep(30 * time.Second)
	}
}

type syncResp struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events []struct {
					EventID string `json:"event_id"`
					Sender  string `json:"sender"`
					Type    string `json:"type"`
					Content struct {
						MsgType string `json:"msgtype"`
						Body    string `json:"body"`
					} `json:"content"`
				} `json:"events"`
			} `json:"timeline"`
		} `json:"join"`
	} `json:"rooms"`
}

func (b *bot) syncOnce() error {
	q := url.Values{}
	q.Set("timeout", fmt.Sprintf("%d", b.cfg.SyncMS))
	if b.nextBatch != "" {
		q.Set("since", b.nextBatch)
	}
	resp, err := b.httpGet("/_matrix/client/v3/sync?" + q.Encode())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("HTTP 401（MATRIX_BOT_TOKEN 无效？）")
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var out syncResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode sync: %w", err)
	}
	if out.NextBatch == "" {
		return fmt.Errorf("sync: empty next_batch")
	}
	b.nextBatch = out.NextBatch
	if b.firstSync {
		b.firstSync = false // 首次 sync 含存量事件，不处理，只取游标
		return nil
	}
	room, ok := out.Rooms.Join[b.roomID]
	if !ok {
		return nil
	}
	for _, e := range room.Timeline.Events {
		if e.Type != "m.room.message" || e.Content.MsgType != "m.text" {
			continue
		}
		if e.Sender == b.cfg.BotUser {
			continue
		}
		if !b.markSeen(e.EventID) {
			continue
		}
		go b.handle(e.Sender, e.Content.Body)
	}
	return nil
}

// markSeen 去重（有上限，超了整体清空——消息重放风险可接受，重复审批会被 api 侧 409 挡掉）。
func (b *bot) markSeen(eventID string) bool {
	if eventID == "" {
		return true
	}
	b.seenMu.Lock()
	defer b.seenMu.Unlock()
	if _, ok := b.seen[eventID]; ok {
		return false
	}
	b.seen[eventID] = struct{}{}
	if len(b.seen) > b.seenCap {
		b.seen = map[string]struct{}{}
		b.seen[eventID] = struct{}{}
	}
	return true
}

// handle 解析 `approve|reject <id> [reason]` 并调 workflow-api。
func (b *bot) handle(sender, body string) {
	m := cmdRe.FindStringSubmatch(body)
	if m == nil {
		return // 非指令消息忽略
	}
	action, id := m[1], m[2]
	reason := strings.TrimSpace(m[3])
	localpart := sender
	if i := strings.Index(localpart, ":"); i > 0 {
		localpart = localpart[:i]
	}
	ok, err := b.decide(action, id, localpart, reason)
	if err != nil {
		_ = b.reply(fmt.Sprintf("❌ %s #%s 失败：%s", action, id, err))
		return
	}
	if !ok {
		_ = b.reply(fmt.Sprintf("❌ %s #%s 被拒（审批单不存在 / 已决策）", action, id))
		return
	}
	mark := "✅ approved"
	if action == "reject" {
		mark = "⛔ rejected"
	}
	_ = b.reply(fmt.Sprintf("%s #%s by %s（CD 已触发，见 audit_logs）", mark, id, sender))
}

// decide 调 workflow-api 审批接口（无鉴权，approver=matrix 用户；api 侧留 audit_logs + 触发 CD）。
func (b *bot) decide(action, id, approver, reason string) (bool, error) {
	if reason == "" {
		reason = "via matrix"
	}
	payload, _ := json.Marshal(map[string]string{"approver": approver, "reason": reason})
	endpoint := b.cfg.APIURL + fmt.Sprintf("/approvals/%s/%s", id, action)
	resp, err := b.api.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return false, fmt.Errorf("api %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if resp.StatusCode/100 != 2 {
		var out struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if out.Error == "" {
			out.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return false, fmt.Errorf("%s", out.Error)
	}
	return true, nil
}

// reply 在 #approvals 房间回一条 m.text 消息。
func (b *bot) reply(text string) error {
	payload, _ := json.Marshal(map[string]any{"msgtype": "m.text", "body": text})
	txn := fmt.Sprintf("%d-%d", time.Now().UnixNano(), len(b.seen))
	resp, err := b.httpPut("/_matrix/client/v3/rooms/"+url.PathEscape(b.roomID)+"/send/m.room.message/"+txn, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("reply: HTTP %d", resp.StatusCode)
	}
	return nil
}

// httpGet 带 token 的 GET（homeserver）。
func (b *bot) httpGet(path string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, b.cfg.Homeserver+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.BotToken)
	return b.client.Do(req)
}

// httpPut 带 token 的 PUT（homeserver）。
func (b *bot) httpPut(path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPut, b.cfg.Homeserver+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.BotToken)
	req.Header.Set("Content-Type", "application/json")
	return b.client.Do(req)
}
