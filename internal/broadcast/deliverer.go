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
	// Deliver sends ev to every target except origin. origin is the window
	// that had real input focus when the engine picked this event up — the
	// current "master" (REFERENCE.md 7.10) — and it has already received ev
	// through passthrough, so delivering to it again would double the key.
	// The engine resolves it once per broadcast (one wm.Active call, ~1ms,
	// REFERENCE.md 7.6) and hands it down rather than every backend
	// re-querying it; every implementation MUST skip it.
	//
	// origin is the empty WindowID when the engine could not determine the
	// focused window and the origin gate is off (see Engine.broadcast): it
	// matches no target, so delivery falls back to "everything ticked".
	//
	// Per-target failures are reported through notify; the returned count is
	// how many targets actually received ev. A returned error is a whole-
	// delivery failure (e.g. couldn't determine origin), not a per-target one.
	Deliver(ctx context.Context, ev keys.Event, targets []WindowID, origin WindowID, notify func(string, bool)) (int, error)

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
// focus to the origin the engine resolved. It is no longer the default
// (REFERENCE.md 4.12/7.9 call it rejected for real multiboxing); it is the
// last-resort fallback for targets that can take neither the agent nor the
// xsend backend. It moves focus, so the engine serialises it against
// passthrough.
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

func (d *danceDeliverer) Deliver(ctx context.Context, ev keys.Event, targets []WindowID, origin WindowID, notify func(string, bool)) (int, error) {
	// The dance needs a real origin even when the engine could not resolve
	// one (origin == "", which only happens with the gate off): unlike the
	// focus-free backends it has to restore focus at the end, and a dance
	// that activates targets and never restores would strand the user on the
	// last target. So re-query here, and fail the whole delivery if that
	// fails too — exactly the behaviour this backend had before origin was
	// hoisted into the interface.
	if origin == "" {
		var err error
		if origin, err = d.wm.Active(ctx); err != nil {
			notify("active window: "+err.Error(), true)
			return 0, err
		}
	}

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
