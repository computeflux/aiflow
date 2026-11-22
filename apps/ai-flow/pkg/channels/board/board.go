// Package board implements the "board" virtual channel: the ai-flow side of
// the board frontend AI sidebar (POST /chat, SSE).
//
// Design (docs/plan/ai-sidebar-aibot-plan.md):
//   - board is an internal channel (constants.internalChannels): the manager's
//     outbound dispatcher silently skips internal channels, so every bus
//     message addressed to "board" (tool feedback / tool result / final /
//     media) is consumed by this channel's own relay goroutine. The
//     synchronous send path (Manager.SendMessage / SendToChannel →
//     Channel.Send) and the streaming path (GetStreamer → BeginStream /
//     Update / Finalize) all funnel into per-chat sinks that write SSE
//     frames.
//   - Sessions: inbound messages carry an explicit SessionKey =
//     board:<boardID>#<session>, so the agent loop keeps one history per
//     browser session across requests.
//   - Identity: chat_id = u<user_id>; the sender JWT travels with the
//     request via a per-request relay context and is injected into tool
//     execution contexts (workflow.ContextWithJWT) for pkg/tools/workflow.
//   - SSE frame protocol matches legacy (workflow-api
//     /boards/:id/ai-chat/stream): token / tool / tool_result / result /
//     error.
package board

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/computeflux/ai-flow/pkg/bus"
	"github.com/computeflux/ai-flow/pkg/channels"
	"github.com/computeflux/ai-flow/pkg/logger"
)

// ChannelName is the runtime name of the board chat channel.
const ChannelName = "board"

// TurnTimeout bounds one board chat turn (inbound publish → turn end). The
// HTTP watchdog enforces it; the channel only uses it for stale-message guards.
const TurnTimeout = 10 * time.Minute

// ---------------------------------------------------------------------------
// SSE frame types (legacy-compatible protocol)
// ---------------------------------------------------------------------------

// ToolCall is the legacy-shaped mutation tool call returned in `result`
// frames; the frontend renders confirmation cards from it and lands the
// change through workflow-api /ai-execute after human confirmation.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall mirrors the OpenAI function-call shape.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Frame is one SSE event payload.
type Frame struct {
	Type    string     `json:"type"`
	Text    string     `json:"text,omitempty"`
	Name    string     `json:"name,omitempty"`
	Args    any        `json:"args,omitempty"`
	Error   any        `json:"error,omitempty"`
	Content string     `json:"content,omitempty"`
	Calls   []ToolCall `json:"tool_calls,omitempty"`
}

// EncodeFrame serializes a frame as an SSE data block.
func EncodeFrame(f Frame) []byte {
	b, err := json.Marshal(f)
	if err != nil {
		b, _ = json.Marshal(Frame{Type: "error", Error: "frame encode failed"})
	}
	return append(append([]byte("data: "), b...), "\n\n"...)
}

// EncodeComment serializes an SSE comment line (keep-alive).
func EncodeComment(text string) []byte {
	return append(append([]byte(": "), text...), "\n\n"...)
}

// ---------------------------------------------------------------------------
// Session helpers
// ---------------------------------------------------------------------------

// SessionKey composes the explicit agent session key for a board chat.
func SessionKey(boardID int, session string) string {
	return fmt.Sprintf("board:%d#%s", boardID, session)
}

// ParseSessionKey extracts the board id from an explicit board session key.
func ParseSessionKey(key string) (int, bool) {
	if !strings.HasPrefix(key, "board:") {
		return 0, false
	}
	body := strings.TrimPrefix(key, "board:")
	idx := strings.IndexByte(body, '#')
	if idx <= 0 {
		return 0, false
	}
	id, err := strconv.Atoi(body[:idx])
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// ChatUserID extracts the user id from a board chat id ("u<user_id>").
func ChatUserID(chatID string) string {
	return strings.TrimPrefix(chatID, "u")
}

// TurnInfo returns the caller identity (user id + JWT) for a turn. Used by
// the agent loop's identity bridge.
func (t *Turn) TurnInfo() (userID, jwt string) {
	return ChatUserID(t.ChatID), t.JWT
}

// NewInbound builds the inbound message for one board chat request.
func NewInbound(boardID int, session, userID, content string) bus.InboundMessage {
	chatID := "u" + userID
	return bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:  ChannelName,
			ChatID:   chatID,
			ChatType: "direct",
			SenderID: userID,
		},
		Content:    content,
		SessionKey: SessionKey(boardID, session),
	}
}

