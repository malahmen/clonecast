package broadcast

import (
	"context"

	"github.com/malahmen/clonecast/internal/keys"
)

// Deliverer is the pluggable step-2 mechanism: given a broadcast-worthy event
// and the current target set, it delivers the event to each target. It is what
// makes clonecast's delivery method swappable (see REFERENCE.md 7.9) — the
// focus dance is one implementation; an in-bottle PostMessage agent
// (Technique B) and X11 XSendEvent (Technique A) are others.
//
// Implementations fall into two kinds, distinguished by MovesFocus:
//
//   - Focus-moving (the dance): it activates each target, so while it runs the
//     real input focus is NOT on the origin window. The engine must serialise
//     such a delivery against passthrough (holding focusMu for its whole
//     duration) or a keystroke typed mid-delivery lands on the wrong window.
//   - Focus-free (agent, xsend): it delivers to each target without ever
//     moving real focus. Passthrough can run concurrently and never has to
//     wait, so the engine skips the focus lock entirely.
type Deliverer interface {
	// Deliver sends ev to each target except origin — origin already received
	// ev via passthrough (step 1), so every implementation skips it; the
	// engine resolves origin once per broadcast (via WindowManager.Active)
	// and passes it in, rather than each backend querying it separately. This
	// is what makes the "master" the currently focused window rather than a
	// fixed one (REFERENCE.md 4.15/7.10): whichever ticked target you focus
	// becomes origin on the very next broadcast key, with no re-ticking.
	// Per-target failures are reported through notify; the returned count is
	// how many targets actually received ev. A returned error is a whole-
	// delivery failure, not a per-target one.
	Deliver(ctx context.Context, ev keys.Event, origin WindowID, targets []WindowID, notify func(string, bool)) (int, error)

	// MovesFocus reports whether Deliver moves real OS input focus. When true
	// the engine runs Deliver under its focus lock, blocking passthrough for
	// the duration; when false, passthrough is never blocked by delivery.
	MovesFocus() bool

	// Close releases any resources (connections, sockets). The Source/Injector
	// the engine owns are closed by their owners, not here.
	Close() error
}

// danceDeliverer is the original focus-juggling delivery (REFERENCE.md 4.2):
// for each target activate it, wait SettleDelay, inject, and finally restore
// focus to origin (passed in by the engine — see Deliverer's doc). It is the
// last-resort fallback for targets that can take neither the agent nor xsend
// backend. It moves focus, so the engine serialises it against passthrough.
type danceDeliverer struct {
	wm     WindowManager
	inj    Injector
	settle func() Config // returns current config so SettleDelay stays live-tunable
}

func newDanceDeliverer(wm WindowManager, inj Injector, cfg func() Config) *danceDeliverer {
	return &danceDeliverer{wm: wm, inj: inj, settle: cfg}
}

func (d *danceDeliverer) MovesFocus() bool { return true }
func (d *danceDeliverer) Close() error     { return nil }

func (d *danceDeliverer) Deliver(ctx context.Context, ev keys.Event, origin WindowID, targets []WindowID, notify func(string, bool)) (int, error) {
	cfg := d.settle()
	delivered := 0
	for _, t := range targets {
		if t == origin {
			continue // already received it in step 1 (passthrough)
		}
		if err := d.wm.Activate(ctx, t); err != nil {
			notify("activate "+string(t)+": "+err.Error(), true)
			continue
		}
		sleep(ctx, cfg.SettleDelay)
		if err := d.inj.Emit(ev); err != nil {
			notify("emit "+ev.String()+" to "+string(t)+": "+err.Error(), true)
			continue
		}
		delivered++
	}

	if err := d.wm.Activate(ctx, origin); err != nil {
		notify("restore focus to "+string(origin)+": "+err.Error(), true)
	}
	return delivered, nil
}
