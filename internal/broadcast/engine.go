// Package broadcast contains the platform independent core of clonecast: it
// takes key events from a Source, decides which ones to broadcast, and drives
// an Injector and a WindowManager to deliver them to the selected targets.
//
// Delivery model (see REFERENCE.md, "Delivery by focus juggling"):
//
//  1. Every event is replayed to the currently focused window first. Because
//     the Source grabs the physical keyboard, nothing reaches that window
//     otherwise, so this step is what makes the keyboard keep working. This
//     step never waits on the backlog of other keys' step 2s — only, when
//     one is actually in flight right now, on that single one. See the
//     paragraphs below for why that distinction is load-bearing.
//  2. If broadcasting is enabled and the key is in the allowlist, the engine
//     then activates each target window in turn, replays the event, and finally
//     restores focus to the original window.
//
// Only Down and Up transitions are broadcast. Auto-repeat events are replayed
// to the focused window only, because juggling focus at repeat rate is both
// slow and pointless: the target applications generate their own repeats from
// the Down they already received.
//
// Passthrough is decoupled from the backlog of broadcast dances, but not
// from whichever one dance is actually in flight right now — that
// distinction matters and was originally missed; see the two paragraphs
// below.
//
// Step 2 costs roughly SettleDelay per target plus WindowManager round-trip
// time — tens of milliseconds per key, easily 100ms+ with several targets.
// Running it inline in the same goroutine that reads Source events would
// mean every keystroke waits for the *previous* keystroke's full
// multi-window dance before it could even reach your own screen. Instead,
// handle() does step 1 immediately and hands step 2 to a single dedicated
// worker goroutine (broadcastLoop) via a bounded queue, so passthrough
// latency no longer depends on how many *queued* dances there are or how
// slow the compositor is in general. Dances still never interleave with
// each other (one worker, one at a time).
//
// That queue alone is not sufficient, though: a dance's whole point is to
// move real input focus away from the origin window and back. For as long
// as one is doing that, the assumption behind step 1 — "the origin window
// has focus, so Emit reaches it" — is false. A naive decoupling emits
// passthrough the instant an event arrives regardless, so a keystroke that
// lands while a dance is between "activate target" and "restore origin" gets
// delivered to whatever window the dance currently has focused, not to the
// window the user is actually looking at. This is what focusMu is for: a
// dance holds it for its *entire* duration (from before it reads the
// origin through the final restore), and passthrough's Emit also takes it,
// just for that one call. The result is passthrough only ever waits when a
// dance is actually mid-flight right now — never for the rest of the queue
// behind it — and when it does wait, it's for a bounded, single dance's
// worth of time, and it's waiting for a correctness reason, not an
// incidental one. Because a dance already holds focusMu for its own
// duration, its own per-target Emit calls don't re-acquire it — only
// passthrough's do.
package broadcast

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/malahmen/clonecast/internal/keys"
)

// Source produces key events from the physical keyboard(s).
type Source interface {
	Events() <-chan keys.Event
	Close() error
}

// Injector replays key events into the system, to whichever window has focus.
type Injector interface {
	Emit(keys.Event) error
	Close() error
}

// WindowID identifies a top level window. It is opaque and backend specific.
type WindowID string

// Window is a top level window as reported by the WindowManager.
type Window struct {
	ID      WindowID
	Title   string
	Class   string // resource class / app id, e.g. "steam" or "org.kde.konsole"
	PID     int
	Focused bool
}

// WindowManager lists windows and moves focus between them.
type WindowManager interface {
	List(ctx context.Context) ([]Window, error)
	Active(ctx context.Context) (WindowID, error)
	Activate(ctx context.Context, id WindowID) error
}

// Notice is a human readable status line emitted by the engine for the UI.
type Notice struct {
	Time time.Time
	Text string
	Err  bool
}

