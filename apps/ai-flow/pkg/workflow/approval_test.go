// PicoClaw - Ultra-lightweight personal AI agent
// License: MIT
//
// [workflow-fork] P4-1 unit tests: confirm-token issuance / verification /
// expiry / tampering (params, user, tool, forged sig) / single-use replay,
// preview flow, canonical params hashing, audit double-write (incl. the
// high-risk rejection on audit failure) and the HTTP audit sink against a
// stub workflow-api.

package workflow

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testSecret = "test-approval-secret"

// fakeClock is a manually advanced clock for expiry tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// memSink records audit records in memory; fail makes WriteAudit error.
type memSink struct {
	mu      sync.Mutex
	records []AuditRecord
	fail    bool
}

func (s *memSink) WriteAudit(_ context.Context, rec AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("audit sink: simulated write failure")
	}
	s.records = append(s.records, rec)
	return nil
}

func (s *memSink) setFail(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = v
}

func (s *memSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

func newTestApproval(t *testing.T) (*Approval, *fakeClock, *memSink) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
	sink := &memSink{}
	a := NewApproval(testSecret, DefaultApprovalTTL, sink)
	a.SetClock(clk.Now)
	return a, clk, sink
}

func TestParamsHashCanonical(t *testing.T) {
	// Map key order must not affect the hash (canonical JSON: sorted keys).
	h1, err := ParamsHash(map[string]any{"b": 2, "a": 1})
	if err != nil {
		t.Fatalf("ParamsHash: %v", err)
	}
	h2, _ := ParamsHash(map[string]any{"a": 1, "b": 2})
	if h1 != h2 {
		t.Fatalf("canonical hash unstable: %s != %s", h1, h2)
	}
	// Value changes must change the hash.
	h3, _ := ParamsHash(map[string]any{"a": 1, "b": 3})
	if h1 == h3 {
		t.Fatal("hash collision on changed value")
	}
	// Hash is sha256 hex of canonical JSON.
	sum := sha256.Sum256([]byte(`{"a":1,"b":2}`))
	if want := hex.EncodeToString(sum[:]); h1 != want {
		t.Fatalf("hash = %s, want sha256(canonical) %s", h1, want)
	}
	// nil args → hash of "null".
	if _, err := ParamsHash(nil); err != nil {
		t.Fatalf("ParamsHash(nil): %v", err)
	}
}

func TestPreviewIssuesToken(t *testing.T) {
	a, clk, _ := newTestApproval(t)
	pv, err := a.Preview("user-1", "publish_card", "发布卡片 #42 到 X", map[string]any{"card_id": 42})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if pv.Token == "" || pv.ParamsHash == "" || pv.Tool != "publish_card" {
		t.Fatalf("preview incomplete: %+v", pv)
	}
	// params_hash = SHA256(canonical JSON args).
	sum := sha256.Sum256([]byte(`{"card_id":42}`))
	if want := hex.EncodeToString(sum[:]); pv.ParamsHash != want {
		t.Fatalf("params_hash = %s, want %s", pv.ParamsHash, want)
	}
	// 5-minute validity.
	if got := pv.ExpiresAt.Sub(clk.Now()); got != DefaultApprovalTTL {
		t.Fatalf("ttl = %v, want %v", got, DefaultApprovalTTL)
	}
	// Token signature = HMAC-SHA256(secret, user|tool|params_hash|expire).
	expireStr := strings.SplitN(pv.Token, "|", 2)[0]
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(fmt.Sprintf("user-1|publish_card|%s|%s", pv.ParamsHash, expireStr)))
	if want := hex.EncodeToString(mac.Sum(nil)); !strings.HasSuffix(pv.Token, want) {
		t.Fatalf("token sig mismatch: %s", pv.Token)
	}
}

func TestPreviewNoSecretFailsClosed(t *testing.T) {
	a := NewApproval("", DefaultApprovalTTL, nil)
	if _, err := a.Preview("u", "tool", "act", nil); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("Preview with empty secret: %v, want ErrNoSecret", err)
	}
	if _, err := a.Confirm(context.Background(), "u", "tool", nil, "x", true, ""); !errors.Is(err, ErrNoSecret) {
		t.Fatalf("Confirm with empty secret: %v, want ErrNoSecret", err)
	}
}

