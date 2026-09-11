package broadcast

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/malahmen/clonecast/internal/keys"
)

type fakeSource struct{ ch chan keys.Event }

func (f *fakeSource) Events() <-chan keys.Event { return f.ch }
func (f *fakeSource) Close() error              { return nil }

// recorder captures the exact sequence of injector and window manager calls.
// Since the engine now calls Emit from two goroutines (passthrough and the
// broadcast worker — see engine.go's package doc), active is guarded by mu
// too, not just log: Emit reads it while Activate, running concurrently on
// the worker, can be writing it.
type recorder struct {
	mu     sync.Mutex
	log    []string
	active WindowID
	// delay, if set, is slept at the top of Activate — lets tests simulate a
	// slow compositor round-trip without touching real timers elsewhere.
	delay time.Duration
	// activeErr, if set, makes Active fail — the compositor-hiccup path the
	// engine has to decide about (see Engine.origin).
	activeErr error
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
}

func (r *recorder) Emit(ev keys.Event) error {
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	r.add("emit@" + string(active) + ":" + keys.Name(ev.Code) + "/" + ev.State.String())
	return nil
}
func (r *recorder) Close() error { return nil }

func (r *recorder) List(context.Context) ([]Window, error) { return nil, nil }
func (r *recorder) Active(context.Context) (WindowID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.activeErr != nil {
		return "", r.activeErr
	}
	return r.active, nil
}
func (r *recorder) Activate(_ context.Context, id WindowID) error {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	r.active = id
	r.mu.Unlock()
	r.add("activate:" + string(id))
	return nil
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

func run(t *testing.T, e *Engine, src *fakeSource, events ...keys.Event) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	for _, ev := range events {
		src.ch <- ev
	}
	// Let the engine drain: it processes strictly in order, so a final
	// synthetic event with a fresh channel would be over-engineering here.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
}

func newEngine(rec *recorder, src *fakeSource) *Engine {
	cfg := DefaultConfig()
	cfg.SettleDelay = 0
	return New(src, rec, rec, cfg)
}

func TestDisabledOnlyPassesThrough(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	e.SetTargets([]WindowID{"t1"})
	e.SetFilter(keys.All())

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	want := []string{"emit@origin:A/down"}
	assertLog(t, rec.snapshot(), want)
}

func TestBroadcastFocusDance(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	e.SetTargets([]WindowID{"t1", "origin", "t2"})
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)

	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	want := []string{
		"emit@origin:A/down",
		"activate:t1", "emit@t1:A/down",
		"activate:t2", "emit@t2:A/down",
		"activate:origin",
	}
	assertLog(t, rec.snapshot(), want)
}

func TestFilterAndRepeatAreNotBroadcast(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	e.SetTargets([]WindowID{"t1"})
	a, _ := keys.Parse("a")
	b, _ := keys.Parse("b")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)

	run(t, e, src,
		keys.Event{Code: b, State: keys.Down},   // filtered out
		keys.Event{Code: a, State: keys.Repeat}, // repeats never broadcast
	)

	want := []string{"emit@origin:B/down", "emit@origin:A/repeat"}
	assertLog(t, rec.snapshot(), want)
}

