package board

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/computeflux/ai-flow/pkg/agent"
	"github.com/computeflux/ai-flow/pkg/bus"
	"github.com/computeflux/ai-flow/pkg/logger"
)

// ChatPath is the gateway route mounted for the board AI sidebar.
const ChatPath = "/chat"

// Options configures the board chat HTTP handler.
type Options struct {
	// InboundTimeout bounds publishing the inbound message onto the bus.
	InboundTimeout time.Duration
	// KeepAliveInterval is the SSE comment ping cadence.
	KeepAliveInterval time.Duration
}

// Handler serves POST /chat: one board chat request = one SSE stream. The
// request body carries {board_id, session, user_id, content}; the caller's
// Authorization Bearer JWT is forwarded to workflow tools through the turn
// context (see Turn / ContextWithTurn + IdentityHook) and the workflow
// client's identity resolution.
type Handler struct {
	channel *Channel
	bus     *bus.MessageBus

	inboundTimeout time.Duration
	keepAlive      time.Duration

	pumpMu sync.Mutex
	pumps  map[*Turn]context.CancelFunc
}

var _ http.Handler = (*Handler)(nil)

// NewHandler creates the POST /chat handler for a board channel.
func NewHandler(ch *Channel, b *bus.MessageBus, opts Options) *Handler {
	h := &Handler{channel: ch, bus: b, pumps: make(map[*Turn]context.CancelFunc)}
	h.inboundTimeout = opts.InboundTimeout
	if h.inboundTimeout <= 0 {
		h.inboundTimeout = 10 * time.Second
	}
	h.keepAlive = opts.KeepAliveInterval
	if h.keepAlive <= 0 {
		h.keepAlive = 15 * time.Second
	}
	return h
}

// Mount mounts the channel's outbound relay and returns its HTTP handler for
// ChatPath (implements channels.WebhookHandler so it can be registered on
// the channels manager / health server mux). The manager calls Start on
// channel start; ServeHTTP handles POST /chat.
func (h *Handler) Mount(m *bus.MessageBus) (http.Handler, string) {
	return h, ChatPath
}

// WebhookPath implements channels.WebhookHandler.
func (h *Handler) WebhookPath() string { return ChatPath }

// Start starts the channel's outbound relay goroutine.
func (h *Handler) Start(ctx context.Context) error {
	return h.channel.Start(ctx)
}

// Stop stops the channel and detaches active turns.
func (h *Handler) Stop(ctx context.Context) error {
	return h.channel.Stop(ctx)
}

type chatRequest struct {
	BoardID int    `json:"board_id"`
	Session string `json:"session"`
	UserID  string `json:"user_id"`
	Content string `json:"content"`
}

