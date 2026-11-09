package broadcast

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/malahmen/clonecast/internal/keys"
)

// Held keys after Kamino 134-136 and 202. These use the focus-free fakes
// (fakeDeliverer, reachDeliverer): no shipped backend moves focus.

// focusFreeEngine is g1 (the master, focused) and g2 ticked, A broadcast,
// broadcasting on, delivered by a fakeDeliverer.
func focusFreeEngine(rec *recorder, targets ...WindowID) (*Engine, *fakeSource, *fakeDeliverer, keys.Code) {
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := New(src, rec, rec, DefaultConfig())
	d := &fakeDeliverer{}
	e.SetDeliverer(d)
	e.SetTargets(targets)
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)
	return e, src, d, a
}

// ups lists the windows each delivered Up reached, in order.
func ups(d *fakeDeliverer) [][]WindowID {
	var out [][]WindowID
	for _, c := range d.snapshot() {
		if c.ev.State == keys.Up {
			out = append(out, c.delivered)
		}
	}
	return out
}

func sameSet(got, want []WindowID) bool {
	if len(got) != len(want) {
		return false
	}
	for _, w := range want {
		if !contains(got, w) {
			return false
		}
	}
	return true
}

// Kamino 134: the TUI pushes the target list every 5 s. An identical list must
// release nothing: it used to release every held key, so a held W stopped
// every clone within 5 s, and the master got an Up too.
func TestSetTargetsWithTheSameListReleasesNothing(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		e.SetTargets([]WindowID{"g1", "g2"})
		time.Sleep(40 * time.Millisecond)
	})
	if u := ups(d); len(u) != 0 {
		t.Fatalf("an identical target list released A: Ups to %v", u)
	}
	if got := e.heldTargets(a); !sameSet(got, []WindowID{"g2"}) {
		t.Fatalf("A held on %v, want [g2] still", got)
	}
}

// Kamino 134: only the windows that left the list are released. g2 stays and
// keeps walking; g3 left and is released.
func TestSetTargetsReleasesOnlyTheWindowsThatLeft(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2", "g3")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		e.SetTargets([]WindowID{"g1", "g2"})
		time.Sleep(40 * time.Millisecond)
	})
	u := ups(d)
	if len(u) != 1 || !sameSet(u[0], []WindowID{"g3"}) {
		t.Fatalf("Ups to %v, want one, to [g3] only", u)
	}
	if got := e.heldTargets(a); !sameSet(got, []WindowID{"g2"}) {
		t.Fatalf("A held on %v, want [g2]", got)
	}
}

// Kamino 134: the master got the key by passthrough and was skipped by the
// broadcast, so it holds nothing through the deliverer. Recording it as held
// sent it an Up through its agent at the next release, mid-press.
func TestBroadcastNeverRecordsTheSkippedOrigin(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		if got := e.heldTargets(a); !sameSet(got, []WindowID{"g2"}) {
			t.Errorf("A held on %v, want [g2]: g1 is the origin", got)
		}
		b, _ := keys.Parse("b")
		e.SetFilter(keys.Of(b)) // releases everything held
		time.Sleep(40 * time.Millisecond)
	})
	for _, to := range ups(d) {
		if contains(to, "g1") {
			t.Fatalf("an Up went to the origin g1 through the deliverer: %v", ups(d))
		}
	}
}