// ---------------------------------------------------------------------------
// Turn: per-request turn bookkeeping attached by the HTTP handler
// ---------------------------------------------------------------------------

// Turn is the per-request turn bookkeeping attached by the HTTP handler.
// All bus messages, streamer callbacks and tool executions belonging to one
// POST /chat request resolve to the same *Turn:
//
//   - the HTTP handler attaches it to the request context (ContextWithTurn)
//     before publishing the inbound message, and the agent loop propagates
//     that context into the turn's execution + tool contexts (see
//     pipeline_execute.go), so mutation tools capture their calls via
//     TurnFromContext(ctx) race-free with respect to stream finalization;
//   - the channel relay / Send path looks it up from the *outgoing* message
//     context (messages are published with the turn context).
//
// Frames are buffered in a queue and written by the handler's pump goroutine
// (single writer). The streamer's Finalize/Cancel mark the turn done and
// wake the pump; the pump then emits the result frame (with captured
// mutation calls) and closes the stream.
type Turn struct {
	ChatID string
	JWT    string

	mu       sync.Mutex
	queue    []Frame
	done     bool
	final    string
	errMsg   string
	calls    []ToolCall
	callSeq  int
	streamer *streamer // set by BeginStream (configured-streaming path)

	notify chan struct{}
}

// NewTurn creates a turn for a chat id with the caller's JWT.
func NewTurn(chatID, jwt string) *Turn {
	return &Turn{ChatID: chatID, JWT: jwt, notify: make(chan struct{}, 1)}
}

type turnCtxKeyStruct struct{}

var turnCtxKey turnCtxKeyStruct

// ContextWithTurn attaches the turn to a Go context.
func ContextWithTurn(ctx context.Context, t *Turn) context.Context {
	return context.WithValue(ctx, turnCtxKey, t)
}

// TurnFromContext extracts the active turn (nil when absent).
func TurnFromContext(ctx context.Context) *Turn {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(turnCtxKey).(*Turn)
	return t
}

// enqueueFrame buffers a frame for the pump and wakes it. Frames arriving
// after the turn is done are dropped (late tool feedback, etc.).
func (t *Turn) enqueueFrame(f Frame) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.queue = append(t.queue, f)
	t.mu.Unlock()
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// SendKeepAlive asks the pump to emit an SSE keep-alive comment.
func (t *Turn) SendKeepAlive() {
	t.enqueueFrame(Frame{Type: "keepalive"})
}

// Finish marks the turn done (the pump emits the final result frame with
// any captured mutation calls, using finalContent as the result content
// when the model produced no streamed prose; a non-empty errMsg (streamer
// cancel / watchdog / relay failure) emits an error frame instead).
func (t *Turn) Finish(finalContent, errMsg string) {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	t.final = finalContent
	t.errMsg = errMsg
	t.mu.Unlock()
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// AddCall records a mutation tool call for the final result frame. Safe for
// concurrent use; called from tool Execute via the turn context.
func (t *Turn) AddCall(name string, args map[string]any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return
	}
	t.callSeq++
	argsJSON, err := json.Marshal(args)
	if err != nil {
		argsJSON = []byte("{}")
	}
	t.calls = append(t.calls, ToolCall{
		ID:       fmt.Sprintf("board_tc_%d", t.callSeq),
		Type:     "function",
		Function: FunctionCall{Name: name, Arguments: string(argsJSON)},
	})
}

// takeCalls pops the captured mutation tool calls (for the result frame).
func (t *Turn) takeCalls() []ToolCall {
	t.mu.Lock()
	defer t.mu.Unlock()
	calls := t.calls
	t.calls = nil
	return calls
}

// setStreamer attaches the configured-streaming streamer for this turn.
func (t *Turn) setStreamer(s *streamer) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.streamer = s
}

// drainFrames pops all buffered frames. The bools report the terminal state
// (set once the streamer finalized / the turn was finished), with the final
// content and error text for the result frame.
func (t *Turn) drainFrames() ([]Frame, bool, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	frames := t.queue
	t.queue = nil
	return frames, t.done, t.final, t.errMsg
}

// ---------------------------------------------------------------------------
// Channel
// ---------------------------------------------------------------------------

// Channel is the board chat virtual channel.
type Channel struct {
	bus *bus.MessageBus

	mu      sync.Mutex
	turns   map[string]*Turn // chatID → active turn (one turn per chat)
	running bool
	cancel  context.CancelFunc
	sub     *bus.OutboundSubscription
}

