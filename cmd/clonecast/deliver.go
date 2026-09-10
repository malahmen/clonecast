package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/platform/agent"
)

// selectDeliverer applies the --deliver choice to engine, returning a cleanup
// func. "dance" (default) leaves the engine's built-in focus dance in place.
// "agent" and "xsend" swap in the corresponding backend (see REFERENCE.md
// 7.9). xsend is platform-specific (setupXsend lives in the platform files).
func selectDeliverer(engine *broadcast.Engine, wm broadcast.WindowManager, deliver, agentSpec, xsendSpec string) (func(), error) {
	switch deliver {
	case "", "dance":
		return func() {}, nil
	case "agent":
		return setupAgent(engine, wm, agentSpec)
	case "xsend":
		return setupXsend(engine, wm, xsendSpec)
	default:
		return nil, fmt.Errorf("unknown --deliver %q (want dance, agent, or xsend)", deliver)
	}
}

// setupAgent wires the agent backend: targets are matched to an in-bottle
// agent endpoint by window title, per the --agent map.
func setupAgent(engine *broadcast.Engine, wm broadcast.WindowManager, spec string) (func(), error) {
	m, err := parseTitleMap(spec)
	if err != nil {
		return nil, fmt.Errorf("--agent: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("--deliver agent needs --agent \"Title=host:port,...\"")
	}
	// Normalise bare ports to a loopback endpoint.
	for k, v := range m {
		if !strings.Contains(v, ":") {
			m[k] = "127.0.0.1:" + v
		}
	}
	tc := newTitleCache(wm)
	d := agent.New(func(id broadcast.WindowID) string { return m[tc.title(id)] })
	engine.SetDeliverer(d)
	return func() { _ = d.Close() }, nil
}

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
