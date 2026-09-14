package main

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/platform/agent"
	"github.com/malahmen/clonecast/internal/tui"
)

// Delivery backend names, in the order the TUI cycles through them.
const (
	deliverAgent = "agent" // in-bottle PostMessage agent — the reliable path (REFERENCE.md 4.12/7.9)
	deliverDance = "dance" // legacy focus juggling — last resort
	deliverXsend = "xsend" // experimental X11 XSendEvent
)

// backends owns every delivery backend available for this run and which one
// the engine is currently using. Each backend is built at most once and kept
// (the agent registry and its connections survive a round trip through another
// backend), so the TUI can switch delivery at runtime instead of the user
// restarting clonecast with a different --deliver. See REFERENCE.md 7.9.
type backends struct {
	engine    *broadcast.Engine
	wm        broadcast.WindowManager
	agents    *agent.Registry
	xsendSpec string

	mu    sync.Mutex
	cur   string
	made  map[string]broadcast.Deliverer
	close map[string]func()
}

func newBackends(engine *broadcast.Engine, wm broadcast.WindowManager, agents *agent.Registry, xsendSpec string) *backends {
	return &backends{
		engine:    engine,
		wm:        wm,
		agents:    agents,
		xsendSpec: xsendSpec,
		made:      map[string]broadcast.Deliverer{},
		close:     map[string]func(){},
	}
}

// Names lists the backends this run can actually use, so the TUI never cycles
// into one that is guaranteed to fail: the agent backend needs at least one
// agent to have registered (REFERENCE.md 4.17 — there is nothing to configure
// any more, so "is it usable?" is simply "did an agent show up?"), and xsend
// only exists on Linux. The answer changes while clonecast runs, as prefixes
// boot and shut down.
func (b *backends) Names() []string {
	names := []string{deliverDance}
	if b.agents != nil && b.agents.Count() > 0 {
		names = append(names, deliverAgent)
	}
	if xsendAvailable {
		names = append(names, deliverXsend)
	}
	sort.Strings(names)
	return names
}

// Current is the backend the engine is delivering with.
func (b *backends) Current() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cur
}

// Use switches the engine to the named backend, building it on first use.
// Safe while the engine runs (Engine.SetDeliverer is).
func (b *backends) Use(name string) error {
	if name == deliverAgent && b.agents != nil && b.agents.Count() == 0 {
		return errNoAgentsRegistered(b.agents.Addr())
	}
	return b.use(name)
}

// Start selects the backend clonecast boots with. It differs from Use in one
// place: picking the agent backend with nothing registered yet is a warning,
// not a refusal. Registration is asynchronous — an agent may be seconds away
// from connecting (or the game may not be launched yet) — so refusing to start
// would mean quitting over a condition that fixes itself. The returned string
// is the warning to show; "" means all is well.
func (b *backends) Start(name string) (string, error) {
	warn := ""
	if name == deliverAgent && b.agents != nil && b.agents.Count() == 0 {
		warn = errNoAgentsRegistered(b.agents.Addr()).Error()
	}
	return warn, b.use(name)
}

func (b *backends) use(name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if name == b.cur {
		return nil
	}
	d, ok := b.made[name]
	if !ok {
		var (
			cl  func()
			err error
		)
		switch name {
		case deliverDance:
			d, cl, err = b.engine.DanceDeliverer(), func() {}, nil
		case deliverAgent:
			d, cl, err = b.newAgent()
		case deliverXsend:
			d, cl, err = setupXsend(b.wm, b.xsendSpec)
		default:
			err = fmt.Errorf("unknown delivery backend %q (want %s, %s, or %s)", name, deliverAgent, deliverDance, deliverXsend)
		}
		if err != nil {
			return err
		}
		b.made[name], b.close[name] = d, cl
	}
	b.engine.SetDeliverer(d)
	b.cur = name
	return nil
}

// Close releases every backend that was built. The agent registry is not one
// of them: it belongs to the run, not to the backend, so agents stay connected
// while the user tries another backend.
func (b *backends) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, cl := range b.close {
		cl()
		delete(b.close, name)
		delete(b.made, name)
	}
}

// newAgent wires the agent backend to the registry. There is nothing to
// configure: targets are paired to registered agents by prefix (REFERENCE.md
// 4.17), so ticking a window in the TUI is the whole of target selection.
func (b *backends) newAgent() (broadcast.Deliverer, func(), error) {
	if b.agents == nil {
		return nil, nil, errors.New("this build has no agent registry")
	}
	d := agent.New(b.agents)
	return d, func() { _ = d.Close() }, nil
}

// errNoAgentsRegistered is what a bare `clonecast` hits when no prefix has an
// agent yet. It replaces the old "--agent is empty" error: the flag is gone,
// so the actionable advice is now about installing and starting agents.
func errNoAgentsRegistered(addr string) error {
	return fmt.Errorf(`the "agent" delivery backend (the default) has no agents: nothing has connected to %s.
    Each game's Wine prefix runs its own clonecast-agent.exe, which dials clonecast and registers itself:
        make agent                                    # build it
        clonecast agent list                          # prefixes of running Wine processes
        clonecast agent install --prefix <prefix>     # autostart it on that prefix's next boot
    Then restart the game (the agent starts with the prefix) and it appears in the Targets list with a %s marker.
    Until then, fall back to the legacy focus dance with
        clonecast --deliver dance
    (the dance moves real focus per key: slow, and rejected for multiboxing — see REFERENCE.md 4.11/4.12)`, addr, tui.AgentMark)
}