// Kamino 135: broadcast off, W down takes passthrough; broadcast on, W up
// takes the agent. The virtual keyboard must get the Up too, or it holds W
// for good and the compositor hands it to whatever has focus.
func TestAnUpRoutedToTheAgentReleasesThePassthroughDown(t *testing.T) {
	w, _ := keys.Parse("w")
	cases := []struct {
		name string
		// between runs after the Down and before the Up.
		between func(e *Engine, rec *recorder)
		start   WindowID
		enabled bool
	}{
		{"broadcast turned on mid-press", func(e *Engine, _ *recorder) { e.SetEnabled(true) }, "g1", false},
		{"a game clicked after the terminal", func(_ *Engine, rec *recorder) {
			rec.mu.Lock()
			rec.active = "g1"
			rec.mu.Unlock()
		}, "terminal", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{active: c.start}
			src := &fakeSource{ch: make(chan keys.Event, 8)}
			e, d := masterEngine(rec, src, "g1", "g2")
			e.SetEnabled(c.enabled)
			runLive(t, e, src, func(send func(keys.Event)) {
				send(keys.Event{Code: w, State: keys.Down})
				c.between(e, rec)
				send(keys.Event{Code: w, State: keys.Up})
			})
			got := rec.snapshot()
			if !logHas(got, "emit@"+string(c.start)+":W/down") {
				t.Fatalf("the Down did not take passthrough: %v", got)
			}
			if !logHas(got, "emit@g1:W/up") {
				t.Fatalf("the virtual keyboard never got W up: %v (deliveries %+v)", got, d.snapshot())
			}
			if e.passthroughHolds(w) {
				t.Fatal("passthrough still records W as down")
			}
		})
	}
}

// Kamino 135, the other side: an Up routed to the agent for a key passthrough
// does not hold is not replayed to the compositor.
func TestAnAgentUpIsNotReplayedWithoutAPassthroughDown(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, _ := masterEngine(rec, src, "g1", "g2")
	w, _ := keys.Parse("w")
	run(t, e, src, keys.Event{Code: w, State: keys.Down}, keys.Event{Code: w, State: keys.Up})
	if n := emits(rec); n != 0 {
		t.Fatalf("%d passthrough emit(s), want none: %v", n, rec.snapshot())
	}
}

// Kamino 136(1): an Up that did not reach a window (no agent, a full queue)
// leaves the key held there, so the next release tries again. It used to be
// recorded as delivered, and the window kept the key down.
func TestAnUnreachedUpStaysHeld(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2", "g3")
	d.setUnreachable("g3", true) // never got the Down

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		if got := e.heldTargets(a); !sameSet(got, []WindowID{"g2"}) {
			t.Errorf("A held on %v, want [g2]: the Down never reached g3", got)
		}
		d.setUnreachable("g2", true)
		send(keys.Event{Code: a, State: keys.Up})
		if got := e.heldTargets(a); !sameSet(got, []WindowID{"g2"}) {
			t.Errorf("A held on %v after an Up that reached nobody, want [g2]", got)
		}
		d.setUnreachable("g2", false)
		e.SetEnabled(false) // releases what is held
		time.Sleep(40 * time.Millisecond)
	})
	u := ups(d)
	if len(u) == 0 || !sameSet(u[len(u)-1], []WindowID{"g2"}) {
		t.Fatalf("Ups to %v, want the last to reach g2", u)
	}
	if got := e.heldTargets(a); len(got) != 0 {
		t.Fatalf("A still held on %v after the release reached it", got)
	}
}

// Kamino 136(2): a Down still in the queue when broadcast goes off is
// delivered after the switch, so releaseHeld has to know about it. held was
// written only after delivery, and the Down went out with no Up behind it.
// Driven without a worker, so the Down is certainly still queued.
func TestAQueuedDownIsReleasedWhenBroadcastGoesOff(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2")

	_ = e.handle(keys.Event{Code: a, State: keys.Down}) // queued, not delivered
	e.SetEnabled(false)
	run(t, e, src) // the worker drains: the Down, then the release

	calls := d.snapshot()
	if len(calls) == 0 || calls[0].ev.State != keys.Down {
		t.Fatalf("deliveries %+v, want the queued Down first", calls)
	}
	last := calls[len(calls)-1]
	if last.ev.State != keys.Up || !sameSet(last.delivered, []WindowID{"g2"}) {
		t.Fatalf("deliveries %+v, want an Up to g2 after the Down", calls)
	}
	if got := e.heldTargets(a); len(got) != 0 {
		t.Fatalf("A still held on %v", got)
	}
}

