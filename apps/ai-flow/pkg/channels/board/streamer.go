package board

import (
	"context"
	"strings"
	"sync"

	"github.com/computeflux/ai-flow/pkg/bus"
)

// streamer implements the bus.Streamer interface for one board chat turn.
// The agent loop's configured-streaming path drives it:
//
//	Update(ctx, accumulated)   → token frame carrying only the new suffix
//	Finalize(ctx, content)     → marks the turn done; the HTTP pump emits
//	                             the result frame (content + captured calls)
//	Cancel(ctx)                → drops this iteration's stream only; the turn
//	                             stays open (the pipeline continues, e.g. the
//	                             tool-call path cancels the LLM stream before
//	                             executing tools and finalizes at turn end)
//
// Update and Finalize are called from the LLM streaming goroutine; all
// mutation happens under mu. Frames flow through the turn queue into the
// handler's single-writer pump.
type streamer struct {
	turn *Turn

	mu        sync.Mutex
	sentLen   int // runes of accumulated content already emitted as tokens
	finalized bool
	cancelled bool
}

func newStreamer(ctx context.Context, t *Turn) *streamer {
	return &streamer{turn: t}
}

// Reset re-arms the streamer for a new LLM iteration after Cancel or a
// mid-turn Finalize (the pipeline cancels the configured stream on every
// tool-call iteration, and an iteration that streams prose alongside tool
// calls finalizes its streamer while the turn continues). The next
// iteration's Update() re-attaches via BeginStream, so Reset keeps the delta
// bookkeeping consistent without emitting a terminal signal.
func (s *streamer) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelled = false
	s.finalized = false
	s.sentLen = 0
}

// SetModelName implements the optional streamer metadata hook (ignored).
func (s *streamer) SetModelName(modelName string) {}

// SetTurnUsage implements the optional streamer metadata hook (ignored).
func (s *streamer) SetTurnUsage(inputTokens, outputTokens int) {}

// Update emits the delta between the accumulated stream text and what was
// already sent. Empty/no-op updates are skipped so tool-call iterations do
// not re-emit the same prefix.
func (s *streamer) Update(ctx context.Context, accumulated string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || s.finalized {
		return nil
	}
	runes := []rune(accumulated)
	if len(runes) <= s.sentLen {
		return nil
	}
	delta := string(runes[s.sentLen:])
	s.sentLen = len(runes)
	s.turn.enqueueFrame(Frame{Type: "token", Text: delta})
	return nil
}

// Finalize marks the streamed turn complete. The final content is not
// re-emitted when the streamer already delivered it (the streamed tail was
// already delivered); a cancelled iteration that later became the turn's
// final answer (e.g. a tool-call iteration that also produced prose) is
// re-armed here so the content is never lost. A mid-turn finalize (prose +
// tool calls in one response) marks finalized=true and the turn done; the
// next iteration's BeginStream re-arms this streamer, and the final
// iteration's Finalize emits the terminal signal with the complete text.
//
// An empty-content finalize (the pipeline publishes the streaming summary
// marker "[LLM summary: generated; delivery handled by the streaming
// publisher]" right after the last streamed delta, before the agent loop
// publishes the real final outbound) must NOT finish the turn with empty
// final content: the relay's final outbound is the authoritative carrier of
// the turn's answer, and Finish is first-write-wins. It only marks this
// iteration's stream done — but it must still wake the pump: if the turn
// already carries a *fillable* empty-final terminal signal (a streamer
// finalize that landed empty), the pump's grace window has to observe it so
// the relay's late final outbound can fill the result content.
func (s *streamer) Finalize(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finalized {
		return nil
	}
	if s.cancelled {
		if strings.TrimSpace(content) == "" {
			s.finalized = true
			s.turn.SendKeepAlive()
			return nil
		}
		s.cancelled = false
		sent := s.sentLen
		if runes := []rune(content); len(runes) > sent {
			s.turn.enqueueFrame(Frame{Type: "token", Text: string(runes[sent:])})
			s.sentLen = len(runes)
		}
	}
	s.finalized = true
	if strings.TrimSpace(content) == "" {
		s.turn.SendKeepAlive()
		return nil
	}
	s.turn.Finish(content, "")
	return nil
}

// FinalizeWithContext implements bus.ContextUsageStreamer: same semantics as
// Finalize (the board SSE protocol has no context-usage frames).
func (s *streamer) FinalizeWithContext(ctx context.Context, content string, usage *bus.ContextUsage) error {
	return s.Finalize(ctx, content)
}

// Cancel aborts this iteration's stream: late updates are ignored. It does
// NOT finish the turn — the pipeline uses Cancel on the tool-call path (and on
// mid-turn retries) and only Finalize terminates the SSE stream. If the turn
// is already finished (e.g. the watchdog), Cancel surfaces the error frame.
func (s *streamer) Cancel(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return
	}
	s.cancelled = true
	s.turn.CancelStream()
}

// Finalized reports whether Finalize ran.
func (s *streamer) Finalized() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalized
}

// Cancelled reports whether Cancel ran.
func (s *streamer) Cancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}