// Config holds the tunables the UI is allowed to change while running.
type Config struct {
	// SettleDelay is how long to wait after activating a window before
	// injecting into it. Too short and the compositor delivers the key to the
	// previous window. Start at ~30ms and tune per machine.
	SettleDelay time.Duration
	// ToggleKey flips broadcasting on and off. It is consumed and never
	// replayed anywhere. Zero disables the hotkey.
	ToggleKey keys.Code
	// OpTimeout bounds each WindowManager call.
	OpTimeout time.Duration
}

// DefaultConfig is a sane starting point.
func DefaultConfig() Config {
	toggle, _ := keys.Parse("SCROLLLOCK")
	return Config{
		SettleDelay: 30 * time.Millisecond,
		ToggleKey:   toggle,
		OpTimeout:   2 * time.Second,
	}
}

// Engine is the broadcast loop. Create with New, drive with Run, and mutate
// targets, filter and enabled state from any goroutine.
type Engine struct {
	src Source
	inj Injector
	cfg Config

	// deliverer performs step 2 (broadcast). Defaults to the focus dance; swap
	// with SetDeliverer for the agent or xsend backends. See REFERENCE.md 7.9.
	deliverer Deliverer

	mu      sync.RWMutex
	targets []WindowID
	filter  keys.Set
	enabled bool

	notices chan Notice

	// focusMu is held by a dance for its entire duration — from before it
	// reads the origin window through the final restore — and by
	// passthrough's Emit call, just for that one call. This is what stops
	// passthrough from firing while a dance has genuinely moved real focus
	// away from origin; see package doc for the failure mode it fixes.
	focusMu sync.Mutex

	// queue hands broadcast-worthy events from Run to broadcastLoop. Sized
	// for a burst of keystrokes typed faster than the worker can dance
	// through them; a full queue drops the newest event rather than ever
	// blocking Run, with a notice so drops are visible instead of silent.
	queue chan keys.Event
}

// New wires an engine. The filter starts empty (nothing broadcast) and
// broadcasting starts disabled, so the tool is safe until the user opts in.
func New(src Source, inj Injector, wm WindowManager, cfg Config) *Engine {
	e := &Engine{
		src:     src,
		inj:     inj,
		cfg:     cfg,
		notices: make(chan Notice, 64),
		queue:   make(chan keys.Event, 16),
	}
	// Default backend: the focus dance, reading SettleDelay live from cfg.
	e.deliverer = newDanceDeliverer(wm, inj, func() Config { return e.cfg })
	return e
}

// SetDeliverer swaps the broadcast delivery backend (see REFERENCE.md 7.9).
// Call it before Run; it is not safe to swap while a delivery may be in
// flight.
func (e *Engine) SetDeliverer(d Deliverer) { e.deliverer = d }

// Notices delivers status lines. The channel is buffered and never blocks the
// engine: if the consumer falls behind, notices are dropped.
func (e *Engine) Notices() <-chan Notice { return e.notices }

// SetTargets replaces the target list. Order is the delivery order.
func (e *Engine) SetTargets(ids []WindowID) {
	e.mu.Lock()
	e.targets = append([]WindowID(nil), ids...)
	e.mu.Unlock()
}

// Targets returns a copy of the current targets.
func (e *Engine) Targets() []WindowID {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return append([]WindowID(nil), e.targets...)
}

// SetFilter replaces the allowlist.
func (e *Engine) SetFilter(s keys.Set) {
	e.mu.Lock()
	e.filter = s
	e.mu.Unlock()
}

// Filter returns the current allowlist.
func (e *Engine) Filter() keys.Set {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.filter
}

// SetEnabled turns broadcasting on or off. Passthrough to the focused window
// keeps working either way.
func (e *Engine) SetEnabled(on bool) {
	e.mu.Lock()
	e.enabled = on
	e.mu.Unlock()
	e.notify(fmt.Sprintf("broadcast %s", onOff(on)), false)
}

// Enabled reports the broadcast state.
func (e *Engine) Enabled() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.enabled
}

