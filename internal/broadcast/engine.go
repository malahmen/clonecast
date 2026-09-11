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
//     resolves the origin — the window that has focus right now, i.e. the
//     current master (REFERENCE.md 7.10) — and hands the event, the ticked
//     targets and that origin to the selected Deliverer, which delivers to
//     every target except the origin (it already got the key in step 1).
//     With the built-in dance backend "delivering" means activating each
//     target in turn, replaying the event, and restoring focus to the origin;
//     the focus-free backends (agent, xsend) never move focus at all.
//
// Because the origin is resolved per broadcast, the master is dynamic: tick
// every client and whichever one you are looking at is the one that plays the
// keys live, with the others mirroring. Config.GateOrigin (on by default)
// additionally requires the focused window to be one of the ticked targets,
// so typing in the terminal that runs clonecast does not drive every client.
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
	// GateOrigin restricts broadcasting to the case where the focused window
	// (the master, see REFERENCE.md 7.10) is itself one of the ticked
	// targets. On by default: without it, typing in the terminal that runs
	// clonecast with broadcasting enabled drives every client. Turning it off
	// restores the "broadcast to every ticked target, whatever has focus"
	// behaviour, which is occasionally wanted (e.g. driving clients from a
	// window that is deliberately not a target).
	GateOrigin bool
}

// DefaultConfig is a sane starting point.
func DefaultConfig() Config {
	toggle, _ := keys.Parse("SCROLLLOCK")
	return Config{
		SettleDelay: 30 * time.Millisecond,
		ToggleKey:   toggle,
		OpTimeout:   2 * time.Second,
		GateOrigin:  true,
	}
}

// Engine is the broadcast loop. Create with New, drive with Run, and mutate
// targets, filter and enabled state from any goroutine.
type Engine struct {
	src Source
	inj Injector
	// wm resolves the origin (focused) window once per broadcast, so that
	// every deliverer can skip it instead of re-querying — see
	// Deliverer.Deliver and REFERENCE.md 7.10.
	wm WindowManager

	// deliverer performs step 2 (broadcast). Defaults to the focus dance; swap
	// with SetDeliverer for the agent or xsend backends. See REFERENCE.md 7.9.
	// Guarded by delivMu so the UI can switch backends while the engine runs.
	// A plain Mutex: it covers a single field read once per broadcast key, far
	// below the contention an RWMutex would need to justify itself.
	delivMu   sync.Mutex
	deliverer Deliverer

	mu      sync.RWMutex
	cfg     Config // live-tunable from the UI, hence under mu
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

	// gateBlocked / activeBroken keep the gate and the wm.Active failure path
	// from writing one notice per keystroke. Only broadcastLoop touches them
	// (single worker goroutine), so they need no lock.
	gateBlocked  WindowID
	activeBroken bool
}

// New wires an engine. The filter starts empty (nothing broadcast) and
// broadcasting starts disabled, so the tool is safe until the user opts in.
func New(src Source, inj Injector, wm WindowManager, cfg Config) *Engine {
	e := &Engine{
		src:     src,
		inj:     inj,
		wm:      wm,
		cfg:     cfg,
		notices: make(chan Notice, 64),
		queue:   make(chan keys.Event, 16),
	}
	// Built-in backend: the focus dance, reading SettleDelay live from cfg.
	// cmd/clonecast swaps in the agent backend for real runs (REFERENCE.md
	// 4.12/7.9); the dance stays as the last-resort fallback.
	e.deliverer = newDanceDeliverer(wm, inj, e.Config)
	return e
}

// SetDeliverer swaps the broadcast delivery backend (see REFERENCE.md 7.9).
// Safe to call while the engine runs — a delivery already in flight finishes
// with the old backend and the next one uses the new backend — so the UI can
// offer the backend as a runtime setting. The caller keeps ownership of both
// deliverers (the engine closes neither).
func (e *Engine) SetDeliverer(d Deliverer) {
	e.delivMu.Lock()
	e.deliverer = d
	e.delivMu.Unlock()
}

// DanceDeliverer returns a focus-dance backend bound to this engine's
// WindowManager, Injector and live config — the same one New installs. It is
// exported so a UI that switched to the agent or xsend backend can switch
// back to the last-resort dance without rebuilding the engine.
func (e *Engine) DanceDeliverer() Deliverer { return newDanceDeliverer(e.wm, e.inj, e.Config) }

