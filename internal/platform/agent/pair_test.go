package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/prefix"
)

// agentsFor builds registry-shaped agents without sockets: pairing never
// touches the connection.
func agentsFor(hellos ...agentwire.Hello) []*Agent {
	out := make([]*Agent, len(hellos))
	for i, h := range hellos {
		out[i] = &Agent{ID: int64(i + 1), Hello: h}
	}
	return out
}

func newTestPairing(t *testing.T, wins func() ([]broadcast.Window, error), procRoot string, notify func(string, bool)) *pairing {
	t.Helper()
	return newPairing(
		func(context.Context) ([]broadcast.Window, error) { return wins() },
		func(pid int) (string, error) { return prefix.PrefixOfPIDIn(procRoot, pid) },
		notify,
	)
}

// TestPairingPrefersPrefixOverTitle is the point of the join: the captions are
// deliberately swapped relative to the prefixes, so a title match would pair
// each agent with the *wrong* window and only the pid -> environ -> WINEPREFIX
// route gets it right.
func TestPairingPrefersPrefixOverTitle(t *testing.T) {
	bottleA, bottleB := t.TempDir(), t.TempDir()
	root := fakeProc(t, map[int][]string{
		5200: {"WINEPREFIX=" + bottleA},
		5300: {"WINEPREFIX=" + bottleB},
	})
	wins := []broadcast.Window{
		{ID: "wA", Title: "Marx", PID: 5200},     // caption says Marx, prefix is A
		{ID: "wB", Title: "Malahmen", PID: 5300}, // caption says Malahmen, prefix is B
	}
	p := newTestPairing(t, func() ([]broadcast.Window, error) { return wins, nil }, root, nil)
	agents := agentsFor(
		agentwire.Hello{Prefix: bottleA, Title: "Malahmen"},
		agentwire.Hello{Prefix: bottleB, Title: "Marx"},
	)
	p.recompute(context.Background(), agents)

	if id, ok := p.agentFor("wA"); !ok || id != 1 {
		t.Errorf("wA paired with agent %d (ok=%v), want the bottleA agent (1)", id, ok)
	}
	if id, ok := p.agentFor("wB"); !ok || id != 2 {
		t.Errorf("wB paired with agent %d (ok=%v), want the bottleB agent (2)", id, ok)
	}
	if w, ok := p.windowFor(1); !ok || w != "wA" {
		t.Errorf("windowFor(1) = %q (ok=%v), want wA", w, ok)
	}
}

// TestPairingTitleFallbackAndMismatchGuard: the title match is used only where
// there is no prefix information to go on. Two known prefixes that disagree is
// a positive mismatch, not missing information, so a shared caption must not
// pair them.
func TestPairingTitleFallbackAndMismatchGuard(t *testing.T) {
	bottleA, bottleB := t.TempDir(), t.TempDir()
	root := fakeProc(t, map[int][]string{
		5200: {"WINEPREFIX=" + bottleA},
		9001: nil, // unreadable environ: this is where the fallback belongs
	})
	wins := []broadcast.Window{
		{ID: "known", Title: "World of Warcraft", PID: 5200},
		{ID: "opaque", Title: "World of Warcraft", PID: 9001},
	}
	p := newTestPairing(t, func() ([]broadcast.Window, error) { return wins, nil }, root, nil)

	// One agent, in a prefix that is NOT the known window's: it must not be
	// title-matched onto "known", but "opaque" has no prefix to contradict it.
	p.recompute(context.Background(), agentsFor(agentwire.Hello{Prefix: bottleB, Title: "World of Warcraft"}))
	if _, ok := p.agentFor("known"); ok {
		t.Error("an agent from another known prefix was title-matched onto a window whose prefix is known")
	}
	if id, ok := p.agentFor("opaque"); !ok || id != 1 {
		t.Errorf("opaque paired with %d (ok=%v), want the agent by title fallback", id, ok)
	}

	// An agent with no announced prefix at all (WINEPREFIX missing from the
	// Windows environment) also falls back to the title.
	p2 := newTestPairing(t, func() ([]broadcast.Window, error) { return wins, nil }, root, nil)
	p2.recompute(context.Background(), agentsFor(agentwire.Hello{Title: "World of Warcraft"}))
	if _, ok := p2.agentFor("known"); !ok {
		t.Error("a prefix-less agent was not title-matched to anything")
	}
}

