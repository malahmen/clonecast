package agent

import (
	"context"
	"fmt"
	"sync"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/prefix"
)

// pairing decides which registered agent serves which window, so that ticking
// a window in the TUI is the only thing the user has to do (REFERENCE.md 4.17,
// research §C3). It replaces the hand-written title->port map.
//
// The join, in one line: window -> KWin's pid -> /proc/<pid>/environ ->
// WINEPREFIX -> the prefix the agent announced in its hello.
//
// Why the pid is trustworthy here and _NET_WM_PID is not: KWin gets a window's
// pid from the X server through the XRes extension, which derives it from the
// socket's peer credentials, so it is the real *host* pid even when the client
// is a Flatpak-sandboxed Wine process. _NET_WM_PID is whatever the client put
// there — inside a Flatpak that is the in-sandbox pid, which means nothing on
// the host. That difference is the whole reason clonecast used to have to
// match by window caption and therefore needed dark-portal to keep captions
// stable (research I4).
//
// Two things are cached because they cannot change: a window's pid, and hence
// the prefix that pid runs in. Everything else is recomputed whenever the
// agent set changes or the periodic refresh fires.
type pairing struct {
	windows  func(context.Context) ([]broadcast.Window, error)
	prefixOf func(pid int) (string, error)
	notify   func(string, bool)

	mu       sync.Mutex
	resolved map[broadcast.WindowID]resolvedWindow // pid -> prefix, cached per window
	pairs    map[broadcast.WindowID]int64          // window -> agent id
	reported map[int64]string                      // agent id -> what was last logged about it
	listErr  string                                // last window-list error, reported once
}

// resolvedWindow is one window's cached identity. Prefix is "" when the join
// could not be made (no pid, unreadable environ, not a Wine process), which is
// what sends this window to the title fallback.
type resolvedWindow struct {
	PID    int
	Prefix string
	Title  string
}

func newPairing(windows func(context.Context) ([]broadcast.Window, error), prefixOf func(int) (string, error), notify func(string, bool)) *pairing {
	if prefixOf == nil {
		prefixOf = prefix.PrefixOfPID
	}
	if notify == nil {
		notify = func(string, bool) {}
	}
	return &pairing{
		windows:  windows,
		prefixOf: prefixOf,
		notify:   notify,
		resolved: map[broadcast.WindowID]resolvedWindow{},
		pairs:    map[broadcast.WindowID]int64{},
		reported: map[int64]string{},
	}
}

// agentFor returns the id of the agent paired with a window, if any.
func (p *pairing) agentFor(id broadcast.WindowID) (int64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.pairs[id]
	return a, ok
}

// windowFor is the reverse lookup, for the log and the UI.
func (p *pairing) windowFor(agentID int64) (broadcast.WindowID, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for w, a := range p.pairs {
		if a == agentID {
			return w, true
		}
	}
	return "", false
}

// recompute re-pairs every window against the current agent set. It is called
// whenever an agent registers or disconnects and on a slow ticker (a window
// can appear long after its agent did — the agent starts at prefix boot,
// before the game).
//
// A failed window list leaves the previous pairing in place: losing the
// compositor for one poll must not make every target look agent-less.
func (p *pairing) recompute(ctx context.Context, agents []*Agent) {
	wins, err := p.windows(ctx)
	if err != nil {
		p.mu.Lock()
		first := p.listErr != err.Error()
		p.listErr = err.Error()
		p.mu.Unlock()
		if first {
			p.notify("agent pairing: list windows: "+err.Error()+" (keeping the previous pairing)", true)
		}
		return
	}

	p.mu.Lock()
	p.listErr = ""
	info := make(map[broadcast.WindowID]resolvedWindow, len(wins))
	for _, w := range wins {
		info[w.ID] = p.resolve(w)
	}
	// Windows that are gone cannot come back under the same id, so their
	// cached prefix is dead weight.
	for id := range p.resolved {
		if _, ok := info[id]; !ok {
			delete(p.resolved, id)
		}
	}
	p.mu.Unlock()

	pairs := map[broadcast.WindowID]int64{}
	how := map[int64]string{}
	used := map[int64]bool{}

	// Pass 1, the real join: the agent's announced WINEPREFIX against the
	// prefix the window's host pid runs in.
	for _, w := range wins {
		in := info[w.ID]
		if in.Prefix == "" {
			continue
		}
		for _, a := range agents {
			if used[a.ID] || a.Hello.Prefix == "" {
				continue
			}
			if prefix.SamePrefix(a.Hello.Prefix, in.Prefix) {
				pairs[w.ID], used[a.ID] = a.ID, true
				how[a.ID] = fmt.Sprintf("prefix (pid %d -> %s)", in.PID, in.Prefix)
				break
			}
		}
	}

	// Pass 2, the fallback (REFERENCE.md 4.17): match the agent's announced
	// window title against the KWin caption. This is what clonecast did for
	// every target before pid pairing existed, and it is still the only thing
	// available when the pid is missing (no XRes), its environ is unreadable,
	// or the agent's own environment carries no WINEPREFIX. A window whose
	// prefix IS known is never title-matched to an agent from a *different*
	// known prefix — that is a positive mismatch, not missing information.
	for _, w := range wins {
		if _, done := pairs[w.ID]; done || w.Title == "" {
			continue
		}
		in := info[w.ID]
		for _, a := range agents {
			if used[a.ID] || a.Hello.Title != w.Title {
				continue
			}
			if in.Prefix != "" && a.Hello.Prefix != "" && !prefix.SamePrefix(a.Hello.Prefix, in.Prefix) {
				continue
			}
			pairs[w.ID], used[a.ID] = a.ID, true
			how[a.ID] = fmt.Sprintf("window title %q (no prefix to match on)", w.Title)
			break
		}
	}

	p.mu.Lock()
	p.pairs = pairs
	// Report how each agent was paired once, not per key: the method matters
	// when something is wrong and is noise otherwise.
	var lines []string
	for _, a := range agents {
		msg := "unpaired: no window matches " + a.Hello.Describe()
		if h, ok := how[a.ID]; ok {
			msg = "paired by " + h
		}
		if p.reported[a.ID] != msg {
			p.reported[a.ID] = msg
			lines = append(lines, fmt.Sprintf("agent %s: %s", a.Label(), msg))
		}
	}
	for id := range p.reported {
		if !agentPresent(agents, id) {
			delete(p.reported, id)
		}
	}
	p.mu.Unlock()

	for _, l := range lines {
		p.notify(l, false)
	}
}

func agentPresent(agents []*Agent, id int64) bool {
	for _, a := range agents {
		if a.ID == id {
			return true
		}
	}
	return false
}

// resolve returns a window's cached identity, doing the /proc read at most
// once per (window, pid). Callers hold p.mu.
func (p *pairing) resolve(w broadcast.Window) resolvedWindow {
	if got, ok := p.resolved[w.ID]; ok && got.PID == w.PID {
		got.Title = w.Title
		p.resolved[w.ID] = got
		return got
	}
	out := resolvedWindow{PID: w.PID, Title: w.Title}
	if w.PID > 0 {
		if pfx, err := p.prefixOf(w.PID); err == nil {
			out.Prefix = pfx
		}
	}
	p.resolved[w.ID] = out
	return out
}