func TestConfirmHappyPath(t *testing.T) {
	a, _, sink := newTestApproval(t)
	args := map[string]any{"card_id": 42, "text": "hello"}
	pv, err := a.Preview("user-1", "publish_card", "发布卡片", args)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	dec, err := a.Confirm(context.Background(), "user-1", "publish_card", args, pv.Token, true, "confirmed_executed")
	if err != nil || !dec.Allowed {
		t.Fatalf("Confirm = %+v, %v; want allowed", dec, err)
	}
	if dec.ParamsHash != pv.ParamsHash {
		t.Fatalf("decision hash %s != preview hash %s", dec.ParamsHash, pv.ParamsHash)
	}
	// Audit double-write: actor=ai-bot, on_behalf_of=user, tool, hash, result.
	if sink.count() != 1 {
		t.Fatalf("audit records = %d, want 1", sink.count())
	}
	rec := sink.records[0]
	if rec.Actor != "ai-bot" || rec.OnBehalfOf != "user-1" || rec.Tool != "publish_card" ||
		rec.ParamsHash != pv.ParamsHash || rec.Result != "confirmed_executed" {
		t.Fatalf("audit record mismatch: %+v", rec)
	}
}

func TestConfirmNoTokenRejected(t *testing.T) {
	a, _, sink := newTestApproval(t)
	for _, tok := range []string{"", "   "} {
		dec, err := a.Confirm(context.Background(), "user-1", "publish_card", map[string]any{"card_id": 1}, tok, true, "")
		if !errors.Is(err, ErrNoToken) || dec.Allowed {
			t.Fatalf("empty token: dec=%+v err=%v, want denied ErrNoToken", dec, err)
		}
	}
	if sink.count() != 0 {
		t.Fatal("rejected confirm must not audit-execute")
	}
}

func TestConfirmExpired(t *testing.T) {
	a, clk, _ := newTestApproval(t)
	args := map[string]any{"card_id": 42}
	pv, _ := a.Preview("user-1", "publish_card", "act", args)

	// Just before expiry: allowed.
	clk.Advance(DefaultApprovalTTL - time.Second)
	// (Use a fresh token for the success leg since the next case replays.)
	pv2, _ := a.Preview("user-1", "publish_card", "act", args)
	dec, err := a.Confirm(context.Background(), "user-1", "publish_card", args, pv2.Token, false, "")
	if err != nil || !dec.Allowed {
		t.Fatalf("confirm before expiry: %+v %v; want allowed", dec, err)
	}

	// Past expiry: rejected (signature still valid — expiry gate is separate).
	clk.Advance(DefaultApprovalTTL + time.Second)
	dec, err = a.Confirm(context.Background(), "user-1", "publish_card", args, pv.Token, false, "")
	if !errors.Is(err, ErrTokenExpired) || dec.Allowed {
		t.Fatalf("expired token: %+v %v; want denied ErrTokenExpired", dec, err)
	}
}

func TestConfirmTampered(t *testing.T) {
	a, _, _ := newTestApproval(t)
	args := map[string]any{"card_id": 42}
	pv, _ := a.Preview("user-1", "publish_card", "act", args)
	ctx := context.Background()

	// 1. Params tampered (different value).
	dec, err := a.Confirm(ctx, "user-1", "publish_card", map[string]any{"card_id": 43}, pv.Token, true, "")
	if !errors.Is(err, ErrTokenInvalid) || dec.Allowed {
		t.Fatalf("tampered params: %+v %v; want denied", dec, err)
	}
	// 2. Params tampered (extra key).
	dec, err = a.Confirm(ctx, "user-1", "publish_card", map[string]any{"card_id": 42, "extra": 1}, pv.Token, true, "")
	if !errors.Is(err, ErrTokenInvalid) || dec.Allowed {
		t.Fatalf("tampered params (extra key): %+v %v; want denied", dec, err)
	}
	// 3. Different user_id.
	dec, err = a.Confirm(ctx, "attacker", "publish_card", args, pv.Token, true, "")
	if !errors.Is(err, ErrTokenInvalid) || dec.Allowed {
		t.Fatalf("tampered user: %+v %v; want denied", dec, err)
	}
	// 4. Different tool (token laundering across tools).
	dec, err = a.Confirm(ctx, "user-1", "delete_board", args, pv.Token, true, "")
	if !errors.Is(err, ErrTokenInvalid) || dec.Allowed {
		t.Fatalf("tampered tool: %+v %v; want denied", dec, err)
	}
	// 5. Forged signature (flip a hex nibble in the sig part).
	sigStart := strings.LastIndexByte(pv.Token, '|')
	forged := pv.Token[:sigStart+1] + flipNibble(pv.Token[sigStart+1:])
	dec, err = a.Confirm(ctx, "user-1", "publish_card", args, forged, true, "")
	if !errors.Is(err, ErrTokenInvalid) || dec.Allowed {
		t.Fatalf("forged sig: %+v %v; want denied", dec, err)
	}
	// 6. Token signed with a foreign secret.
	other := NewApproval("other-secret", DefaultApprovalTTL, nil)
	pvOther, _ := other.Preview("user-1", "publish_card", "act", args)
	dec, err = a.Confirm(ctx, "user-1", "publish_card", args, pvOther.Token, true, "")
	if !errors.Is(err, ErrTokenInvalid) || dec.Allowed {
		t.Fatalf("foreign-secret token: %+v %v; want denied", dec, err)
	}
	// 7. Malformed tokens.
	for _, bad := range []string{"garbage", "123", "|", "abc|def", "9999999999|" + strings.Repeat("00", sha256.Size)} {
		dec, err = a.Confirm(ctx, "user-1", "publish_card", args, bad, true, "")
		if !errors.Is(err, ErrTokenInvalid) || dec.Allowed {
			t.Fatalf("malformed %q: %+v %v; want denied", bad, dec, err)
		}
	}
}

