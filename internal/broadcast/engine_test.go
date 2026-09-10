package broadcast

import (
	"context"
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
	// "origin" is ticked alongside "t1" so the default RequireOriginTicked
	// gate (4.15) lets this broadcast through — origin is part of the
	// multibox session, it just already got the key via passthrough.
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

// fakeFocusFreeDeliverer mimics the shape of the agent/xsend backends (no
// focus movement): it records exactly the origin and target set it was
// handed, so tests can confirm the *engine* resolves origin once per
// broadcast and that origin-skipping isn't dance-specific behavior anymore
// (REFERENCE.md 4.15/7.10) — every backend gets origin from the engine and is
// expected to skip it itself, same as the real agent/x11 deliverers do.
type fakeFocusFreeDeliverer struct {
	mu    sync.Mutex
	calls []call
}

type call struct {
	origin  WindowID
	targets []WindowID
}

func (f *fakeFocusFreeDeliverer) MovesFocus() bool { return false }
func (f *fakeFocusFreeDeliverer) Close() error     { return nil }

func (f *fakeFocusFreeDeliverer) Deliver(_ context.Context, _ keys.Event, origin WindowID, targets []WindowID, _ func(string, bool)) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call{origin: origin, targets: append([]WindowID(nil), targets...)})
	f.mu.Unlock()

	delivered := 0
	for _, t := range targets {
		if t != origin { // exactly what the real agent/x11 deliverers do
			delivered++
		}
	}
	return delivered, nil
}

func (f *fakeFocusFreeDeliverer) snapshot() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]call(nil), f.calls...)
}

// TestDynamicMasterOriginSkippedByNonDanceBackend is the regression test for
// the bug 4.15 fixes: before it, only danceDeliverer resolved and skipped
// origin — agentDeliverer and the xsend deliverer broadcast to every ticked
// target unconditionally, so a focused target ticked alongside its peers
// received the key twice (once via passthrough, once via the backend). Here,
// with a focus-free fake deliverer standing in for agent/xsend, ticking the
// currently-focused window ("malahmen") alongside "marx" must still result in
// exactly one delivered target (marx) — proving the *engine*, not the
// deliverer, is what resolves and threads origin through, so the fix applies
// uniformly to every backend, not just the dance.
func TestDynamicMasterOriginSkippedByNonDanceBackend(t *testing.T) {
	rec := &recorder{active: "malahmen"} // focused window is a ticked target
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	fd := &fakeFocusFreeDeliverer{}
	e.SetDeliverer(fd)
	e.SetTargets([]WindowID{"malahmen", "marx"})
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)

	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	calls := fd.snapshot()
	if len(calls) != 1 {
		t.Fatalf("want exactly 1 Deliver call, got %d: %+v", len(calls), calls)
	}
	if calls[0].origin != "malahmen" {
		t.Fatalf("origin should be the focused window %q, got %q", "malahmen", calls[0].origin)
	}
	delivered := 0
	for _, tgt := range calls[0].targets {
		if tgt != calls[0].origin {
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("want exactly 1 non-origin target delivered (marx), got %d from targets %v", delivered, calls[0].targets)
	}
}

// TestBroadcastGatedWhenOriginNotTicked verifies Config.RequireOriginTicked
// (default on, 4.15): if the focused window isn't one of the ticked targets —
// e.g. you tabbed to an unrelated window like the terminal — broadcast is
// suppressed entirely, even though passthrough still fires. This is what
// makes "tick all your clients" safe without a separate own-window exclusion.
func TestBroadcastGatedWhenOriginNotTicked(t *testing.T) {
	rec := &recorder{active: "konsole"} // focused, but NOT ticked
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	fd := &fakeFocusFreeDeliverer{}
	e.SetDeliverer(fd)
	e.SetTargets([]WindowID{"malahmen", "marx"})
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)

	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	if calls := fd.snapshot(); len(calls) != 0 {
		t.Fatalf("broadcast should be gated (origin %q not ticked), but Deliver was called: %+v", rec.active, calls)
	}
	want := []string{"emit@konsole:A/down"} // passthrough still happens
	assertLog(t, rec.snapshot(), want)
}

// TestBroadcastGateCanBeDisabled confirms RequireOriginTicked=false restores
// the pre-4.15 behavior: broadcast to every ticked target regardless of what
// currently has focus.
func TestBroadcastGateCanBeDisabled(t *testing.T) {
	rec := &recorder{active: "konsole"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	cfg := DefaultConfig()
	cfg.SettleDelay = 0
	cfg.RequireOriginTicked = false
	e := New(src, rec, rec, cfg)
	fd := &fakeFocusFreeDeliverer{}
	e.SetDeliverer(fd)
	e.SetTargets([]WindowID{"malahmen", "marx"})
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)

	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	if calls := fd.snapshot(); len(calls) != 1 {
		t.Fatalf("want exactly 1 Deliver call with the gate disabled, got %d: %+v", len(calls), calls)
	}
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
