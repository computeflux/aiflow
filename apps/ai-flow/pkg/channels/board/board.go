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
	ChatID     string
	JWT        string
	sessionKey string // board session key ("board:<boardID>#<session>") for session-scoped lookups

	mu       sync.Mutex
	queue    []Frame
	done     bool
	final    string
	errMsg   string
	calls    []ToolCall
	callSeq  int
	streamer *streamer // set by BeginStream (configured-streaming path)
	termSrc  string    // terminal-signal source: "relay" (final outbound) or "streamer"

	notify chan struct{}
}

// NewTurn creates a turn for a chat id with the caller's JWT.
func NewTurn(chatID, jwt string) *Turn {
	return &Turn{ChatID: chatID, JWT: jwt, notify: make(chan struct{}, 1)}
}

// SetSessionKey records the board session key for session-scoped turn
// lookups (BeginStream / BoardTurnForSession).
func (t *Turn) SetSessionKey(key string) {
	t.mu.Lock()
	t.sessionKey = key
	t.mu.Unlock()
}

// SessionKey returns the board session key (empty before SetSessionKey).
func (t *Turn) SessionKey() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionKey
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

// enqueueFrame buffers a frame for the pump and wakes it. Frames that arrive
// after the turn is done are appended to the queue and re-signal the pump
// (the pump's terminal drain pass drains them before emitting the result
// frame; a keep-alive wake-up races with frames enqueued by the relay in the
// same window, so the pump must never emit the terminal frame while queued
// frames remain).
func (t *Turn) enqueueFrame(f Frame) {
	t.mu.Lock()
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
//
// First-write-wins for the terminal signal, with two exceptions for a turn
// already marked done with an *empty* final and no error:
//
//   - any later Finish may fill the final content (a streamer finalize can
//     land with empty content — the pipeline's summary-finalize — before the
//     agent loop publishes the real final outbound);
//   - a relay final (FinishFromRelay) may fully replace a streamer-origin
//     terminal signal: a mid-turn streamer finalize (prose + tool calls in
//     one iteration) must not steal the turn's real final answer from the
//     authoritative relay final outbound.
func (t *Turn) Finish(finalContent, errMsg string) {
	t.finish(finalContent, errMsg, "streamer")
}

// FinishFromRelay marks the turn done from the relay's final-outbound path
// (the authoritative turn-completion signal; see Finish's rules).
func (t *Turn) FinishFromRelay(finalContent, errMsg string) {
	t.finish(finalContent, errMsg, "relay")
}

func (t *Turn) finish(finalContent, errMsg string, src string) {
	t.mu.Lock()
	if t.done {
		emptyFinal := t.errMsg == "" && strings.TrimSpace(t.final) == ""
		fillable := errMsg == "" && strings.TrimSpace(finalContent) != "" && emptyFinal
		replaceable := src == "relay" && errMsg == "" && t.errMsg == "" && t.termSrc != "relay"
		if fillable || replaceable {
			t.final = finalContent
			t.errMsg = errMsg
			t.termSrc = src
			t.mu.Unlock()
			select {
			case t.notify <- struct{}{}:
			default:
			}
			return
		}
		t.mu.Unlock()
		return
	}
	t.done = true
	t.final = finalContent
	t.errMsg = errMsg
	t.termSrc = src
	t.streamer = nil
	t.mu.Unlock()
	select {
	case t.notify <- struct{}{}:
	default:
	}
}

// CancelStream detaches the iteration streamer without finishing the turn
// (the pipeline's tool-call path cancels the LLM stream before executing
// tools; the next iteration attaches a fresh streamer via BeginStream). If
// the turn is already done, it terminates with an error frame (legacy
// Cancel semantics).
func (t *Turn) CancelStream() {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		t.Finish("", "assistant turn cancelled")
		return
	}
	t.streamer = nil
	t.mu.Unlock()
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

// detachStreamer pops the active iteration streamer (nil when none).
func (t *Turn) detachStreamer() *streamer {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.streamer
	t.streamer = nil
	return s
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

	mu          sync.Mutex
	turns       map[string]*Turn // chatID → most-recently-attached turn (identity fallbacks)
	sessionTurn map[string]*Turn // board session key → active turn (concurrent turns share a chat)
	running     bool
	cancel      context.CancelFunc
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

// Start marks the channel running. Safe to call once per Start/Stop cycle.
func (c *Channel) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return nil
	}
	c.running = true
	_, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	// [workflow-fork] P3-1: no channel-level fan-out subscription. The relay
	// is purely reactive: every /chat request attaches its turn to the
	// session registry and runs a dedicated per-request relay (see
	// Handler.RelayTurn), which is the sole delivery path for turn-scoped
	// outbounds. A channel-wide relay here would double-deliver every message
	// (the shared channel subscription + the per-request subscription both
	// resolve the same turn from ValueCtx → duplicate tool/token frames).
	c.mu.Unlock()

	logger.InfoCF("board", "board chat channel started", nil)
	return nil
}

// Stop stops the channel and detaches any active turns.
func (c *Channel) Stop(ctx context.Context) error {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.running = false
	c.turns = make(map[string]*Turn)
	c.sessionTurn = make(map[string]*Turn)
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
	// The relay (fan-out subscription) is the primary delivery path and
	// resolves turns strictly from each message's ValueCtx. The synchronous
	// Send path (Manager.SendMessage / SendToChannel) is used by message-like
	// tools; deliver only when the caller's context explicitly carries a
	// board turn. It must never resolve a turn by chat id — the dispatcher's
	// Send ctx carries no turn, and resolving by chat id would re-deliver
	// (or wrongly finish) messages the relay already consumed.
	if t := TurnFromContext(ctx); t != nil {
		c.deliver(ctx, t, msg)
	}
	return []string{"board"}, nil
}