// Kamino 136(2): the user's own Up arriving while its Down is still queued is
// a release, whatever the filter says by then.
func TestAnUpBehindAQueuedDownIsARelease(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2")

	_ = e.handle(keys.Event{Code: a, State: keys.Down})
	e.mu.Lock()
	e.filter = keys.Set{} // A no longer broadcast, without releaseHeld
	e.mu.Unlock()
	_ = e.handle(keys.Event{Code: a, State: keys.Up})
	run(t, e, src)

	u := ups(d)
	if len(u) != 1 || !sameSet(u[0], []WindowID{"g2"}) {
		t.Fatalf("Ups to %v, want one, to g2", u)
	}
}

// Kamino 202: quitting with keys held released nothing. Close releases them
// through the deliverer and on the virtual keyboard.
func TestCloseReleasesHeldKeys(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2")
	b, _ := keys.Parse("b") // passthrough only: not in the filter

	run(t, e, src, keys.Event{Code: a, State: keys.Down}, keys.Event{Code: b, State: keys.Down})
	e.Close()

	u := ups(d)
	if len(u) != 1 || !sameSet(u[0], []WindowID{"g2"}) {
		t.Fatalf("Ups to %v, want one, to g2", u)
	}
	got := rec.snapshot()
	assertHas(t, got, "emit@g1:A/up")
	assertHas(t, got, "emit@g1:B/up")
	if !hasNotice(notices(e), "releasing 2 held key(s)") {
		t.Error("Close did not say what it released")
	}
}

// With master by agent, the master's own keys are held through its agent.
func TestCloseReleasesTheAgentMaster(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, d := masterEngine(rec, src, "g1", "g2")
	w, _ := keys.Parse("w")

	run(t, e, src, keys.Event{Code: w, State: keys.Down})
	e.Close()

	calls := d.snapshot()
	last := calls[len(calls)-1]
	if last.ev.State != keys.Up || !sameSet(last.delivered, []WindowID{"g1"}) {
		t.Fatalf("last delivery %v to %v, want W up to the master g1", last.ev, last.delivered)
	}
	if n := emits(rec); n != 0 {
		t.Fatalf("%d passthrough emit(s), want none: passthrough never had W", n)
	}
}

// closingSource counts Close calls, which is what releases the grab.
type closingSource struct {
	fakeSource
	closes atomic.Int32
}

func (c *closingSource) Close() error { c.closes.Add(1); return nil }

// failingInjector is a virtual keyboard that is gone.
type failingInjector struct{ recorder }

func (f *failingInjector) Emit(keys.Event) error {
	return errors.New("write /dev/uinput: no such device")
}

// Kamino 202: a failing Emit left the keyboard grabbed and dead: the engine
// reported it per key and went on. It now releases the grab (closes the
// source), says so, and stops.
func TestAFailingEmitReleasesTheKeyboard(t *testing.T) {
	inj := &failingInjector{recorder{active: "g1"}}
	src := &closingSource{fakeSource: fakeSource{ch: make(chan keys.Event, 8)}}
	e := New(src, inj, inj, DefaultConfig())
	e.SetDeliverer(&fakeDeliverer{})
	a, _ := keys.Parse("a")

	done := make(chan error, 1)
	go func() { done <- e.Run(context.Background()) }()
	src.ch <- keys.Event{Code: a, State: keys.Down}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "keyboard released") {
			t.Fatalf("Run returned %v, want the virtual keyboard failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run kept going on a dead virtual keyboard")
	}
	if n := src.closes.Load(); n != 1 {
		t.Fatalf("source closed %d times, want once (that is the ungrab)", n)
	}
	if !hasNotice(notices(e), "released the keyboard") {
		t.Fatal("the release was not reported")
	}
}

