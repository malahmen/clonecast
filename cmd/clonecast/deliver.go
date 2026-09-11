package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/platform/agent"
)

// Delivery backend names, in the order the TUI cycles through them.
const (
	deliverAgent = "agent" // in-bottle PostMessage agent — the reliable path (REFERENCE.md 4.12/7.9)
	deliverDance = "dance" // legacy focus juggling — last resort
	deliverXsend = "xsend" // experimental X11 XSendEvent
)

// backends owns every delivery backend available for this run and which one
// the engine is currently using. Each backend is built at most once and kept
// (agent connections survive a round trip through another backend), so the
// TUI can switch delivery at runtime instead of the user restarting clonecast
// with a different --deliver. See REFERENCE.md 7.9.
type backends struct {
	engine    *broadcast.Engine
	wm        broadcast.WindowManager
	agentSpec string
	xsendSpec string

	mu    sync.Mutex
	cur   string
	made  map[string]broadcast.Deliverer
	close map[string]func()
}

func newBackends(engine *broadcast.Engine, wm broadcast.WindowManager, agentSpec, xsendSpec string) *backends {
	return &backends{
		engine:    engine,
		wm:        wm,
		agentSpec: agentSpec,
		xsendSpec: xsendSpec,
		made:      map[string]broadcast.Deliverer{},
		close:     map[string]func(){},
	}
}

// Names lists the backends this run can actually use, so the TUI never cycles
// into one that is guaranteed to fail: agent needs --agent endpoints, and
// xsend only exists on Linux.
func (b *backends) Names() []string {
	names := []string{deliverDance}
	if b.agentSpec != "" {
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

// Close releases every backend that was built.
func (b *backends) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, cl := range b.close {
		cl()
		delete(b.close, name)
		delete(b.made, name)
	}
}

// newAgent wires the agent backend: targets are matched to an in-bottle agent
// endpoint by window title, per the --agent map.
func (b *backends) newAgent() (broadcast.Deliverer, func(), error) {
	m, err := parseTitleMap(b.agentSpec)
	if err != nil {
		return nil, nil, fmt.Errorf("--agent: %w", err)
	}
	if len(m) == 0 {
		return nil, nil, errNoAgentEndpoints
	}
	// Normalise bare ports to a loopback endpoint.
	for k, v := range m {
		if !strings.Contains(v, ":") {
			m[k] = "127.0.0.1:" + v
		}
	}
	tc := newTitleCache(b.wm)
	d := agent.New(func(id broadcast.WindowID) string { return m[tc.title(id)] })
	return d, func() { _ = d.Close() }, nil
}

// errNoAgentEndpoints is what a bare `clonecast` hits now that agent is the
// default backend (REFERENCE.md 4.12/7.9): say what to pass and what the
// fallback is, rather than starting up and silently broadcasting nowhere.
var errNoAgentEndpoints = fmt.Errorf(`the "agent" delivery backend (the default) needs --agent "Title=host:port,...", one entry per target window,
    pointing at the clonecast-agent.exe running inside that target's Wine prefix, e.g.
        clonecast --agent "Malahmen=48900,Marx=48901"
    Run one agent per prefix first (see README "Usage" and REFERENCE.md 4.13-4.14), or fall back to the legacy focus dance with
        clonecast --deliver dance
    (the dance moves real focus per key: slow, and rejected for multiboxing — see REFERENCE.md 4.11/4.12)`)

// parseTitleMap parses "Title A=val,Title B=val2" into a map. Keys and values
// are trimmed; empty entries are ignored. A key may contain spaces (window
// titles do); only the first '=' separates key from value.
func parseTitleMap(spec string) (map[string]string, error) {
	m := map[string]string{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.IndexByte(part, '=')
		if eq < 0 {
			return nil, fmt.Errorf("entry %q has no '='", part)
		}
		key := strings.TrimSpace(part[:eq])
		val := strings.TrimSpace(part[eq+1:])
		if key == "" || val == "" {
			return nil, fmt.Errorf("entry %q has an empty key or value", part)
		}
		m[key] = val
	}
	return m, nil
}

// titleCache resolves a WindowID to its current title via the WindowManager,
// cached briefly so per-key delivery doesn't do a compositor round-trip each
// time. Refreshed on a miss or when older than ttl.
type titleCache struct {
	wm  broadcast.WindowManager
	ttl time.Duration

	mu sync.Mutex
	at time.Time
	m  map[broadcast.WindowID]string
}

func newTitleCache(wm broadcast.WindowManager) *titleCache {
	return &titleCache{wm: wm, ttl: 2 * time.Second}
}

func (t *titleCache) title(id broadcast.WindowID) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.m == nil || time.Since(t.at) > t.ttl {
		t.refresh()
	}
	if s, ok := t.m[id]; ok {
		return s
	}
	t.refresh() // a miss may just be a stale cache
	return t.m[id]
}

func (t *titleCache) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	wins, err := t.wm.List(ctx)
	if err != nil {
		return
	}
	m := make(map[broadcast.WindowID]string, len(wins))
	for _, w := range wins {
		m[w.ID] = w.Title
	}
	t.m = m
	t.at = time.Now()
}
