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

func (f *fakeDeliverer) Deliver(_ context.Context, req Delivery) (int, error) {
	var delivered []WindowID
	for _, t := range req.Targets {
		if t == req.Origin {
			continue
		}
		delivered = append(delivered, t)
	}
	f.mu.Lock()
	f.calls = append(f.calls, deliverCall{ev: req.Event, origin: req.Origin, targets: append([]WindowID(nil), req.Targets...), delivered: delivered})
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

// TestActiveFailureSkipsBroadcastEvenWithGateOff: an unresolvable focused
// window is fail-closed whatever the gate says. Broadcasting blind would
// deliver to a window passthrough already served, doubling every keystroke on
// the client being played — the bug this mechanism exists to prevent.
func TestActiveFailureSkipsBroadcastEvenWithGateOff(t *testing.T) {
	rec := &recorder{active: "origin", activeErr: errors.New("kwin script timeout")}
	e, src, d := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "t2"})
	e.SetGateOrigin(false)

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	if calls := d.snapshot(); len(calls) != 0 {
		t.Fatalf("want no delivery when the focused window is unknown, got %v", calls)
	}
	if !hasNotice(notices(e), "active window") {
		t.Fatal("the suppressed broadcast was not reported")
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

// TestDelivererSwapIsSafeMidDelivery exercises the contract SetDeliverer's doc
// comment makes: a delivery already in flight finishes against the backend it
// started with, and the next one uses the new backend. Documented happens-before
// claims are worth nothing untested, since -race only sees executed paths.
func TestDelivererSwapIsSafeMidDelivery(t *testing.T) {
	rec := &recorder{active: "t1"}
	e, src, _ := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "t2"})

	// A backend that blocks inside Deliver until released, so a swap is
	// guaranteed to land while a delivery is genuinely in flight.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	slow := &blockingDeliverer{entered: entered, release: release}
	e.SetDeliverer(slow)

	a, _ := keys.Parse("a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.Run(ctx) }()

	src.ch <- keys.Event{Code: a, State: keys.Down}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery never started")
	}

	swapped := &fakeDeliverer{}
	e.SetDeliverer(swapped) // mid-flight swap
	close(release)

	src.ch <- keys.Event{Code: a, State: keys.Up}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(swapped.snapshot()) > 0 {
			return // the second event went to the new backend: contract holds
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the swapped-in deliverer never received the next event")
}

// TestDelivererSwapUnderLoad hammers SetDeliverer while the broadcast worker
// reads the field, so the race detector sees a genuinely concurrent
// read/write pair. TestDelivererSwapIsSafeMidDelivery above covers the
// semantics; this one covers the memory safety the doc comment claims.
// Verified by removing delivMu and confirming -race then reports the race.
func TestDelivererSwapUnderLoad(t *testing.T) {
	rec := &recorder{active: "t1"}
	e, src, _ := newFakeEngine(t, rec)
	e.SetTargets([]WindowID{"t1", "t2"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.Run(ctx) }()

	swapping := make(chan struct{})
	go func() {
		defer close(swapping)
		for i := 0; i < 500; i++ {
			e.SetDeliverer(&fakeDeliverer{})
		}
	}()

	a, _ := keys.Parse("a")
	for i := 0; i < 500; i++ {
		select {
		case src.ch <- keys.Event{Code: a, State: keys.Down}:
		case <-time.After(time.Second):
			t.Fatal("engine stopped consuming events")
		}
	}
	<-swapping
}

type blockingDeliverer struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingDeliverer) MovesFocus() bool { return false }
func (b *blockingDeliverer) Close() error     { return nil }
func (b *blockingDeliverer) Deliver(_ context.Context, req Delivery) (int, error) {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-b.release
	return len(req.Targets), nil
}

// --- held keys (REFERENCE.md: stuck keys) ------------------------------------
//
// Every reason not to broadcast a key applies equally to its Up, and dropping
// an Up does not skip a keystroke — it leaves the key held down in the game.
// Autorun on, a spell key stuck, a character running into a wall.

// runLive starts the engine and lets the test change settings between
// keystrokes, which is the whole shape of these failures.
func runLive(t *testing.T, e *Engine, src *fakeSource, body func(send func(keys.Event))) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	body(func(ev keys.Event) {
		src.ch <- ev
		time.Sleep(40 * time.Millisecond) // let the worker finish this one
	})
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
}

func logHas(got []string, want string) bool {
	for _, s := range got {
		if s == want {
			return true
		}
	}
	return false
}

func assertHas(t *testing.T, got []string, want string) {
	t.Helper()
	if !logHas(got, want) {
		t.Fatalf("missing %q in %v", want, got)
	}
}

