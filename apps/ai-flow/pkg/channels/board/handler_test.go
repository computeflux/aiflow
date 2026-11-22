package board

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/computeflux/ai-flow/pkg/bus"
)

// TestHandlerChatRoundTrip exercises the full HTTP flow: POST /chat reads
// the request, attaches the turn, publishes the inbound, streams frames fed
// by the channel relay (which resolves the turn from each message's
// ValueCtx), and closes with a `result` frame carrying captured mutation
// calls.
func TestHandlerChatRoundTrip(t *testing.T) {
	b := bus.NewMessageBus()
	defer b.Close()

	ch := New(b)
	h := NewHandler(ch, b, Options{InboundTimeout: 2 * time.Second, KeepAliveInterval: time.Hour})
	if err := ch.Start(context.Background()); err != nil {
		t.Fatalf("start channel: %v", err)
	}
	defer ch.Stop(context.Background())

	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"board_id":7,"session":"s1","user_id":"42","content":"建一张卡"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
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

	// Consume inbound, echo a token + tool feedback, then finish the turn.
	done := make(chan struct{})
	go func() {
		defer close(done)
		msg := <-b.InboundChan()
		if msg.Channel != ChannelName || msg.ChatID != "u42" {
			t.Errorf("inbound = %s/%s, want board/u42", msg.Channel, msg.ChatID)
			return
		}
		tok := TurnFromContext(msg.ValueCtx)
		if tok == nil {
			t.Errorf("inbound ValueCtx has no board turn")
			return
		}
		tok.AddCall("create_card", map[string]any{"list_id": float64(3), "title": "x"})
		msgOut := bus.OutboundMessage{
			Channel: ChannelName,
			ChatID:  "u42",
			Context: bus.NewOutboundContext(ChannelName, "u42", ""),
			Content: "hello",
		}
		msgOut.Context.ValueCtx = ContextWithTurn(context.Background(), tok)
		if err := b.PublishOutbound(context.Background(), msgOut); err != nil {
			t.Errorf("publish token: %v", err)
			return
		}
		final := bus.OutboundMessage{
			Channel: ChannelName,
			ChatID:  "u42",
			Context: bus.NewOutboundContext(ChannelName, "u42", ""),
			Content: "hello",
		}
		final.Context.ValueCtx = ContextWithTurn(context.Background(), tok)
		if final.Context.Raw == nil {
			final.Context.Raw = map[string]string{}
		}
		final.Context.Raw["outbound_kind"] = "final"
		if err := b.PublishOutbound(context.Background(), final); err != nil {
			t.Errorf("publish final: %v", err)
		}
	}()

	frames := readFrames(t, resp.Body, 5*time.Second)
	<-done

	var gotToken bool
	var result *Frame
	for i := range frames {
		switch frames[i].Type {
		case "token":
			if frames[i].Text == "hello" {
				gotToken = true
			}
		case "result":
			result = &frames[i]
		}
	}
	if !gotToken {
		t.Fatalf("no token frame delivered: %+v", frames)
	}
	if result == nil {
		t.Fatalf("no result frame delivered: %+v", frames)
	}
	if len(result.Calls) != 1 || result.Calls[0].Function.Name != "create_card" {
		t.Fatalf("result calls = %+v", result.Calls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(result.Calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("call args: %v", err)
	}
	if args["title"] != "x" {
		t.Fatalf("call args = %+v", args)
	}
}

func readFrames(t *testing.T, body io.ReadCloser, timeout time.Duration) []Frame {
	t.Helper()

	type out struct {
		frames []Frame
	}
	ch := make(chan out, 1)
	go func() {
		var frames []Frame
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var f Frame
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f); err != nil {
				continue
			}
			frames = append(frames, f)
			if f.Type == "result" || f.Type == "error" {
				break
			}
		}
		ch <- out{frames: frames}
	}()

	select {
	case res := <-ch:
		return res.frames
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for frames")
		return nil
	}
}
