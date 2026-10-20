package broadcast

import (
	"testing"

	"github.com/malahmen/clonecast/internal/keys"
)

// reachDeliverer is a focus-free deliverer that reaches the windows in reach.
type reachDeliverer struct {
	fakeDeliverer
	reach map[WindowID]bool
}

func (r *reachDeliverer) Reaches(id WindowID) bool { return r.reach[id] }

func masterEngine(rec *recorder, src *fakeSource, reach ...WindowID) (*Engine, *reachDeliverer) {
	cfg := DefaultConfig()
	cfg.SettleDelay = 0
	cfg.MasterByAgent = true
	e := New(src, rec, rec, cfg)
	d := &reachDeliverer{reach: map[WindowID]bool{}}
	for _, id := range reach {
		d.reach[id] = true
	}
	e.SetDeliverer(d)
	e.SetTargets([]WindowID{"g1", "g2"})
	e.SetEnabled(true)
	return e, d
}

func emits(rec *recorder) int {
	n := 0
	for _, l := range rec.snapshot() {
		if len(l) > 5 && l[:5] == "emit@" {
			n++
		}
	}
	return n
}

// The point of MasterByAgent (REFERENCE.md 4.25): while broadcasting, the
// master's keys, all of them and not just the broadcast ones, go to it
// through the agent, and nothing goes to the compositor, so a game holding
// the X focus gets nothing.
func TestMasterByAgentFeedsTheMasterAndSkipsPassthrough(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, d := masterEngine(rec, src, "g1", "g2")
	a, _ := keys.Parse("a")

	run(t, e, src, keys.Event{Code: a, State: keys.Down}, keys.Event{Code: a, State: keys.Repeat}, keys.Event{Code: a, State: keys.Up})

	if n := emits(rec); n != 0 {
		t.Fatalf("%d passthrough emit(s), want none: %v", n, rec.snapshot())
	}
	calls := d.snapshot()
	if len(calls) != 3 {
		t.Fatalf("%d deliveries, want 3 (down, repeat, up): %+v", len(calls), calls)
	}
	for _, c := range calls {
		if len(c.delivered) != 1 || c.delivered[0] != "g1" {
			t.Errorf("%v went to %v, want only the master g1", c.ev, c.delivered)
		}
	}
}

func TestMasterByAgentStillBroadcastsToTheOthers(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, d := masterEngine(rec, src, "g1", "g2")
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))

	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	calls := d.snapshot()
	if len(calls) != 2 {
		t.Fatalf("%d deliveries, want the master's and the broadcast: %+v", len(calls), calls)
	}
	if got := calls[0].delivered; len(got) != 1 || got[0] != "g1" {
		t.Errorf("first delivery went to %v, want the master g1", got)
	}
	if got := calls[1]; got.origin != "g1" || len(got.delivered) != 1 || got.delivered[0] != "g2" {
		t.Errorf("broadcast: origin %q delivered %v, want origin g1 delivered [g2]", got.origin, got.delivered)
	}
	if n := emits(rec); n != 0 {
		t.Errorf("%d passthrough emit(s), want none", n)
	}
}

// Every case where the master cannot be fed through the agent keeps the old
// path, so the keyboard never goes dead.
func TestMasterByAgentFallsBackToPassthrough(t *testing.T) {
	a, _ := keys.Parse("a")
	cases := []struct {
		name   string
		active WindowID
		reach  []WindowID
	}{
		{"no agent for the master", "g1", []WindowID{"g2"}},
		{"the terminal is focused", "terminal", []WindowID{"g1", "g2"}},
	}
	for _, c := range cases {
		rec := &recorder{active: c.active}
		src := &fakeSource{ch: make(chan keys.Event, 8)}
		e, d := masterEngine(rec, src, c.reach...)
		run(t, e, src, keys.Event{Code: a, State: keys.Down})
		if n := emits(rec); n != 1 {
			t.Errorf("%s: %d passthrough emit(s), want 1", c.name, n)
		}
		if calls := d.snapshot(); len(calls) != 0 {
			t.Errorf("%s: %d deliveries, want none: %+v", c.name, len(calls), calls)
		}
	}
}

// Broadcast off is how the user types normally (chat, Shift combinations): the
// master's keys take passthrough then.
func TestMasterByAgentNotWhileBroadcastIsOff(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, d := masterEngine(rec, src, "g1", "g2")
	e.SetEnabled(false)
	a, _ := keys.Parse("a")

	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	if n := emits(rec); n != 1 {
		t.Errorf("%d passthrough emit(s), want 1", n)
	}
	if calls := d.snapshot(); len(calls) != 0 {
		t.Errorf("%d deliveries, want none: %+v", len(calls), calls)
	}
}

// Broadcast off while W is held on the master: the user's Up then takes
// passthrough and never reaches the game, so turning broadcast off must
// release it there itself.
func TestBroadcastOffReleasesTheAgentMaster(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, d := masterEngine(rec, src, "g1", "g2")
	w, _ := keys.Parse("w")

	run(t, e, src, keys.Event{Code: w, State: keys.Down})
	e.SetEnabled(false)
	run(t, e, src)

	calls := d.snapshot()
	last := calls[len(calls)-1]
	if last.ev.State != keys.Up || len(last.delivered) != 1 || last.delivered[0] != "g1" {
		t.Fatalf("last delivery %v to %v, want W up to the master g1", last.ev, last.delivered)
	}
}

func TestMasterByAgentIsOffByDefault(t *testing.T) {
	if DefaultConfig().MasterByAgent {
		t.Fatal("MasterByAgent on by default")
	}
}

// Holding W on one game and clicking another before letting go: the Up must
// reach the game that got the Down, or that character keeps walking.
func TestMasterByAgentReleasesOnTheOldMaster(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e, d := masterEngine(rec, src, "g1", "g2")
	w, _ := keys.Parse("w")

	run(t, e, src, keys.Event{Code: w, State: keys.Down})
	rec.mu.Lock()
	rec.active = "g2"
	rec.mu.Unlock()
	run(t, e, src, keys.Event{Code: w, State: keys.Up})

	calls := d.snapshot()
	last := calls[len(calls)-1]
	got := map[WindowID]bool{}
	for _, id := range last.delivered {
		got[id] = true
	}
	if last.ev.State != keys.Up || !got["g1"] || !got["g2"] {
		t.Fatalf("last delivery %v to %v, want the Up to both g1 (held it) and g2 (master now)", last.ev, last.delivered)
	}
}