// Run consumes events until ctx is cancelled or the source closes. It owns
// passthrough (step 1, always immediate) and hands broadcast dances (step 2)
// to a dedicated worker goroutine it starts here — see package doc for why.
func (e *Engine) Run(ctx context.Context) error {
	go e.broadcastLoop(ctx)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-e.src.Events():
			if !ok {
				return errors.New("key source closed")
			}
			e.handle(ev)
		}
	}
}

func (e *Engine) handle(ev keys.Event) {
	if e.cfg.ToggleKey != 0 && ev.Code == e.cfg.ToggleKey {
		if ev.State == keys.Down {
			e.SetEnabled(!e.Enabled())
		}
		return // consumed, never replayed
	}

	// Step 1: the focused window always gets the key. Waits only if a dance
	// is actively holding real focus away from origin right now (focusMu) —
	// never for the rest of the queue behind it. See package doc.
	if err := e.passthroughEmit(ev); err != nil {
		e.notify(fmt.Sprintf("passthrough %v: %v", ev, err), true)
		return
	}

	e.mu.RLock()
	enabled, filter := e.enabled, e.filter
	e.mu.RUnlock()

	if !enabled || ev.State == keys.Repeat || !filter.Allows(ev.Code) {
		return
	}

	// Step 2: hand off to the worker instead of dancing here. A full queue
	// means keys are arriving faster than the worker can visit targets for
	// them — drop this one rather than block passthrough to catch up.
	select {
	case e.queue <- ev:
	default:
		e.notify(fmt.Sprintf("broadcast queue full, dropped %v", ev), true)
	}
}

// broadcastLoop is the single worker that performs every focus dance, one at
// a time, for as long as ctx is alive. Serialising here (rather than in Run)
// is what keeps two dances from interleaving, and lets Run keep reading new
// keys off Source the whole time a dance is in flight — their passthrough
// may have to wait on focusMu until the dance restores origin, but Run
// itself is never blocked, so it's always ready for whatever comes next.
func (e *Engine) broadcastLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.queue:
			e.broadcast(ctx, ev)
		}
	}
}

func (e *Engine) broadcast(ctx context.Context, ev keys.Event) {
	e.mu.RLock()
	targets := append([]WindowID(nil), e.targets...)
	e.mu.RUnlock()
	if len(targets) == 0 {
		return
	}

	// A focus-moving backend (the dance) must hold focusMu for its entire
	// duration: from before it reads the origin window through the final
	// restore, real focus may not be where passthrough assumes it is (see
	// package doc). A focus-free backend (agent/xsend) never moves focus, so
	// passthrough need never wait on it and we skip the lock. Because a
	// focus-moving delivery holds focusMu here, its own Emit calls (inside the
	// deliverer) don't re-acquire it.
	if e.deliverer.MovesFocus() {
		e.focusMu.Lock()
		defer e.focusMu.Unlock()
	}

	opCtx, cancel := context.WithTimeout(ctx, e.cfg.OpTimeout)
	defer cancel()

	delivered, err := e.deliverer.Deliver(opCtx, ev, targets, e.notify)
	if err != nil {
		return // deliverer already reported the failure via notify
	}
	e.notify(fmt.Sprintf("%v -> %d target(s)", ev, delivered), false)
}

// passthroughEmit is step 1's Emit, guarded by focusMu so it can never fire
// while a dance is actively holding real focus away from origin — see
// package doc and focusMu's doc on Engine.
func (e *Engine) passthroughEmit(ev keys.Event) error {
	e.focusMu.Lock()
	defer e.focusMu.Unlock()
	return e.inj.Emit(ev)
}

func (e *Engine) notify(text string, isErr bool) {
	select {
	case e.notices <- Notice{Time: time.Now(), Text: text, Err: isErr}:
	default:
	}
}

func sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func onOff(b bool) string {
	if b {
		return "ON"
	}
	return "OFF"
}
