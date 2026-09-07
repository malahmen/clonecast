// Package broadcast contains the platform independent core of clonecast: it
// takes key events from a Source, decides which ones to broadcast, and drives
// an Injector and a WindowManager to deliver them to the selected targets.
//
// Delivery model (see REFERENCE.md, "Delivery by focus juggling"):
//
//  1. Every event is replayed to the currently focused window first. Because
//     the Source grabs the physical keyboard, nothing reaches that window
//     otherwise, so this step is what makes the keyboard keep working.
//  2. If broadcasting is enabled and the key is in the allowlist, the engine
//     then activates each target window in turn, replays the event, and finally
//     restores focus to the original window.
//
// Only Down and Up transitions are broadcast. Auto-repeat events are replayed
// to the focused window only, because juggling focus at repeat rate is both
// slow and pointless: the target applications generate their own repeats from
// the Down they already received.
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
	wm  WindowManager
	cfg Config

	mu      sync.RWMutex
	targets []WindowID
	filter  keys.Set
	enabled bool

	notices chan Notice
}

// New wires an engine. The filter starts empty (nothing broadcast) and
// broadcasting starts disabled, so the tool is safe until the user opts in.
func New(src Source, inj Injector, wm WindowManager, cfg Config) *Engine {
	return &Engine{
		src:     src,
		inj:     inj,
		wm:      wm,
		cfg:     cfg,
		notices: make(chan Notice, 64),
	}
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

// Run consumes events until ctx is cancelled or the source closes. It is the
// only goroutine that talks to the Injector and WindowManager, which is what
// guarantees two key events never interleave their focus dances.
func (e *Engine) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-e.src.Events():
			if !ok {
				return errors.New("key source closed")
			}
			e.handle(ctx, ev)
		}
	}
}

func (e *Engine) handle(ctx context.Context, ev keys.Event) {
	if e.cfg.ToggleKey != 0 && ev.Code == e.cfg.ToggleKey {
		if ev.State == keys.Down {
			e.SetEnabled(!e.Enabled())
		}
		return // consumed, never replayed
	}

	// Step 1: the focused window always gets the key.
	if err := e.inj.Emit(ev); err != nil {
		e.notify(fmt.Sprintf("passthrough %v: %v", ev, err), true)
		return
	}

	e.mu.RLock()
	enabled, filter, targets := e.enabled, e.filter, append([]WindowID(nil), e.targets...)
	e.mu.RUnlock()

	if !enabled || ev.State == keys.Repeat || !filter.Allows(ev.Code) || len(targets) == 0 {
		return
	}

	// Step 2: focus dance.
	opCtx, cancel := context.WithTimeout(ctx, e.cfg.OpTimeout)
	defer cancel()

	origin, err := e.wm.Active(opCtx)
	if err != nil {
		e.notify(fmt.Sprintf("active window: %v", err), true)
		return
	}

	delivered := 0
	for _, t := range targets {
		if t == origin {
			continue // already received it in step 1
		}
		if err := e.wm.Activate(opCtx, t); err != nil {
			e.notify(fmt.Sprintf("activate %s: %v", t, err), true)
			continue
		}
		sleep(opCtx, e.cfg.SettleDelay)
		if err := e.inj.Emit(ev); err != nil {
			e.notify(fmt.Sprintf("emit %v to %s: %v", ev, t, err), true)
			continue
		}
		delivered++
	}

	if delivered > 0 || len(targets) > 0 {
		if err := e.wm.Activate(opCtx, origin); err != nil {
			e.notify(fmt.Sprintf("restore focus to %s: %v", origin, err), true)
		}
	}
	e.notify(fmt.Sprintf("%v -> %d target(s)", ev, delivered), false)
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