func TestConfirmSingleUseReplay(t *testing.T) {
	a, _, sink := newTestApproval(t)
	args := map[string]any{"card_id": 42}
	pv, _ := a.Preview("user-1", "publish_card", "act", args)
	ctx := context.Background()

	dec, err := a.Confirm(ctx, "user-1", "publish_card", args, pv.Token, true, "confirmed_executed")
	if err != nil || !dec.Allowed {
		t.Fatalf("first confirm: %+v %v; want allowed", dec, err)
	}
	// Replay with the same token → rejected, no extra audit.
	for i := 0; i < 3; i++ {
		dec, err = a.Confirm(ctx, "user-1", "publish_card", args, pv.Token, true, "confirmed_executed")
		if !errors.Is(err, ErrTokenReplayed) || dec.Allowed {
			t.Fatalf("replay %d: %+v %v; want denied ErrTokenReplayed", i, dec, err)
		}
	}
	if sink.count() != 1 {
		t.Fatalf("audit records = %d, want 1 (replays must not execute)", sink.count())
	}
}

func TestAuditFailureRejectsHighRisk(t *testing.T) {
	a, _, sink := newTestApproval(t)
	args := map[string]any{"card_id": 42}
	pv, _ := a.Preview("user-1", "publish_card", "act", args)
	sink.setFail(true)

	// High-risk: audit write failure → refuse execution (留痕红线).
	dec, err := a.Confirm(context.Background(), "user-1", "publish_card", args, pv.Token, true, "")
	if !errors.Is(err, ErrAuditWrite) || dec.Allowed {
		t.Fatalf("high-risk audit failure: %+v %v; want denied ErrAuditWrite", dec, err)
	}

	// Non-high-risk: audit best-effort, execution proceeds.
	pv2, _ := a.Preview("user-1", "read_tool", "act", args)
	dec, err = a.Confirm(context.Background(), "user-1", "read_tool", args, pv2.Token, false, "")
	if err != nil || !dec.Allowed {
		t.Fatalf("low-risk audit failure: %+v %v; want allowed", dec, err)
	}
}

func TestNilSinkRejectsHighRisk(t *testing.T) {
	// sink == nil：高危操作 fail-closed 拒绝（无审计通道绝不允许静默放行）；
	// 非高危尽力无审计放行（sink nil = 部署选择，仅对低危生效）。
	a := NewApproval(testSecret, DefaultApprovalTTL, nil)
	args := map[string]any{"card_id": 42}
	pv, _ := a.Preview("user-1", "publish_card", "act", args)
	dec, err := a.Confirm(context.Background(), "user-1", "publish_card", args, pv.Token, true, "")
	if !errors.Is(err, ErrAuditWrite) || dec.Allowed {
		t.Fatalf("nil sink high-risk: %+v %v; want denied ErrAuditWrite", dec, err)
	}
	pv2, _ := a.Preview("user-1", "read_tool", "act", args)
	dec, err = a.Confirm(context.Background(), "user-1", "read_tool", args, pv2.Token, false, "")
	if err != nil || !dec.Allowed {
		t.Fatalf("nil sink low-risk: %+v %v; want allowed", dec, err)
	}
}