// ServeHTTP implements the POST /chat SSE endpoint.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeHTTPError(w, http.StatusMethodNotAllowed, "method not allowed, use POST")
		return
	}

	var req chatRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeHTTPError(w, http.StatusBadRequest, "invalid json body: "+err.Error())
		return
	}
	req.Session = strings.TrimSpace(req.Session)
	req.UserID = strings.TrimSpace(req.UserID)
	req.Content = strings.TrimSpace(req.Content)
	if req.BoardID <= 0 || req.Session == "" || req.UserID == "" || req.Content == "" {
		writeHTTPError(w, http.StatusBadRequest, "board_id, session, user_id and content are required")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeHTTPError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	jwt := bearerFromHeader(r.Header.Get("Authorization"))

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// SSE turns run far longer than the gateway server's global WriteTimeout
	// (30s): clear the per-response write deadline so long generations are
	// not cancelled mid-stream. The turn's own watchdog bounds the stream.
	if rc := http.NewResponseController(w); rc != nil {
		if err := rc.SetWriteDeadline(time.Time{}); err != nil {
			logger.WarnCF("board", "clear SSE write deadline failed", map[string]any{"error": err.Error()})
		}
	}
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	chatID := "u" + req.UserID
	turnCtx := NewTurn(chatID, jwt)
	turnCtx.SetSessionKey(SessionKey(req.BoardID, req.Session))

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	release := h.channel.AttachTurn(chatID, turnCtx)
	defer func() {
		release()
	}()

	// Publish the inbound message carrying the request-scoped turn context:
	// the agent loop consumes it from InboundChan with this same ctx and
	// propagates it into the turn's execution + tool contexts, so every
	// outbound publish and tool execution for this request resolves back to
	// this turn (see pkg/agent pipeline). The IdentityHook also reads the
	// relay identity from the hook ctx.
	pubCtx, pubCancel := context.WithTimeout(
		ContextWithTurn(ctx, turnCtx),
		h.inboundTimeout,
	)
	pubErr := h.bus.PublishInbound(pubCtx, NewInbound(req.BoardID, req.Session, req.UserID, req.Content))
	pubCancel()
	if pubErr != nil {
		fmt.Fprintf(w, "%s", string(EncodeFrame(Frame{Type: "error", Error: fmt.Sprintf("publish inbound failed: %v", pubErr)})))
		flusher.Flush()
		return
	}

	// Per-request relay: this stream only observes outbound messages whose
	// context carries its own turn (ValueCtx). A single shared relay would
	// resolve "the active turn for the chat", which is racy when concurrent
	// turns share a chat (steering continuations, queued follow-ups).
	relaySub := h.bus.SubscribeOutbound(64)
	defer relaySub.Close()

	// Pump: single writer for this stream. Drains the turn's frame queue on
	// notifications and terminates when the turn is done (streamer finalize
	// / final outbound / watchdog / client disconnect).
	pumpCtx, pumpCancel := context.WithCancel(context.Background())
	h.registerPump(turnCtx, pumpCancel)
	go func() {
		defer h.unregisterPump(turnCtx)
		h.pump(pumpCtx, turnCtx, w, flusher)
		cancel() // unblock the SSE loop below when the pump terminates
	}()
	defer pumpCancel()

	// Relay goroutine: deliver this request's turn-scoped outbounds.
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		h.channel.RelayTurn(turnCtx, relaySub)
	}()
	defer func() { relaySub.Close(); <-relayDone }()

	// Watchdog: hard bound on one turn.
	watchdog := time.NewTimer(TurnTimeout)
	defer watchdog.Stop()

	// Keep-alive ticker for the SSE connection.
	keepAlive := time.NewTicker(h.keepAlive)
	defer keepAlive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-watchdog.C:
			turnCtx.Finish("", "board chat turn timeout")
			// The pump emits the error result and cancels itself; give it a
			// grace window before we return (which ends the response).
			select {
			case <-pumpCtx.Done():
			case <-time.After(5 * time.Second):
			}
			return
		case <-keepAlive.C:
			turnCtx.SendKeepAlive()
		}
	}
}

// registerPump records the turn's pump cancel func so the channel relay can
// wake it (e.g. to deliver a keep-alive).
func (h *Handler) registerPump(t *Turn, cancel context.CancelFunc) {
	h.pumpMu.Lock()
	defer h.pumpMu.Unlock()
	h.pumps[t] = cancel
}

func (h *Handler) unregisterPump(t *Turn) {
	h.pumpMu.Lock()
	defer h.pumpMu.Unlock()
	delete(h.pumps, t)
}

// pump drains the turn's frame queue and terminates the stream. It is woken
// by turn notifications (frames enqueued / terminal signal). On the
// terminal signal it emits the result frame (final content + captured
// mutation calls) — or an error frame — and closes.
func (h *Handler) pump(
	ctx context.Context,
	turn *Turn,
	w http.ResponseWriter,
	flusher http.Flusher,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-turn.notify:
		}

	drain:
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			frames, done, final, errMsg := turn.drainFrames()

			if done {
				// Drain any frames the relay/streamer enqueued concurrently with
				// the terminal signal (tool feedback and late token frames must
				// not be dropped by the result frame). The relay runs on its own
				// goroutine, so give in-flight messages a short grace window to
				// land in the queue before the stream closes.
				grace := time.After(150 * time.Millisecond)
			drained:
				for {
					for {
						extra, _, extraFinal, extraErr := turn.drainFrames()
						if len(extra) > 0 {
							frames = append(frames, extra...)
						}
						// Only a non-empty terminal payload may override the terminal
						// signal captured with the first drain: late auxiliary frames
						// (tool feedback) that land in the queue during the grace window
						// return empty final/errMsg and must not evict the final content.
						if extraFinal != "" {
							final = extraFinal
						}
						if extraErr != "" {
							errMsg = extraErr
						}
						if len(extra) == 0 {
							break
						}
					}
					select {
					case <-turn.notify:
						continue drained
					case <-grace:
						break drained
					case <-ctx.Done():
						break drained
					}
				}
			}

			for _, f := range frames {
				if f.Type == "keepalive" {
					if _, err := w.Write(EncodeComment(": ping")); err != nil {
						return
					}
					flusher.Flush()
					continue
				}
				if _, err := w.Write(EncodeFrame(f)); err != nil {
					return
				}
				flusher.Flush()
			}

			if done {
				calls := turn.takeCalls()
				var finalFrame Frame
				if errMsg != "" {
					finalFrame = Frame{Type: "error", Error: errMsg, Calls: calls}
				} else {
					content := final
					if content == "" {
						content = lastTokenContent(frames)
					}
					if content == "" && len(calls) > 0 {
						content = "已生成变更提案，请在下方确认后落卡。"
					}
					finalFrame = Frame{Type: "result", Content: content, Calls: calls}
				}
				if _, err := w.Write(EncodeFrame(finalFrame)); err != nil {
					return
				}
				flusher.Flush()
				return
			}

			if len(frames) == 0 {
				break drain
			}
		}
	}
}

