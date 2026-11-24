package channels_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/computeflux/ai-flow/pkg/bus"
	"github.com/computeflux/ai-flow/pkg/channels"
	"github.com/computeflux/ai-flow/pkg/channels/board"
	"github.com/computeflux/ai-flow/pkg/config"
)

// TestBoardRelayStreamingRoundTrip exercises the manager streaming plumbing
// for the board relay channel across tool-call turns, mirroring the agent
// pipeline's real sequences:
//
//   - direct-answer turn: iteration streams tokens, the tool-call path
//     cancels the stream, tools run (tool-feedback outbounds), the final
//     iteration streams tokens and finalizes, and the agent loop publishes
//     the legacy final outbound.
//   - mid-turn-finalize turn (the "prose + tool calls" case): an iteration
//     streams prose and finalizes its streamer while the turn continues with
//     tool calls; a later iteration streams the final answer. The result
//     frame must carry the final answer exactly once (no duplication, no
//     empty content).
//
// Every bus message addressed to "board" carries the turn on its ValueCtx
// (exactly like the agent loop's inbound-context stamp). Regression: the
// manager's generic suppression plumbing marked the stream active on every
// streamer finalize, so the final iteration's streamer was never re-armed
// after a tool-call iteration cancelled it; the final outbound was suppressed
// and the SSE stream never emitted a result frame (the board UI hung on
// "AI 正在思考" until the 10-minute watchdog). The pump additionally dropped
// frames queued concurrently with the terminal signal.
func TestBoardRelayStreamingRoundTrip(t *testing.T) {
	tests := []struct {
		name            string
		midFinalize     bool // an iteration finalizes its streamer mid-turn (prose + tool calls)
		finalNoStreamer bool // the final iteration's streamer was cancelled (tool-call response): the relay final outbound is the only content carrier
		emptyFinalize   bool // the final iteration finalizes its streamer with empty content (summary-finalize): the relay final outbound must fill the result
		cancelThenEmpty bool // iteration 1 cancels; final iteration finalizes with empty content
		wantStreamed    string
		wantFinalText   string // complete final answer (result content, streamed tail, or both)
		wantToolCount   int
	}{
		{
			name:          "direct_answer",
			wantStreamed:  "先查一下共 3 个技能",
			wantFinalText: "共 3 个技能",
			wantToolCount: 1,
		},
		{
			name:          "mid_turn_finalize",
			midFinalize:   true,
			wantStreamed:  "先看一眼共 3 个技能",
			wantFinalText: "共 3 个技能",
			wantToolCount: 1,
		},
		{
			// Regression: the final LLM iteration answered with tool calls only
			// (no prose), so its streamer was cancelled and never finalized. The
			// turn's answer arrives exclusively through the relay's final
			// outbound; deliver must NOT blank its content when the attached
			// streamer never finalized (the old rule blanked it whenever a
			// streamer was merely attached → empty result frame).
			name:            "relay_carries_final",
			finalNoStreamer: true,
			wantStreamed:    "先查一下",
			wantFinalText:   "共 3 个技能",
			wantToolCount:   1,
		},
		{
			// Regression (live): the final iteration streams its answer, then
			// the pipeline's summary-finalize lands with empty content. That
			// must not finish the turn with an empty final; the relay's final
			// outbound carries the answer into the result frame.
			name:          "empty_finalize_then_relay",
			emptyFinalize: true,
			wantStreamed:  "先查一下共 3 个技能",
			wantFinalText: "共 3 个技能",
			wantToolCount: 1,
		},
		{
			// Regression (live, tool-turn): iteration 1 cancels its stream for
			// tools; the final iteration streams prose and its summary-finalize
			// lands empty. The relay's final outbound must still fill the
			// result content.
			name:            "cancel_then_empty_finalize",
			cancelThenEmpty: true,
			wantStreamed:    "先查一下共 3 个技能",
			wantFinalText:   "共 3 个技能",
			wantToolCount:   1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := bus.NewMessageBus()
			defer b.Close()

			cfg := &config.Config{}
			m, err := channels.NewManager(cfg, b, nil)
			if err != nil {
				t.Fatalf("new manager: %v", err)
			}

			ch := board.New(b)
			h := board.NewHandler(ch, b, board.Options{InboundTimeout: 2 * time.Second, KeepAliveInterval: time.Hour})
			m.RegisterChannel(board.ChannelName, ch)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := m.StartAll(ctx); err != nil {
				t.Fatalf("start all: %v", err)
			}
			defer m.StopAll(context.Background())

			srv := httptest.NewServer(h)
			defer srv.Close()

			body := `{"board_id":1,"session":"toolturn","user_id":"42","content":"先查技能再总结"}`
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/chat", strings.NewReader(body))
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer jwt-token")

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("do request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			framesCh := make(chan []board.Frame, 1)
			go func() {
				var frames []board.Frame
				sc := bufio.NewScanner(resp.Body)
				sc.Buffer(make([]byte, 1024*1024), 1024*1024)
				for sc.Scan() {
					line := sc.Text()
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var f board.Frame
					if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
						continue
					}
					frames = append(frames, f)
					if f.Type == "result" || f.Type == "error" {
						break
					}
				}
				framesCh <- frames
			}()

			// Consume the inbound and replay the agent loop's turn-scoped publishes.
			go func() {
				msg := <-b.InboundChan()
				turnCtx := msg.ValueCtx
				chatID := msg.ChatID
				// The handler attaches the turn (session registry + keep-alive wake)
				// after publishing the inbound; the real agent loop spends seconds in
				// routing/hooks before publishing. Wait for the attach so relay
				// deliveries never race it (mirrors production ordering).
				deadline := time.Now().Add(2 * time.Second)
				for ch.BoardTurnForSession(msg.SessionKey) == nil && time.Now().Before(deadline) {
					time.Sleep(2 * time.Millisecond)
				}
				if ch.BoardTurnForSession(msg.SessionKey) == nil {
					t.Errorf("turn never attached for session %s", msg.SessionKey)
					return
				}

				pub := func(kind, content string) {
					out := bus.OutboundMessage{
						Channel: board.ChannelName,
						ChatID:  chatID,
						Context: bus.NewOutboundContext(board.ChannelName, chatID, ""),
						Content: content,
					}
					if kind != "" {
						out.Context.Raw = map[string]string{"message_kind": kind}
					}
					out.Context.ValueCtx = turnCtx
					if err := b.PublishOutbound(context.Background(), out); err != nil {
						t.Errorf("publish %s: %v", kind, err)
					}
				}

				// Iteration 1: stream tokens, then the tool-call path cancels.
				st, ok := b.GetStreamer(context.Background(), board.ChannelName, chatID, msg.SessionKey)
				if !ok {
					t.Errorf("iteration 1: no streamer")
					return
				}
				first := "先查一下"
				if tc.midFinalize {
					first = "先看一眼"
					// The iteration also finalized its streamer (prose + tool
					// calls in the same response).
					_ = st.Update(context.Background(), first)
					if err := st.Finalize(context.Background(), first); err != nil {
						t.Errorf("mid finalize: %v", err)
						return
					}
				} else {
					_ = st.Update(context.Background(), first)
					st.Cancel(context.Background())
				}

				// Tool feedback (tool-call iteration).
				pub("tool_feedback", "调用工具 workflow_board")

				// Final iteration: stream tokens and finalize (the relay_carries_final
				// case ends the iteration with tool calls instead → cancel, no
				// finalize, so the relay's final outbound must carry the answer).
				st2, ok := b.GetStreamer(context.Background(), board.ChannelName, chatID, msg.SessionKey)
				if !ok {
					t.Errorf("final iteration: no streamer")
					return
				}
				if tc.finalNoStreamer {
					st2.Cancel(context.Background())
				} else if tc.emptyFinalize || tc.cancelThenEmpty {
					_ = st2.Update(context.Background(), "共 3 个技能")
					if err := st2.Finalize(context.Background(), ""); err != nil {
						t.Errorf("empty finalize: %v", err)
						return
					}
				} else {
					_ = st2.Update(context.Background(), "共 3 个技能")
					if err := st2.Finalize(context.Background(), "共 3 个技能"); err != nil {
						t.Errorf("finalize: %v", err)
						return
					}
				}

				// Legacy final outbound (agent_outbound PublishResponseIfNeeded).
				final := bus.OutboundMessage{
					Channel: board.ChannelName,
					ChatID:  chatID,
					Context: bus.NewOutboundContext(board.ChannelName, chatID, ""),
					Content: "共 3 个技能",
				}
				final.Context.Raw = map[string]string{"outbound_kind": "final"}
				// The agent loop stamps the turn's publisher context onto the
				// final outbound (publishResponseIfNeeded sets ValueCtx from the
				// active turn state); the relay resolves the target turn from it.
				final.Context.ValueCtx = turnCtx
				if err := b.PublishOutbound(context.Background(), final); err != nil {
					t.Errorf("publish final: %v", err)
				}
			}()

			select {
			case frames := <-framesCh:
				var streamed strings.Builder
				var result *board.Frame
				var toolFrames int
				for i := range frames {
					switch frames[i].Type {
					case "token":
						streamed.WriteString(frames[i].Text)
					case "tool":
						toolFrames++
					case "result":
						result = &frames[i]
					case "error":
						t.Fatalf("error frame received: %+v", frames[i])
					}
				}
				if result == nil {
					t.Fatalf("no result frame delivered: %+v", frames)
				}
				if got := streamed.String(); got != tc.wantStreamed {
					t.Fatalf("streamed tokens = %q, want %q", got, tc.wantStreamed)
				}
				// The final answer must reach the client exactly once (result content,
				// the streamed tail, or both — never a truncated/empty answer).
				rc := result.Content
				runes := []rune(rc)
				wantRunes := []rune(tc.wantFinalText)
				suffixOK := len(runes) >= len(wantRunes) &&
					string(runes[len(runes)-len(wantRunes):]) == tc.wantFinalText
				if !suffixOK {
					t.Fatalf("result content = %q, want final text %q (result must carry the complete answer)", rc, tc.wantFinalText)
				}
				if toolFrames != tc.wantToolCount {
					t.Fatalf("tool frames = %d, want %d", toolFrames, tc.wantToolCount)
				}
			case <-time.After(8 * time.Second):
				t.Fatalf("timed out waiting for result frame (stream never terminated)")
			}
		})
	}
}