func TestReplayCanonicalizedSpellings(t *testing.T) {
	// Single-use must be keyed on the canonical (expire, sig) pair: after
	// consuming "<expire>|<sig>", equivalent spellings — expire with
	// leading zeros, upper-case hex sig — parse to the same (expire, sig)
	// and must be rejected as replays, never re-allowed.
	a, clk, sink := newTestApproval(t)
	args := map[string]any{"card_id": 42}
	pv, _ := a.Preview("user-1", "publish_card", "act", args)
	ctx := context.Background()

	dec, err := a.Confirm(ctx, "user-1", "publish_card", args, pv.Token, false, "r")
	if err != nil || !dec.Allowed {
		t.Fatalf("first confirm: %+v %v; want allowed", dec, err)
	}

	sep := strings.LastIndexByte(pv.Token, '|')
	expireStr := pv.Token[:sep]
	sigHex := pv.Token[sep+1:]

	variants := []string{
		"0" + expireStr + "|" + sigHex,                   // leading zero on expire
		"00" + expireStr + "|" + strings.ToUpper(sigHex), // leading zeros + upper-case sig
	}
	for _, v := range variants {
		dec, err = a.Confirm(ctx, "user-1", "publish_card", args, v, false, "r")
		if !errors.Is(err, ErrTokenReplayed) || dec.Allowed {
			t.Fatalf("canonicalized replay %q: %+v %v; want denied ErrTokenReplayed", v, dec, err)
		}
	}
	clk.Advance(0) // no-op keep clk referenced
	if sink.count() != 1 {
		t.Fatalf("audit records = %d, want 1 (canonicalized replays must not execute)", sink.count())
	}
}

func TestConsumedMapPruning(t *testing.T) {
	a, clk, _ := newTestApproval(t)
	// Issue more than pruneEvery tokens; consumed map stays bounded.
	for i := 0; i < 200; i++ {
		clk.Advance(time.Second)
		if _, err := a.Preview("u", "t", "a", map[string]any{"i": i}); err != nil {
			t.Fatalf("Preview %d: %v", i, err)
		}
	}
	a.mu.Lock()
	n := len(a.consumed)
	a.mu.Unlock()
	if n > 128 {
		t.Fatalf("consumed map unbounded: %d entries", n)
	}
}

func TestHTTPAuditSink(t *testing.T) {
	type captured struct {
		rec  AuditRecord
		auth string
		path string
	}
	var mu sync.Mutex
	var got captured
	var hits int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		mu.Lock()
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got.rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	sink := NewHTTPAuditSink(srv.URL)
	rec := AuditRecord{
		Actor: "ai-bot", OnBehalfOf: "user-9", Tool: "publish_card",
		ParamsHash: strings.Repeat("ab", sha256.Size), Result: "confirmed_executed",
		Action: "picoclaw_write_confirm",
	}
	ctx := ContextWithJWT(context.Background(), "jwt-token-1")
	if err := sink.WriteAudit(ctx, rec); err != nil {
		t.Fatalf("WriteAudit: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got.path != "/audit-logs" {
		t.Fatalf("path = %s, want /audit-logs", got.path)
	}
	if got.auth != "Bearer jwt-token-1" {
		t.Fatalf("auth = %q, want Bearer jwt-token-1", got.auth)
	}
	if got.rec.Actor != "ai-bot" || got.rec.OnBehalfOf != "user-9" || got.rec.Tool != "publish_card" {
		t.Fatalf("record mismatch: %+v", got.rec)
	}
}

func TestHTTPAuditSinkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	sink := NewHTTPAuditSink(srv.URL)
	err := sink.WriteAudit(context.Background(), AuditRecord{Actor: "ai-bot", Tool: "t"})
	if err == nil {
		t.Fatal("expected error on 5xx")
	}
	if st, ok := HTTPStatusOf(err); !ok || st != http.StatusInternalServerError {
		t.Fatalf("status = %d %v", st, ok)
	}
}

func TestConfirmConcurrentSingleUse(t *testing.T) {
	a, _, sink := newTestApproval(t)
	args := map[string]any{"card_id": 1}
	pv, _ := a.Preview("user-1", "tool", "act", args)

	var wg sync.WaitGroup
	var allowed int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dec, err := a.Confirm(context.Background(), "user-1", "tool", args, pv.Token, true, "r")
			if err == nil && dec.Allowed {
				atomic.AddInt32(&allowed, 1)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&allowed); got != 1 {
		t.Fatalf("concurrent confirms allowed = %d, want exactly 1", got)
	}
	if sink.count() != 1 {
		t.Fatalf("audit records = %d, want 1", sink.count())
	}
}

// flipNibble flips the first hex nibble of s (deterministic tamper).
func flipNibble(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] == '0' {
		b[0] = '1'
	} else {
		b[0] = '0'
	}
	return string(b)
}