var _ channels.Channel = (*Channel)(nil)
var _ channels.StreamingCapable = (*Channel)(nil)

// New creates the board channel bound to a message bus and installs it as
// the package default (used by the mutation tools' session fallback).
func New(b *bus.MessageBus) *Channel {
	c := &Channel{
		bus:   b,
		turns: make(map[string]*Turn),
	}
	setDefaultChannel(c)
	return c
}

var (
	defaultMu      sync.Mutex
	defaultChannel *Channel
)

func setDefaultChannel(c *Channel) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultChannel = c
}

// DefaultChannel returns the process's board channel (nil when none was
// created). Used by the mutation tools to resolve a board turn from the
// tool session key.
func DefaultChannel() *Channel {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	return defaultChannel
}

// Name implements channels.Channel.
func (c *Channel) Name() string { return ChannelName }

// Start launches the outbound relay. Safe to call once per Start/Stop cycle.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	c.running = true
	cctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	// Fan-out subscription: the relay observes every outbound published on
	// the bus without consuming the dispatcher queues (a consume-based
	// relay races the manager dispatcher for the same messages, which
	// makes final delivery nondeterministic).
	sub := c.bus.SubscribeOutbound(bus.DefaultBufferSize())
	c.sub = sub
	c.mu.Unlock()

	go c.relay(cctx, sub)
	logger.InfoCF("board", "board chat channel started", nil)
	return nil
}

// Stop stops the relay and detaches any active turns.
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.sub != nil {
		c.sub.Close()
		c.sub = nil
	}
	c.running = false
	c.turns = make(map[string]*Turn)
	c.mu.Unlock()
	logger.InfoCF("board", "board chat channel stopped", nil)
	return nil
}

// IsRunning implements channels.Channel.
func (c *Channel) IsRunning() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.running
}

// IsAllowed / IsAllowedSender: board chats are authenticated at the HTTP
// layer (JWT required); the channel accepts every sender the gateway injects.
func (c *Channel) IsAllowed(senderID string) bool { return true }

// IsAllowedSender implements channels.Channel.
func (c *Channel) IsAllowedSender(sender bus.SenderInfo) bool { return true }

// ReasoningChannelID implements channels.Channel.
func (c *Channel) ReasoningChannelID() string { return "" }

// Send implements channels.Channel: synchronous outbound (manager direct
// sends / dispatcher). Delivery resolves the destination turn from the
// message context (the agent loop publishes with the turn context), so no
// chatID keying is involved — concurrent turns on other chats are never
// mis-delivered. Messages with no turn attached (no active /chat request)
// are acknowledged and dropped.
func (c *Channel) Send(ctx context.Context, msg bus.OutboundMessage) ([]string, error) {
	if t := TurnFromContext(ctx); t != nil {
		c.deliver(ctx, t, msg)
	}
	return []string{"board"}, nil
}

// SendMedia implements channels.MediaSender: synchronous media delivery to
// the active turn's sink (manager sendMediaWithRetry path).
func (c *Channel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) ([]string, error) {
	t := TurnFromContext(ctx)
	if t == nil {
		return nil, fmt.Errorf("board: no active turn for chat %s", msg.ChatID)
	}
	refs := make([]string, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		refs = append(refs, part.Ref)
	}
	t.enqueueFrame(mediaFrame(refs))
	return []string{"board"}, nil
}

// ---------------------------------------------------------------------------
// Outbound relay: observe bus messages addressed to this channel (fan-out)
// ---------------------------------------------------------------------------

func (c *Channel) relay(ctx context.Context, sub *bus.OutboundSubscription) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-sub.Outbound:
			if !ok {
				return
			}
			if msg.Context.Channel != ChannelName && msg.Channel != ChannelName {
				continue // not ours; the manager dispatcher delivers it
			}
			if t := TurnFromContext(msg.Context.ValueCtx); t != nil {
				c.deliver(msg.Context.ValueCtx, t, msg)
			}
		case media, ok := <-sub.OutboundMedia:
			if !ok {
				return
			}
			if media.Context.Channel != ChannelName && media.Channel != ChannelName {
				continue
			}
			if t := TurnFromContext(media.Context.ValueCtx); t != nil {
				refs := make([]string, 0, len(media.Parts))
				for _, part := range media.Parts {
					refs = append(refs, part.Ref)
				}
				t.enqueueFrame(mediaFrame(refs))
			}
		}
	}
}