func assertLacks(t *testing.T, got []string, unwanted string) {
	t.Helper()
	if logHas(got, unwanted) {
		t.Fatalf("unexpected %q in %v", unwanted, got)
	}
}

func heldEngine(t *testing.T, rec *recorder, src *fakeSource, targets ...WindowID) (*Engine, keys.Code) {
	t.Helper()
	e := newEngine(rec, src)
	e.SetTargets(targets)
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)
	return e, a
}

// Turning broadcast off between the Down and the Up was the simplest way to
// strand a key: the Up arrived at a disabled engine and went nowhere.
func TestDisablingBroadcastReleasesHeldKeys(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, a := heldEngine(t, rec, src, "t1", "origin")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		assertHas(t, rec.snapshot(), "emit@t1:A/down")
		e.SetEnabled(false)
		time.Sleep(40 * time.Millisecond)
		send(keys.Event{Code: a, State: keys.Up})
	})
	assertHas(t, rec.snapshot(), "emit@t1:A/up")
}

// The same for the allowlist: a key dropped from it mid-press would never see
// its Up broadcast.
func TestFilterChangeReleasesHeldKeys(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, a := heldEngine(t, rec, src, "t1", "origin")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		b, _ := keys.Parse("b")
		e.SetFilter(keys.Of(b)) // 'a' is no longer broadcast at all
		time.Sleep(40 * time.Millisecond)
	})
	assertHas(t, rec.snapshot(), "emit@t1:A/up")
}

// The origin gate closing is not a reason to leave a key down. The gate asks
// whether to START broadcasting from this window.
func TestGateClosingStillReleasesHeldKeys(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, a := heldEngine(t, rec, src, "t1", "origin")
	if !e.GateOrigin() {
		t.Fatal("this test needs the origin gate on")
	}

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		assertHas(t, rec.snapshot(), "emit@t1:A/down")
		// Focus moves to something that is not a target: the gate shuts.
		rec.mu.Lock()
		rec.active = "a-browser"
		rec.mu.Unlock()
		send(keys.Event{Code: a, State: keys.Up})
	})
	assertHas(t, rec.snapshot(), "emit@t1:A/up")
}

// A release goes to the windows that were sent the Down, not to whatever is
// ticked now — the windows that left the list are exactly the ones nothing
// else would ever release.
func TestTargetChangeReleasesTheOldTargets(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, a := heldEngine(t, rec, src, "t1", "origin")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		assertHas(t, rec.snapshot(), "emit@t1:A/down")
		e.SetTargets([]WindowID{"t2", "origin"})
		time.Sleep(40 * time.Millisecond)
	})
	got := rec.snapshot()
	assertHas(t, got, "emit@t1:A/up")   // the holder
	assertLacks(t, got, "emit@t2:A/up") // never had it down
}

// An ordinary Down/Up leaves nothing held, so a stray second Up is not
// broadcast — the bypass must not become a permanent hole in the filter.
func TestAReleasedKeyIsNoLongerHeld(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, a := heldEngine(t, rec, src, "t1", "origin")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		send(keys.Event{Code: a, State: keys.Up})
		if n := len(e.heldTargets(a)); n != 0 {
			t.Fatalf("%d target(s) still holding A after its Up", n)
		}
		e.SetEnabled(false)
		b, _ := keys.Parse("b")
		e.SetFilter(keys.Of(b))
		time.Sleep(40 * time.Millisecond)
		// A second Up with nothing held: passthrough only.
		send(keys.Event{Code: a, State: keys.Up})
	})
	// Exactly one broadcast Up to t1, from the real release.
	n := 0
	for _, s := range rec.snapshot() {
		if s == "emit@t1:A/up" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("broadcast %d Ups to t1, want exactly 1 (log: %v)", n, rec.snapshot())
	}
}

// A full queue drops the newest event rather than blocking passthrough — but
// not when that event is the Up of a held key. Driven directly rather than by
// timing: the queue is filled, the key marked held, and one slot freed.
func TestFullQueueDoesNotDropARelease(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, a := heldEngine(t, rec, src, "t1", "origin")

	for len(e.queue) < cap(e.queue) { // full, with no worker running
		e.queue <- keys.Event{Code: a, State: keys.Repeat}
	}
	e.noteDelivered(keys.Event{Code: a, State: keys.Down}, []WindowID{"t1"})

	done := make(chan struct{})
	go func() { e.handle(keys.Event{Code: a, State: keys.Up}); close(done) }()

	time.Sleep(30 * time.Millisecond) // the release is waiting for room
	<-e.queue                         // make one slot
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handle never enqueued the release")
	}

	var ups int
	for len(e.queue) > 0 {
		if ev := <-e.queue; ev.State == keys.Up {
			ups++
		}
	}
	if ups != 1 {
		t.Fatalf("queued %d Ups, want 1 — the release was dropped", ups)
	}
}
