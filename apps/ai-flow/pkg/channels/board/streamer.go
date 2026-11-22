package board

import (
	"context"
	"sync"
)

// streamer implements the bus.Streamer interface for one board chat turn.
// The agent loop's configured-streaming path drives it:
//
//	Update(ctx, accumulated)   → token frame carrying only the new suffix
//	Finalize(ctx, content)     → marks the turn done; the HTTP pump emits
//	                             the result frame (content + captured calls)
//	Cancel(ctx)                → marks the turn done with an error
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
// re-emitted (the streamed tail was already delivered); the pump uses the
// terminal signal to emit the result frame with captured mutation calls.
func (s *streamer) Finalize(ctx context.Context, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled || s.finalized {
		return nil
	}
	s.finalized = true
	s.turn.Finish("", "")
	return nil
}

// Cancel aborts the turn: late updates/finalizes are ignored, and the pump
// terminates the stream with an error frame.
func (s *streamer) Cancel(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled {
		return
	}
	s.cancelled = true
	s.turn.Finish("", "assistant turn cancelled")
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
