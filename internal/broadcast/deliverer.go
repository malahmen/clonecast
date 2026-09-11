package broadcast

import (
	"context"

	"github.com/malahmen/clonecast/internal/keys"
)

// Delivery is one broadcast request: everything a Deliverer needs to place a
// single key event on a set of windows. It is a struct rather than a longer
// parameter list because Targets and Origin are both window identities and
// read identically at a call site, and because this interface has already
// grown a parameter once (Google Go style guide, "function argument lists").
type Delivery struct {
	// Event is the key transition to deliver.
	Event keys.Event
	// Targets are the ticked windows, in list order.
	Targets []WindowID
	// Origin is the focused window (the master). Always one of Targets when
	// the gate is on, and always a window the engine actually resolved.
	Origin WindowID
	// Notify reports per-target failures to the engine's notice stream.
	Notify func(string, bool)
}

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
	// Deliver sends req.Event to every target in req.Targets except
	// req.Origin — the window that had real input focus when the engine
	// picked the event up, i.e. the current "master" (REFERENCE.md 7.10).
	// Origin already received the event through passthrough, so delivering
	// to it again would double the key; every implementation MUST skip it.
	// The engine resolves it once per broadcast (one wm.Active call, ~1ms,
	// REFERENCE.md 7.6) rather than having every backend re-query it, and
	// never calls Deliver at all when it could not be resolved.
	//
	// Per-target failures are reported through req.Notify; the returned count
	// is how many targets actually received the event. A returned error is a
	// whole-delivery failure, not a per-target one.
	//
	// Implementations must be safe for concurrent use only to the extent the
	// engine needs: it calls Deliver from a single worker goroutine.
	Deliver(ctx context.Context, req Delivery) (int, error)

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

func (d *danceDeliverer) Deliver(ctx context.Context, req Delivery) (int, error) {
	cfg := d.settle()
	delivered := 0
	for _, t := range req.Targets {
		if t == req.Origin {
			continue // already received it in step 1 (passthrough)
		}
		if err := d.wm.Activate(ctx, t); err != nil {
			req.Notify("activate "+string(t)+": "+err.Error(), true)
			continue
		}
		sleep(ctx, cfg.SettleDelay)
		if err := d.inj.Emit(req.Event); err != nil {
			req.Notify("emit "+req.Event.String()+" to "+string(t)+": "+err.Error(), true)
			continue
		}
		delivered++
	}

	if err := d.wm.Activate(ctx, req.Origin); err != nil {
		req.Notify("restore focus to "+string(req.Origin)+": "+err.Error(), true)
	}
	return delivered, nil
}