// Kamino 202: notices reach the log file, and REFERENCE.md 8 promises no
// record of what was typed. None of the error paths may name the key, nor
// carry its code.
func TestNoticesDoNotNameTheKey(t *testing.T) {
	slash, _ := keys.Parse("slash")
	secret := []string{keys.Name(slash), keys.Event{Code: slash, State: keys.Down}.String(), "53"}
	check := func(t *testing.T, ns []Notice) {
		t.Helper()
		if len(ns) == 0 {
			t.Fatal("no notice at all")
		}
		for _, n := range ns {
			for _, s := range secret {
				if strings.Contains(n.Text, s) {
					t.Errorf("notice %q names the key (%q)", n.Text, s)
				}
			}
		}
	}

	t.Run("failing emit", func(t *testing.T) {
		inj := &failingInjector{recorder{active: "g1"}}
		e := New(&fakeSource{ch: make(chan keys.Event)}, inj, inj, DefaultConfig())
		_ = e.handle(keys.Event{Code: slash, State: keys.Down})
		check(t, notices(e))
	})
	t.Run("queue full", func(t *testing.T) {
		rec := &recorder{active: "g1"}
		e, _, _, _ := focusFreeEngine(rec, "g1", "g2")
		e.SetFilter(keys.Of(slash))
		_ = notices(e)
		for len(e.queue) < cap(e.queue) {
			e.queue <- item{}
		}
		_ = e.handle(keys.Event{Code: slash, State: keys.Down})
		check(t, notices(e))
	})
	t.Run("queue full for a release", func(t *testing.T) {
		rec := &recorder{active: "g1"}
		e, _, _, _ := focusFreeEngine(rec, "g1", "g2")
		e.noteDelivered(keys.Event{Code: slash, State: keys.Down}, []WindowID{"g2"})
		_ = notices(e)
		for len(e.queue) < cap(e.queue) {
			e.queue <- item{}
		}
		_ = e.handle(keys.Event{Code: slash, State: keys.Up}) // waits releaseEnqueueWait
		check(t, notices(e))
	})
}

// Kamino 136(1), on the release path: a release that did not reach a window
// leaves it held, and the next release reaches it.
func TestAnUnreachedReleaseIsTriedAgain(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, d, a := focusFreeEngine(rec, "g1", "g2")

	runLive(t, e, src, func(send func(keys.Event)) {
		send(keys.Event{Code: a, State: keys.Down})
		d.setUnreachable("g2", true)
		e.SetEnabled(false) // a release that reaches nobody
		time.Sleep(40 * time.Millisecond)
		if got := e.heldTargets(a); !sameSet(got, []WindowID{"g2"}) {
			t.Errorf("A held on %v after a release that reached nobody, want [g2]", got)
		}
		d.setUnreachable("g2", false)
		e.SetFilter(keys.Set{}) // releases again
		time.Sleep(40 * time.Millisecond)
	})
	u := ups(d)
	if len(u) != 2 || !sameSet(u[1], []WindowID{"g2"}) {
		t.Fatalf("Ups to %v, want a second one reaching g2", u)
	}
}

// gatedDeliverer is a fakeDeliverer whose first Deliver waits for open, so a
// Down can be caught after the worker took it off the queue and before it is
// recorded as held.
type gatedDeliverer struct {
	fakeDeliverer
	entered chan struct{}
	open    chan struct{}
	once    atomic.Bool
}

func (g *gatedDeliverer) Deliver(ctx context.Context, req Delivery) ([]WindowID, error) {
	if g.once.CompareAndSwap(false, true) {
		close(g.entered)
		<-g.open
	}
	return g.fakeDeliverer.Deliver(ctx, req)
}

// Kamino 136(2): a Down the worker is delivering right now is not in the
// queue and not yet in held; broadcast going off then must still release it.
func TestADownInFlightIsReleasedWhenBroadcastGoesOff(t *testing.T) {
	rec := &recorder{active: "g1"}
	e, src, _, a := focusFreeEngine(rec, "g1", "g2")
	g := &gatedDeliverer{entered: make(chan struct{}), open: make(chan struct{})}
	e.SetDeliverer(g)

	runLive(t, e, src, func(send func(keys.Event)) {
		src.ch <- keys.Event{Code: a, State: keys.Down}
		select {
		case <-g.entered:
		case <-time.After(time.Second):
			t.Fatal("the Down never reached the deliverer")
		}
		e.SetEnabled(false)
		close(g.open)
		time.Sleep(40 * time.Millisecond)
	})
	u := ups(&g.fakeDeliverer)
	if len(u) != 1 || !sameSet(u[0], []WindowID{"g2"}) {
		t.Fatalf("Ups to %v, want one, to g2", u)
	}
}