// mediaFrame maps media refs to a token frame with attachment lines (the
// board UI has no media cards; legacy parity).
func mediaFrame(refs []string) Frame {
	var sb strings.Builder
	for _, ref := range refs {
		sb.WriteString("📎 ")
		sb.WriteString(ref)
		sb.WriteString("\n")
	}
	return Frame{Type: "token", Text: sb.String()}
}

// deliver maps one outbound bus message onto the turn's frame queue. The
// configured-streaming path already emits token frames through the streamer
// (BeginStream → Update/Finalize), so a streamer attached to the turn means
// regular content outbounds are skipped; only the `final` outbound is
// consumed (as the turn-completion signal + fallback content source).
func (c *Channel) deliver(ctx context.Context, t *Turn, msg bus.OutboundMessage) {
	rawKind := strings.ToLower(strings.TrimSpace(msg.Context.Raw["message_kind"]))
	outKind := strings.ToLower(strings.TrimSpace(msg.Context.Raw["outbound_kind"]))

	t.mu.Lock()
	hasStreamer := t.streamer != nil
	t.mu.Unlock()

	switch {
	case outKind == "final":
		content := msg.Content
		if rawKind == "tool_feedback" || rawKind == "tool_calls" {
			content = ""
		}
		t.Finish(content, "")
	case rawKind == "tool_feedback":
		t.enqueueFrame(Frame{Type: "tool", Text: msg.Content})
	case rawKind == "tool_calls":
		t.enqueueFrame(Frame{Type: "tool", Text: msg.Content})
	default:
		if hasStreamer {
			// Streaming owns the text surface; regular outbounds would
			// duplicate the streamed tokens.
			return
		}
		if strings.TrimSpace(msg.Content) == "" {
			return
		}
		t.enqueueFrame(Frame{Type: "token", Text: msg.Content})
	}
}

// turnFor returns the active turn for a chat id (nil when none).
func (c *Channel) turnFor(chatID string) *Turn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.turns[chatID]
}

// BeginStream implements channels.StreamingCapable: the agent loop's
// configured-streaming path attaches a per-turn SSE streamer to the chat.
func (c *Channel) BeginStream(ctx context.Context, chatID string) (channels.Streamer, error) {
	c.mu.Lock()
	t := c.turns[chatID]
	c.mu.Unlock()
	if t == nil {
		return nil, fmt.Errorf("board: no active turn for chat %s", chatID)
	}
	st := newStreamer(ctx, t)
	t.setStreamer(st)
	return st, nil
}

// AttachTurn registers the active turn for one chat (replaces any stale
// turn on the same chat, finishing it with an error frame). The returned
// release func detaches it (only if it is still the attached turn).
func (c *Channel) AttachTurn(chatID string, t *Turn) (release func()) {
	c.mu.Lock()
	if prev, ok := c.turns[chatID]; ok && prev != t {
		logger.WarnCF("board", "replacing stale board turn", map[string]any{"chat_id": chatID})
		prev.Finish("", "replaced by a newer request")
	}
	c.turns[chatID] = t
	c.mu.Unlock()
	// Wake the pump so it observes the freshly attached turn (frames
	// enqueued before this point — e.g. tool feedback arriving between
	// relay lookups — are drained by the wake-up).
	t.SendKeepAlive()

	release = func() {
		c.mu.Lock()
		if cur, ok := c.turns[chatID]; ok && cur == t {
			delete(c.turns, chatID)
		}
		c.mu.Unlock()
	}
	return release
}

// BoardTurnForSession returns the active board chat turn for an agent-loop
// session key ([workflow-fork] P3-1). The session key for board chats is
// "board:<boardID>#<session>". Used by tools (e.g. the workflow write
// tools) to resolve the request-scoped identity of a board turn from the
// tool execution context.
func (c *Channel) BoardTurnForSession(sessionKey string) *Turn {
	if _, ok := ParseSessionKey(sessionKey); !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.turns {
		return t // board sessions are 1:1 with chats; single active turn
	}
	return nil
}

// JWTFor returns the JWT attached to the active turn for a chat (empty when
// none). Used by tool-side identity bridging fallbacks.
func (c *Channel) JWTFor(chatID string) string {
	t := c.turnFor(chatID)
	if t == nil {
		return ""
	}
	return t.JWT
}
