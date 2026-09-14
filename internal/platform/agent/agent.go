// Package agent is the Linux side of the "agent" delivery backend: the
// registry that in-bottle clonecast-agents connect to, the window<->agent
// pairing, and the broadcast.Deliverer that writes key frames down each
// agent's connection (REFERENCE.md 4.13/4.17). The agent PostMessages them to
// its game window, so nothing ever moves focus and the engine runs this
// backend without the focus lock. It is the reliable default for Wine/Proton
// targets.
//
// The connection is inbound: clonecast listens once (Registry) and agents dial
// in and announce their prefix. Before REFERENCE.md 4.17 it was the other way
// round — clonecast dialled a per-instance port from a hand-written
// --agent "Title=port" map — which meant specifying targets twice and made
// pairing depend on window captions that only dark-portal kept stable.
package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
)

// Deliverer fans key events out to the registered in-bottle agents.
type Deliverer struct {
	reg *Registry

	mu     sync.Mutex
	warned map[broadcast.WindowID]bool // targets already warned about (no agent)
}

// New builds an agent Deliverer over a registry. The registry owns the
// connections and the pairing; the deliverer only asks it "who serves this
// window?" per broadcast key.
func New(reg *Registry) *Deliverer {
	return &Deliverer{reg: reg, warned: map[broadcast.WindowID]bool{}}
}

func (d *Deliverer) MovesFocus() bool { return false }

// Close does not close the registry: the registry outlives any one backend
// (the TUI can switch to the dance and back, and agents must stay connected
// across that), so cmd/clonecast owns its lifetime.
func (d *Deliverer) Close() error { return nil }

// Deliver sends ev to every ticked target except origin. Skipping origin is
// what makes the master dynamic (REFERENCE.md 7.10): the focused window has
// already had the key replayed into it by passthrough, and posting it again
// here would make that client see every broadcast key twice.
func (d *Deliverer) Deliver(_ context.Context, req broadcast.Delivery) (int, error) {
	frame := agentwire.Frame{Code: uint16(req.Event.Code), Down: req.Event.State != keys.Up}
	delivered := 0
	for _, t := range req.Targets {
		if t == req.Origin {
			continue // the master: passthrough already delivered the key there
		}
		a := d.reg.agentFor(t)
		if a == nil {
			d.warnOnce(t, req.Notify)
			continue
		}
		d.clearWarning(t)
		if err := a.send(frame); err != nil {
			// A broken connection means the agent (or its whole prefix) is
			// gone. Drop it so it disappears from the registry and the
			// Targets list; it re-registers by itself when it comes back.
			d.reg.drop(a)
			req.Notify(fmt.Sprintf("agent %s: %v (dropped; it will re-register)", a.Label(), err), true)
			continue
		}
		delivered++
	}
	return delivered, nil
}

// warnOnce reports a ticked target with no registered agent a single time, so
// a window that will never have one (a browser, the terminal) doesn't spam the
// log per key. The TUI says the same thing in place, on the row itself.
func (d *Deliverer) warnOnce(t broadcast.WindowID, notify func(string, bool)) {
	d.mu.Lock()
	first := !d.warned[t]
	d.warned[t] = true
	d.mu.Unlock()
	if first {
		notify("agent: no agent has registered for target "+string(t)+
			" (install one with `clonecast agent install --prefix <path>`); ignoring it", true)
	}
}

// clearWarning re-arms the warning for a target that now has an agent, so the
// next time it loses one it is reported again.
func (d *Deliverer) clearWarning(t broadcast.WindowID) {
	d.mu.Lock()
	delete(d.warned, t)
	d.mu.Unlock()
}