// TestPassthroughNeverFiresWhileADanceHoldsFocus is the regression test for
// the bug described in engine.go's package doc: an earlier, naive version of
// the decoupling fix let passthrough for a later key fire immediately even
// while a dance was actively holding real focus on some other window,
// delivering it to the wrong place. Correctness requires the opposite of
// "never blocked" — passthrough must wait for a dance that's actually in
// flight right now.
//
// The recorder's Emit logs "emit@<active>", where <active> is whatever
// Activate last set — i.e. exactly what a real WindowManager would report
// has focus at that instant. If passthrough for 'b' fired while the dance
// had "t1" focused, this test would see "emit@t1:B/down" instead of
// "emit@origin:B/down", which is precisely the misdirected-keystroke bug.
func TestPassthroughNeverFiresWhileADanceHoldsFocus(t *testing.T) {
	rec := &recorder{active: "origin", delay: 50 * time.Millisecond}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	// "origin" is ticked as well as t1: with the origin gate on (the default)
	// the focused window must itself be a target for anything to broadcast,
	// and the dance skips it as the master. The dance's visible steps are
	// therefore unchanged: activate t1, emit, restore origin.
	e.SetTargets([]WindowID{"t1", "origin"})
	a, _ := keys.Parse("a")
	b, _ := keys.Parse("b")
	e.SetFilter(keys.Of(a)) // only 'a' broadcasts; 'b' is passthrough-only
	e.SetEnabled(true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()

	src.ch <- keys.Event{Code: a, State: keys.Down} // dance: activate(t1)[50ms], emit, activate(origin)[50ms]
	time.Sleep(10 * time.Millisecond)               // let the dance start and take focusMu
	src.ch <- keys.Event{Code: b, State: keys.Down} // must wait for the dance, not fire mid-dance

	// Comfortably longer than the ~100ms dance plus b's own passthrough.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	got := rec.snapshot()
	restoreIdx, bIdx := -1, -1
	for i, l := range got {
		if l == "activate:origin" && restoreIdx == -1 {
			restoreIdx = i
		}
		if l == "emit@origin:B/down" {
			bIdx = i
		}
	}
	if restoreIdx == -1 {
		t.Fatalf("dance never restored focus to origin: %v", got)
	}
	if bIdx == -1 {
		t.Fatalf("passthrough for 'b' never happened (correctly, as emit@origin — got instead: %v)", got)
	}
	if bIdx < restoreIdx {
		t.Fatalf("passthrough for 'b' logged before the dance's restore step at index %d vs %d — "+
			"it must have fired while focus was still on t1: %v", bIdx, restoreIdx, got)
	}
}

func TestToggleKeyIsConsumed(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	toggle := e.cfg.ToggleKey

	run(t, e, src,
		keys.Event{Code: toggle, State: keys.Down},
		keys.Event{Code: toggle, State: keys.Up},
	)

	if !e.Enabled() {
		t.Fatal("toggle key should have enabled broadcasting")
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("toggle key leaked to injector: %v", got)
	}
}

// ---- origin (dynamic master) and the origin gate ---------------------------

// fakeDeliverer stands in for a focus-free backend (agent/xsend): it records
// what the engine handed it and honours the contract that origin is skipped.
type fakeDeliverer struct {
	mu    sync.Mutex
	calls []deliverCall
}

type deliverCall struct {
	ev        keys.Event
	origin    WindowID
	targets   []WindowID
	delivered []WindowID
}

func (f *fakeDeliverer) MovesFocus() bool { return false }
func (f *fakeDeliverer) Close() error     { return nil }

func (f *fakeDeliverer) Deliver(_ context.Context, ev keys.Event, targets []WindowID, origin WindowID, _ func(string, bool)) (int, error) {
	var delivered []WindowID
	for _, t := range targets {
		if t == origin {
			continue
		}
		delivered = append(delivered, t)
	}
	f.mu.Lock()
	f.calls = append(f.calls, deliverCall{ev: ev, origin: origin, targets: append([]WindowID(nil), targets...), delivered: delivered})
	f.mu.Unlock()
	return len(delivered), nil
}

func (f *fakeDeliverer) snapshot() []deliverCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]deliverCall(nil), f.calls...)
}

// notices drains whatever the engine reported; the channel is buffered, so
// this is safe to call after the engine has stopped.
func notices(e *Engine) []Notice {
	var out []Notice
	for {
		select {
		case n := <-e.Notices():
			out = append(out, n)
		default:
			return out
		}
	}
}

func hasNotice(ns []Notice, substr string) bool {
	for _, n := range ns {
		if strings.Contains(n.Text, substr) {
			return true
		}
	}
	return false
}

func newFakeEngine(t *testing.T, rec *recorder) (*Engine, *fakeSource, *fakeDeliverer) {
	t.Helper()
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	d := &fakeDeliverer{}
	e.SetDeliverer(d)
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)
	return e, src, d
}

