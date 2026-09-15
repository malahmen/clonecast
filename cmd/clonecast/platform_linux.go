//go:build linux

package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/platform/evdev"
	"github.com/malahmen/clonecast/internal/platform/kwin"
	"github.com/malahmen/clonecast/internal/platform/x11"
)

func newLinuxPlatform() (*platform, error) {
	src, err := evdev.Open()
	if err != nil {
		return nil, fmt.Errorf("keyboard capture: %w", err)
	}
	inj, err := evdev.NewInjector(src.Devices()[0])
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("virtual keyboard: %w", err)
	}
	// Give the compositor time to notice the new input device before the
	// first passthrough event, otherwise the first keystrokes vanish.
	time.Sleep(500 * time.Millisecond)

	wm, err := kwin.New()
	if err != nil {
		_ = inj.Close()
		_ = src.Close()
		return nil, fmt.Errorf("kwin: %w", err)
	}
	return &platform{
		src: src,
		inj: inj,
		wm:  wm,
		close: func() {
			_ = wm.Close()
			_ = inj.Close()
			_ = src.Close()
		},
	}, nil
}

// xsendAvailable reports whether the xsend backend can be built at all; it is
// X11-only, so the TUI must not offer it elsewhere.
const xsendAvailable = true

// setupXsend wires the experimental xsend backend (Technique A): XSendEvent to
// each target's X window. spec optionally maps a target's KWin title to the X
// window title to aim at ("KWin Title=X Title,..."); unmapped targets aim at a
// window whose title equals their own KWin title. See REFERENCE.md 4.12/7.9 —
// requires the client in Wine virtual-desktop mode and has an unsolved
// stuck-key caveat, so it is opt-in only.
func setupXsend(wm broadcast.WindowManager, spec string) (broadcast.Deliverer, func(), error) {
	xmap, err := parseTitleMap(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("--xsend: %w", err)
	}
	s, err := x11.New()
	if err != nil {
		return nil, nil, fmt.Errorf("xsend: %w", err)
	}
	tc := newTitleCache(wm)
	titles := func(id broadcast.WindowID) string {
		kt := tc.title(id)
		if xt, ok := xmap[kt]; ok {
			return xt
		}
		return kt
	}
	d := x11.NewDeliverer(s, titles)
	return d, func() { _ = d.Close() }, nil
}

// parseTitleMap parses "Title A=val,Title B=val2" into a map. Keys and values
// are trimmed; empty entries are ignored. A key may contain spaces (window
// titles do); only the first '=' separates key from value.
//
// Only --xsend uses this now: the agent backend's title map is gone, replaced
// by agents that register themselves and are paired by prefix (REFERENCE.md
// 4.17). xsend is experimental and Linux-only, which is why the last of the
// title-map machinery lives here rather than in deliver.go.
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
