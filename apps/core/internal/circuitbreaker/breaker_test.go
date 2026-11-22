package circuitbreaker

import (
	"context"
	"testing"
	"time"
)

func newTestBreaker(t *testing.T) (*Breaker, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var nowPtr *time.Time
	nowPtr = &now
	b := &Breaker{
		name:  "test-skill",
		state: StateClose,
		now:   func() time.Time { return *nowPtr },
	}
	return b, nowPtr
}

func TestCloseToOpenAfter5Failures(t *testing.T) {
	b, _ := newTestBreaker(t)
	for i := 0; i < 4; i++ {
		if opened := b.RecordFailure(context.Background()); opened {
			t.Fatalf("failure %d should not open breaker", i+1)
		}
		if !b.Allow(context.Background()) {
			t.Fatalf("breaker should stay close after %d failures", i+1)
		}
	}
	if !b.RecordFailure(context.Background()) {
		t.Fatal("5th consecutive failure should open breaker")
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open", b.State())
	}
	if b.Allow(context.Background()) {
		t.Fatal("open breaker should reject")
	}
}

func TestOpenToHalfAfter60s(t *testing.T) {
	b, now := newTestBreaker(t)
	b.RecordFailure(context.Background())
	b.RecordFailure(context.Background())
	b.RecordFailure(context.Background())
	b.RecordFailure(context.Background())
	b.RecordFailure(context.Background()) // open

	if b.Allow(context.Background()) {
		t.Fatal("open within 60s should reject")
	}
	*now = now.Add(61 * time.Second)
	if !b.Allow(context.Background()) {
		t.Fatal("open after 60s should half-open (allow probe)")
	}
	if b.State() != StateHalf {
		t.Fatalf("state = %s, want half", b.State())
	}
}

func TestHalfCloseAfter2Successes(t *testing.T) {
	b, now := newTestBreaker(t)
	for i := 0; i < 5; i++ {
		b.RecordFailure(context.Background())
	}
	*now = now.Add(61 * time.Second)
	b.Allow(context.Background()) // → half

	b.RecordSuccess(context.Background())
	if b.State() != StateHalf {
		t.Fatalf("after 1 success in half, state = %s, want half", b.State())
	}
	b.RecordSuccess(context.Background())
	if b.State() != StateClose {
		t.Fatalf("after 2 successes in half, state = %s, want close", b.State())
	}
	if b.consecutiveFailures != 0 {
		t.Fatalf("failures should reset on close, got %d", b.consecutiveFailures)
	}
}

func TestHalfFailureBackToOpen(t *testing.T) {
	b, now := newTestBreaker(t)
	for i := 0; i < 5; i++ {
		b.RecordFailure(context.Background())
	}
	*now = now.Add(61 * time.Second)
	b.Allow(context.Background()) // → half

	if !b.RecordFailure(context.Background()) {
		t.Fatal("probe failure in half should re-open")
	}
	if b.State() != StateOpen {
		t.Fatalf("state = %s, want open", b.State())
	}
	// openedAt 已重置：60s 内继续拒绝
	if b.Allow(context.Background()) {
		t.Fatal("re-opened breaker should reject within 60s")
	}
}

func TestSuccessResetsFailureCount(t *testing.T) {
	b, _ := newTestBreaker(t)
	for i := 0; i < 4; i++ {
		b.RecordFailure(context.Background())
	}
	b.RecordSuccess(context.Background()) // close 下成功清零
	for i := 0; i < 4; i++ {
		b.RecordFailure(context.Background())
	}
	if b.State() != StateClose {
		t.Fatal("4+4 with success in between should stay close")
	}
}

func TestForceOpenAndReset(t *testing.T) {
	b, _ := newTestBreaker(t)
	b.ForceOpen(context.Background())
	if b.State() != StateOpen {
		t.Fatalf("ForceOpen: state = %s", b.State())
	}
	b.Reset(context.Background())
	if b.State() != StateClose || b.consecutiveFailures != 0 {
		t.Fatalf("Reset: state=%s failures=%d", b.State(), b.consecutiveFailures)
	}
}

func TestRegistryGetCaches(t *testing.T) {
	r := NewRegistry(nil)
	b1 := r.Get("s1")
	b2 := r.Get("s1")
	if b1 != b2 {
		t.Fatal("registry should cache breaker per skill")
	}
	if r.Get("s2") == b1 {
		t.Fatal("different skill should get different breaker")
	}
}