// SendMedia implements channels.MediaSender: synchronous media delivery to
// the active turn's sink (manager sendMediaWithRetry path).
func (c *Channel) SendMedia(ctx context.Context, msg bus.OutboundMediaMessage) ([]string, error) {
	// Same contract as Send: deliver only when the caller's context carries
	// a board turn; the relay handles fan-out delivery via ValueCtx.
	if t := TurnFromContext(ctx); t != nil {
		refs := make([]string, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			refs = append(refs, part.Ref)
		}
		t.enqueueFrame(mediaFrame(refs))
		return []string{"board"}, nil
	}
	return []string{"board"}, nil
}

// ---------------------------------------------------------------------------
// Outbound relay: per-request relay loops (see RelayTurn)
// ---------------------------------------------------------------------------

// RelayTurn [workflow-fork] P3-1: per-request relay loop. Consumes a
// dedicated bus outbound subscription and delivers ONLY the messages whose
// context carries the given turn (ValueCtx). Used by the /chat handler so
// each stream receives exactly its own turn's outbounds; media frames get
// the same treatment. Returns when the subscription closes.
func (c *Channel) RelayTurn(t *Turn, sub *bus.OutboundSubscription) {
	for {
		msg, ok := <-sub.Outbound
		if !ok {
			return
		}
		if msg.Context.Channel != ChannelName && msg.Channel != ChannelName {
			continue
		}
		if TurnFromContext(msg.Context.ValueCtx) == t {
			c.deliver(msg.Context.ValueCtx, t, msg)
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
	streamerDone := t.done
	t.mu.Unlock()

	switch {
	case outKind == "final":
		content := msg.Content
		auxiliary := rawKind == "tool_feedback" || rawKind == "tool_calls"
		if auxiliary {
			content = ""
		}
		// The configured-streaming path delivers the final text through the
		// streamer. A finalized streamer already finished the turn itself, so
		// the final outbound is a pure completion signal. A *cancelled*
		// streamer (the pipeline cancels the LLM stream on every tool-call
		// iteration) never finalized, so the final outbound is the only
		// remaining carrier of the turn's answer: keep its content and finish
		// the turn from it (the relay bypasses the manager's suppression for
		// internal channels, so this delivery is guaranteed to arrive).
		//
		// Auxiliary (tool-kind) finals never complete the turn: they must not
		// mark it done (that would terminate the SSE stream mid-turn, before
		// the real answer), and they never carry final content.
		if auxiliary {
			return
		}
		if hasStreamer && streamerDone && content != "" {
			content = ""
		}
		t.FinishFromRelay(content, "")
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
// The turn is resolved from the session key ("board:<boardID>#<session>")
// first — concurrent turns on the same chat each own their session, so a
// steering-continuation turn attaches to ITS OWN stream (the previous chatID
// lookup attached the continuation's tokens to whichever request last
// attached, misrouting the answer). Falls back to the chat slot when the
// session key is not a board session key.
func (c *Channel) BeginStream(ctx context.Context, chatID, sessionKey string) (channels.Streamer, error) {
	var t *Turn
	if _, ok := ParseSessionKey(sessionKey); ok {
		t = c.BoardTurnForSession(sessionKey)
	}
	if t == nil {
		c.mu.Lock()
		t = c.turns[chatID]
		c.mu.Unlock()
	}
	if t == nil {
		return nil, fmt.Errorf("board: no active turn for chat %s", chatID)
	}
	st := newStreamer(ctx, t)
	t.setStreamer(st)
	// Re-arm the previous iteration's streamer (the pipeline cancels the
	// configured stream on every tool-call iteration). A cancelled streamer
	// ignores late updates and its manager hooks no longer fire Finalize, so
	// without this re-arm the streamActive suppression marker set by an
	// earlier iteration's finalize is never cleared and the final outbound of
	// the turn is suppressed → the SSE stream never terminates.
	if prev := t.detachStreamer(); prev != nil && prev != st {
		prev.Cancel(ctx)
	}
	return st, nil
}

// AttachTurn registers the active turn for one chat. Concurrent turns on the
// same chat are allowed: each POST /chat request owns its own turn and
// subscribes to the bus relay for its own session key, so a second request
// must NEVER finish the first turn's stream (the previous single-slot design
// killed in-flight turns with "replaced by a newer request" whenever the
// agent loop ran a queued turn for the same session concurrently — e.g. a
// steering continuation — and the killed stream never delivered its answer).
// The returned release func detaches this turn from the chat slot (only if
// it is still the attached turn; the slot is best-effort and used by the
// tool-side identity fallbacks).
func (c *Channel) AttachTurn(chatID string, t *Turn) (release func()) {
	sessionKey := t.SessionKey()
	c.mu.Lock()
	c.turns[chatID] = t
	if c.sessionTurn == nil {
		c.sessionTurn = make(map[string]*Turn)
	}
	c.sessionTurn[sessionKey] = t
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
		if cur, ok := c.sessionTurn[sessionKey]; ok && cur == t {
			delete(c.sessionTurn, sessionKey)
		}
		c.mu.Unlock()
	}
	return release
}

// BoardTurnForSession returns the active board chat turn for an agent-loop
// session key ([workflow-fork] P3-1). The session key for board chats is
// "board:<boardID>#<session>". Used by tools (e.g. the workflow write
// tools) to resolve the request-scoped identity of a board turn from the
// tool execution context, and by BeginStream to attach a streamer to the
// correct turn when concurrent turns share a chat.
func (c *Channel) BoardTurnForSession(sessionKey string) *Turn {
	if _, ok := ParseSessionKey(sessionKey); !ok {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessionTurn[sessionKey]
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