// TestFocusFreeDelivererSkipsOrigin is the fix for the double-delivery in
// research §B O1: a focus-free backend used to receive every ticked target,
// so the focused one got the key twice (passthrough + delivery). The engine
// must resolve the master itself and hand it down, and it must never appear
// in what the deliverer actually sends to.
func TestFocusFreeDelivererSkipsOrigin(t *testing.T) {
	rec := &recorder{active: "origin"}
	e, src, d := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "origin", "t2"})

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	calls := d.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 delivery, got %d (%v)", len(calls), calls)
	}
	if calls[0].origin != "origin" {
		t.Fatalf("deliverer got origin %q, want %q", calls[0].origin, "origin")
	}
	if got, want := calls[0].delivered, []WindowID{"t1", "t2"}; !sameIDs(got, want) {
		t.Fatalf("delivered to %v, want %v (the focused window must not be in there)", got, want)
	}
	// The master still got the key exactly once, through passthrough.
	assertLog(t, rec.snapshot(), []string{"emit@origin:A/down"})
}

// TestOriginGateBlocksNonTargetOrigin: typing in the terminal that runs
// clonecast, with broadcasting on, must not drive every client (research
// §C1). The gate is on by default.
func TestOriginGateBlocksNonTargetOrigin(t *testing.T) {
	rec := &recorder{active: "konsole"}
	e, src, d := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "t2"})

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	if calls := d.snapshot(); len(calls) != 0 {
		t.Fatalf("broadcast from a non-target origin: %v", calls)
	}
	if !hasNotice(notices(e), "origin gate") {
		t.Fatal("the gate blocked a broadcast without saying so")
	}
	assertLog(t, rec.snapshot(), []string{"emit@konsole:A/down"}) // passthrough only
}

// TestOriginGateOffBroadcastsFromAnyWindow: with the gate lifted, the focused
// window need not be a target — but origin skipping still applies.
func TestOriginGateOffBroadcastsFromAnyWindow(t *testing.T) {
	rec := &recorder{active: "konsole"}
	e, src, d := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "t2"})
	e.SetGateOrigin(false)

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	calls := d.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 delivery, got %d (%v)", len(calls), calls)
	}
	if calls[0].origin != "konsole" {
		t.Fatalf("deliverer got origin %q, want %q", calls[0].origin, "konsole")
	}
	if got, want := calls[0].delivered, []WindowID{"t1", "t2"}; !sameIDs(got, want) {
		t.Fatalf("delivered to %v, want %v", got, want)
	}
}

// TestActiveFailureSkipsBroadcastWhenGated: if the compositor cannot say what
// has focus we must not broadcast blindly — with the gate on the event is
// dropped and reported, never delivered to everything (see Engine.origin).
func TestActiveFailureSkipsBroadcastWhenGated(t *testing.T) {
	rec := &recorder{active: "origin", activeErr: errors.New("kwin script timeout")}
	e, src, d := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "t2"})

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	if calls := d.snapshot(); len(calls) != 0 {
		t.Fatalf("broadcast with an unknown origin: %v", calls)
	}
	ns := notices(e)
	if !hasNotice(ns, "kwin script timeout") {
		t.Fatalf("the wm.Active failure was not reported: %v", ns)
	}
	// Passthrough still works: it never needed to know which window has
	// focus, only the broadcast step does.
	assertLog(t, rec.snapshot(), []string{"emit@origin:A/down"})
}

// TestActiveFailureDeliversToAllWhenGateOff: with the gate off the user has
// already said focus decides nothing, so an unknown origin falls back to
// pre-origin behaviour — every ticked target, nothing skipped.
func TestActiveFailureDeliversToAllWhenGateOff(t *testing.T) {
	rec := &recorder{active: "origin", activeErr: errors.New("kwin script timeout")}
	e, src, d := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "t2"})
	e.SetGateOrigin(false)

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	calls := d.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want 1 delivery, got %d (%v)", len(calls), calls)
	}
	if calls[0].origin != "" {
		t.Fatalf("origin %q, want empty (unknown)", calls[0].origin)
	}
	if got, want := calls[0].delivered, []WindowID{"t1", "t2"}; !sameIDs(got, want) {
		t.Fatalf("delivered to %v, want %v", got, want)
	}
	if !hasNotice(notices(e), "gate off") {
		t.Fatal("falling back to broadcasting everywhere was not reported")
	}
}

func sameIDs(got, want []WindowID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func assertLog(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d calls %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
