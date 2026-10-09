package kwin

import (
	"context"
	"testing"
	"time"
)

// newTestWM is a WM with only the reply plumbing wired: awaitReply and Result
// touch nothing else, so these cases need no session bus and no KWin.
func newTestWM() *WM { return &WM{pending: make(chan kwinReply, 8)} }

// A late reply from a call that already timed out must not be read as the
// current call's answer. For Active() that was a wrong origin, and a wrong
// origin doubles the keystroke on the window being played.
func TestAwaitReplyIgnoresAnEarlierCallsReply(t *testing.T) {
	w := newTestWM()
	cb := &callback{w}
	if err := cb.Result("1", "stale-from-call-1"); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if err := cb.Result("2", "answer-for-call-2"); err != nil {
		t.Fatalf("Result: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := w.awaitReply(ctx, 2)
	if err != nil {
		t.Fatalf("awaitReply: %v", err)
	}
	if got != "answer-for-call-2" {
		t.Fatalf("got %q, want the reply tagged 2", got)
	}
}

// With only a stale reply available, the call must time out rather than
// return the wrong answer: a missing origin fails closed (4.15), a wrong one
// does not.
func TestAwaitReplyTimesOutRatherThanTakeTheWrongReply(t *testing.T) {
	w := newTestWM()
	cb := &callback{w}
	_ = cb.Result("7", "stale")

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	got, err := w.awaitReply(ctx, 8)
	if err == nil {
		t.Fatalf("awaitReply returned %q, want a timeout", got)
	}
}

// A reply that arrives while the call is already waiting is still matched.
func TestAwaitReplyAcceptsAReplyThatArrivesLate(t *testing.T) {
	w := newTestWM()
	cb := &callback{w}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = cb.Result("3", "early-and-wrong")
		_ = cb.Result("4", "right")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := w.awaitReply(ctx, 4)
	if err != nil {
		t.Fatalf("awaitReply: %v", err)
	}
	if got != "right" {
		t.Fatalf("got %q, want %q", got, "right")
	}
}

// A sequence number that is not a number cannot be attributed, so it is
// dropped rather than treated as belonging to the current call.
func TestResultDropsAnUnattributableReply(t *testing.T) {
	w := newTestWM()
	cb := &callback{w}
	if err := cb.Result("not-a-number", "payload"); err != nil {
		t.Fatalf("Result: %v", err)
	}
	if n := len(w.pending); n != 0 {
		t.Fatalf("%d reply/replies queued, want 0", n)
	}
}

// The buffer must not let one stale reply block the real one — the old
// capacity-1 channel dropped whatever arrived while it was occupied.
func TestAStaleReplyDoesNotCrowdOutTheRealOne(t *testing.T) {
	w := newTestWM()
	cb := &callback{w}
	for i := 1; i <= 4; i++ {
		_ = cb.Result("1", "stale")
	}
	_ = cb.Result("9", "real")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := w.awaitReply(ctx, 9)
	if err != nil {
		t.Fatalf("awaitReply: %v", err)
	}
	if got != "real" {
		t.Fatalf("got %q, want %q", got, "real")
	}
}
