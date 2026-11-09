package broadcast

import (
	"testing"

	"github.com/malahmen/clonecast/internal/keys"
)

// carryDeliverer is a reachDeliverer that carries only some key codes, as the
// agent carries only the keys it can map to a Win32 virtual key.
type carryDeliverer struct {
	reachDeliverer
	carry map[keys.Code]bool
}

func (c *carryDeliverer) Carries(code keys.Code) bool { return c.carry[code] }

func carryEngine(rec *recorder, src *fakeSource, carry ...keys.Code) (*Engine, *carryDeliverer) {
	cfg := DefaultConfig()
	cfg.MasterByAgent = true
	e := New(src, rec, rec, cfg)
	d := &carryDeliverer{
		reachDeliverer: reachDeliverer{reach: map[WindowID]bool{"g1": true, "g2": true}},
		carry:          map[keys.Code]bool{},
	}
	for _, c := range carry {
		d.carry[c] = true
	}
	e.SetDeliverer(d)
	e.SetTargets([]WindowID{"g1", "g2"})
	e.SetEnabled(true)
	return e, d
}

func toWindow(calls []deliverCall, code keys.Code, id WindowID) int {
	n := 0
	for _, c := range calls {
		if c.ev.Code != code {
			continue
		}
		for _, w := range c.delivered {
			if w == id {
				n++
			}
		}
	}
	return n
}

// With master by agent, a key the agent cannot carry (an unmapped code)
// reaches the master by passthrough - Down, repeat and Up alike - and not
// through its agent, which would drop it (Kamino 185).
func TestMasterByAgentSendsAnUncarriedKeyByPassthrough(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	a, _ := keys.Parse("a")
	pause, _ := keys.Parse("pause")
	e, d := carryEngine(rec, src, a)

	run(t, e, src,
		keys.Event{Code: pause, State: keys.Down},
		keys.Event{Code: pause, State: keys.Repeat},
		keys.Event{Code: pause, State: keys.Up})

	if n := emits(rec); n != 3 {
		t.Errorf("%d passthrough emit(s), want 3 (down, repeat, up): %v", n, rec.snapshot())
	}
	if n := toWindow(d.snapshot(), pause, "g1"); n != 0 {
		t.Errorf("the master's agent got the uncarried key %d time(s), want none", n)
	}
	if e.passthroughHolds(pause) {
		t.Error("passthrough still holds the key after its Up")
	}
}

// A key the agent carries still goes through it, with no passthrough: the
// Carrier check only diverts what the agent cannot post.
func TestMasterByAgentStillSendsACarriedKeyThroughTheAgent(t *testing.T) {
	rec := &recorder{active: "g1"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	a, _ := keys.Parse("a")
	e, d := carryEngine(rec, src, a)

	run(t, e, src, keys.Event{Code: a, State: keys.Down}, keys.Event{Code: a, State: keys.Up})

	if n := emits(rec); n != 0 {
		t.Errorf("%d passthrough emit(s) for a carried key, want none", n)
	}
	if n := toWindow(d.snapshot(), a, "g1"); n != 2 {
		t.Errorf("the master's agent got the carried key %d time(s), want 2", n)
	}
}