// Config returns the live configuration.
func (e *Engine) Config() Config {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

// SetSettleDelay changes how long delivery waits after activating a target
// before injecting (dance backend only). Takes effect on the next delivery.
func (e *Engine) SetSettleDelay(d time.Duration) {
	if d < 0 {
		d = 0
	}
	e.mu.Lock()
	e.cfg.SettleDelay = d
	e.mu.Unlock()
	e.notify("settle delay "+d.String(), false)
}

// SetToggleKey changes the hotkey that flips broadcasting on and off. Zero
// disables the hotkey. It is consumed and never replayed anywhere.
func (e *Engine) SetToggleKey(c keys.Code) {
	e.mu.Lock()
	e.cfg.ToggleKey = c
	e.mu.Unlock()
	if c == 0 {
		e.notify("toggle hotkey disabled", false)
		return
	}
	e.notify("toggle hotkey "+keys.Name(c), false)
}

// SetGateOrigin turns the origin gate on or off (see Config.GateOrigin).
func (e *Engine) SetGateOrigin(on bool) {
	e.mu.Lock()
	e.cfg.GateOrigin = on
	e.mu.Unlock()
	e.notify("origin gate "+onOff(on), false)
}

// GateOrigin reports whether broadcasting is gated on the focused window
// being a ticked target.
func (e *Engine) GateOrigin() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg.GateOrigin
}

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
	toggle := e.Config().ToggleKey
	if toggle != 0 && ev.Code == toggle {
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
	cfg := e.cfg
	e.mu.RUnlock()
	if len(targets) == 0 {
		return
	}

	e.delivMu.Lock()
	d := e.deliverer
	e.delivMu.Unlock()

	// A focus-moving backend (the dance) must hold focusMu for its entire
	// duration: from before it reads the origin window through the final
	// restore, real focus may not be where passthrough assumes it is (see
	// package doc). A focus-free backend (agent/xsend) never moves focus, so
	// passthrough need never wait on it and we skip the lock. Because a
	// focus-moving delivery holds focusMu here, its own Emit calls (inside the
	// deliverer) don't re-acquire it. Note the lock is taken *before* the
	// origin lookup below, so "origin" is read while focus is still the
	// user's, not a previous dance's.
	if d.MovesFocus() {
		e.focusMu.Lock()
		defer e.focusMu.Unlock()
	}

	opCtx, cancel := context.WithTimeout(ctx, cfg.OpTimeout)
	defer cancel()

	origin, ok := e.origin(opCtx, targets, cfg)
	if !ok {
		return
	}

	delivered, err := d.Deliver(opCtx, Delivery{Event: ev, Targets: targets, Origin: origin, Notify: e.notify})
	if err != nil {
		return // deliverer already reported the failure via notify
	}
	e.notify(fmt.Sprintf("%v -> %d target(s)", ev, delivered), false)
}

// origin resolves the master window for this broadcast and applies the origin
// gate. ok=false means "do not broadcast this event".
//
// wm.Active failing (compositor hiccup, KWin script timeout) is a fail-closed
// case, whatever the gate says: skip the event and report it. Broadcasting
// without an origin would deliver to a window that already got the key from
// passthrough, i.e. double every keystroke on the client being played — the
// precise bug this whole mechanism exists to prevent. Suppressing costs one
// keystroke's broadcast and is recoverable; a doubled key in a game is not.
// This is fail-safe defaults (Saltzer & Schroeder; OWASP "fail securely")
// applied to a tool that types into other people's applications. The notice
// above is what keeps "fail closed" from meaning "fail silently".
func (e *Engine) origin(ctx context.Context, targets []WindowID, cfg Config) (WindowID, bool) {
	origin, err := e.wm.Active(ctx)
	if err != nil {
		if !e.activeBroken { // once per run of failures, not once per key
			e.activeBroken = true
			e.notify("active window: "+err.Error()+" — not broadcasting until the focused window is known again", true)
		}
		return "", false
	}
	e.activeBroken = false

	if cfg.GateOrigin && !contains(targets, origin) {
		if e.gateBlocked != origin { // once per focused window, not once per key
			e.gateBlocked = origin
			e.notify("origin gate: "+string(origin)+" is not a target, not broadcasting", false)
		}
		return "", false
	}
	e.gateBlocked = ""
	return origin, true
}

func contains(ids []WindowID, id WindowID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
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