// TestPairingOneAgentPerWindow: two agents cannot both claim one window, and
// an agent that matches nothing stays unpaired.
func TestPairingOneAgentPerWindow(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	p := newTestPairing(t, func() ([]broadcast.Window, error) { return wins, nil }, t.TempDir(), nil)
	p.recompute(context.Background(), agentsFor(
		agentwire.Hello{Title: "Game"},
		agentwire.Hello{Title: "Game"},
	))
	if id, ok := p.agentFor("w1"); !ok || id != 1 {
		t.Fatalf("w1 paired with %d (ok=%v), want the first agent", id, ok)
	}
	if _, ok := p.windowFor(2); ok {
		t.Error("the second agent claimed a window that was already taken")
	}
}

// TestPairingReportsMethodOnce: the log says how each agent was paired the
// first time and then shuts up — the pairing is recomputed every couple of
// seconds, and a line per recompute would drown the log.
func TestPairingReportsMethodOnce(t *testing.T) {
	bottle := t.TempDir()
	root := fakeProc(t, map[int][]string{5200: {"WINEPREFIX=" + bottle}})
	wins := []broadcast.Window{
		{ID: "w1", Title: "Game", PID: 5200},
		{ID: "w2", Title: "Other", PID: 0},
	}
	var lines []string
	p := newTestPairing(t, func() ([]broadcast.Window, error) { return wins, nil }, root,
		func(s string, _ bool) { lines = append(lines, s) })

	agents := agentsFor(
		agentwire.Hello{Prefix: bottle, Title: "Game"},
		agentwire.Hello{Prefix: "/bottles/nobody", Title: "Nobody"},
	)
	for i := 0; i < 3; i++ {
		p.recompute(context.Background(), agents)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d log lines for 3 recomputes of 2 agents, want 2:\n%v", len(lines), lines)
	}
	if want := "paired by prefix"; !contains(lines, want) {
		t.Errorf("no line mentions %q: %v", want, lines)
	}
	if want := "unpaired"; !contains(lines, want) {
		t.Errorf("no line mentions %q: %v", want, lines)
	}
}

// TestPairingKeepsPairsWhenTheWindowListFails: losing the compositor for one
// poll must not make every target look agent-less (and so silently stop
// receiving keys).
func TestPairingKeepsPairsWhenTheWindowListFails(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	fail := false
	var errs int
	p := newTestPairing(t, func() ([]broadcast.Window, error) {
		if fail {
			return nil, errors.New("kwin: no reply")
		}
		return wins, nil
	}, t.TempDir(), func(_ string, isErr bool) {
		if isErr {
			errs++
		}
	})
	agents := agentsFor(agentwire.Hello{Title: "Game"})
	p.recompute(context.Background(), agents)
	if _, ok := p.agentFor("w1"); !ok {
		t.Fatal("not paired to begin with")
	}

	fail = true
	p.recompute(context.Background(), agents)
	p.recompute(context.Background(), agents)
	if _, ok := p.agentFor("w1"); !ok {
		t.Error("a failed window list dropped the existing pairing")
	}
	if errs != 1 {
		t.Errorf("reported the window-list failure %d times, want once", errs)
	}
}

// TestPairingCachesTheProcRead: a window's prefix cannot change, so the
// /proc read happens once however often the pairing is recomputed.
func TestPairingCachesTheProcRead(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game", PID: 5200}}
	reads := 0
	p := newPairing(
		func(context.Context) ([]broadcast.Window, error) { return wins, nil },
		func(pid int) (string, error) { reads++; return "/bottles/Game", nil },
		nil,
	)
	agents := agentsFor(agentwire.Hello{Prefix: "/bottles/Game"})
	for i := 0; i < 5; i++ {
		p.recompute(context.Background(), agents)
	}
	if reads != 1 {
		t.Fatalf("read /proc %d times, want 1", reads)
	}
	// A new pid under the same window id (a window that was replaced) must be
	// resolved again rather than served from the cache.
	wins = []broadcast.Window{{ID: "w1", Title: "Game", PID: 5301}}
	p.recompute(context.Background(), agents)
	if reads != 2 {
		t.Fatalf("read /proc %d times after the pid changed, want 2", reads)
	}
}

func contains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