// lastTokenContent returns the concatenated token text from a drained frame
// batch (used as the result frame's content when the turn completed without
// a streamer). It looks at the whole queue snapshot, not just the last
// frame.
func lastTokenContent(frames []Frame) string {
	var sb strings.Builder
	for _, f := range frames {
		if f.Type == "token" {
			sb.WriteString(f.Text)
		}
	}
	return sb.String()
}

func writeHTTPError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func bearerFromHeader(header string) string {
	const prefix = "Bearer "
	if len(header) >= len(prefix) && strings.EqualFold(header[:len(prefix)], prefix) {
		return strings.TrimSpace(header[len(prefix):])
	}
	return ""
}

// ParseBoardID parses a positive board id.
func ParseBoardID(s string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// IdentityHook: turn context → workflow tool context (BeforeTool hook)
// ---------------------------------------------------------------------------

// IdentityHook injects the caller identity + JWT into tool execution
// contexts for board turns, so pkg/tools/workflow sees the logged-in user
// (workflow.WithIdentity / workflow.ContextWithJWT). It is an in-process
// hook interceptor mounted on the agent loop: for non-board turns (no turn
// in the hook ctx) it is a no-op, so the global chat surface is untouched.
type IdentityHook struct {
	channel *Channel
}

var _ agent.ToolInterceptor = (*IdentityHook)(nil)

// NewIdentityHook creates the identity hook bound to a board channel.
func NewIdentityHook(ch *Channel) *IdentityHook { return &IdentityHook{channel: ch} }

// BeforeTool implements agent.ToolInterceptor.
func (h *IdentityHook) BeforeTool(
	ctx context.Context,
	call *agent.ToolCallHookRequest,
) (*agent.ToolCallHookRequest, agent.HookDecision, error) {
	if call == nil {
		return call, agent.HookDecision{}, nil
	}

	t := TurnFromContext(ctx)
	if t == nil {
		// Not a board turn (no relay context on this call stack): no-op.
		return call, agent.HookDecision{Action: agent.HookActionContinue}, nil
	}

	userID := ChatUserID(t.ChatID)
	if userID == "" && call.Context != nil && call.Context.Inbound != nil {
		userID = call.Context.Inbound.SenderID
	}
	jwt := t.JWT
	if jwt == "" {
		jwt = h.channel.JWTFor(t.ChatID)
	}

	// Stamp the resolved identity onto the hook request. The pipeline
	// threads the decorated request into the tool execution context (see
	// pkg/agent pipeline_execute), where pkg/tools/workflow resolves it.
	next := call.Clone()
	if next.Context == nil {
		next.Context = &agent.TurnContext{}
	}
	next.Context.Inbound = &bus.InboundContext{
		Channel:  ChannelName,
		ChatID:   t.ChatID,
		SenderID: userID,
		Raw: map[string]string{
			"board_jwt":     jwt,
			"board_user_id": userID,
		},
	}
	return next, agent.HookDecision{Action: agent.HookActionModify}, nil
}

// AfterTool implements agent.ToolInterceptor (no-op).
func (h *IdentityHook) AfterTool(
	ctx context.Context,
	res *agent.ToolResultHookResponse,
) (*agent.ToolResultHookResponse, agent.HookDecision, error) {
	return res, agent.HookDecision{}, nil
}
