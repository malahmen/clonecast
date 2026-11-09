//go:build linux

package x11

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jezek/xgb/xproto"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
)

// fakeTree is an X window tree: who created each window, its name and its
// children.
type fakeTree struct {
	kids  map[xproto.Window][]xproto.Window
	names map[xproto.Window]string
	pids  map[xproto.Window]int
}

func (t fakeTree) children(w xproto.Window) []xproto.Window { return t.kids[w] }
func (t fakeTree) name(w xproto.Window) string              { return t.names[w] }
func (t fakeTree) owner(w xproto.Window) int                { return t.pids[w] }

const wow = "World of Warcraft"

// twoDesktops is two Wine clients, each in its own virtual desktop: KWin
// frames (pid 1) hold each desktop (its explorer.exe: 100, 200), which holds
// the game window (the game process: 101, 201). Both games have the same
// title, as every WoW client does.
func twoDesktops() fakeTree {
	return fakeTree{
		kids: map[xproto.Window][]xproto.Window{
			1:  {10, 20},
			10: {11},
			11: {12},
			20: {21},
			21: {22},
		},
		names: map[xproto.Window]string{11: "Almerinda - Wine desktop", 12: wow, 21: "Malahmen - Wine desktop", 22: wow},
		pids:  map[xproto.Window]int{10: 1, 20: 1, 11: 100, 12: 101, 21: 200, 22: 201},
	}
}

// Kamino 202: the window is looked for under the target's own client. The
// whole-tree title search found window 12 for every target.
func TestFindUnderScopesToTheTargetsClient(t *testing.T) {
	tree := twoDesktops()
	if w, n := findUnder(tree, 1, 200, wow); w != 22 || n != 1 {
		t.Fatalf("pid 200: got window %d (%d matches), want 22 (1)", w, n)
	}
	if w, n := findUnder(tree, 1, 100, wow); w != 12 || n != 1 {
		t.Fatalf("pid 100: got window %d (%d matches), want 12 (1)", w, n)
	}
	// The desktop itself, by its own title, for a target with no --xsend map.
	if w, n := findUnder(tree, 1, 200, "Malahmen - Wine desktop"); w != 21 || n != 1 {
		t.Fatalf("desktop title: got window %d (%d matches), want 21 (1)", w, n)
	}
	// A pid that owns nothing finds nothing, even though the title exists.
	if _, n := findUnder(tree, 1, 300, wow); n != 0 {
		t.Fatalf("pid 300 found %d windows, want 0", n)
	}
	if _, n := findUnder(tree, 1, 0, wow); n != 0 {
		t.Fatalf("pid 0 found %d windows, want 0", n)
	}
}

// Two games in one virtual desktop are counted, so the deliverer can refuse
// rather than guess.
func TestFindUnderCountsSeveralGamesInOneDesktop(t *testing.T) {
	tree := twoDesktops()
	tree.kids[11] = []xproto.Window{12, 13}
	tree.names[13] = wow
	tree.pids[13] = 102
	if w, n := findUnder(tree, 1, 100, wow); w != 12 || n != 2 {
		t.Fatalf("got window %d (%d matches), want 12 (2)", w, n)
	}
}

// fakeX records sends and answers Find from a table keyed by pid.
type fakeX struct {
	wins  map[int]xproto.Window // pid -> window
	count map[int]int           // pid -> matches (default 1 when in wins)
	finds int
	sent  []xproto.Window
}

func (f *fakeX) Find(pid int, title string) (xproto.Window, int, error) {
	f.finds++
	w, ok := f.wins[pid]
	if !ok {
		return 0, 0, nil
	}
	if n, ok := f.count[pid]; ok {
		return w, n, nil
	}
	return w, 1, nil
}

func (f *fakeX) SendKey(w xproto.Window, _ keys.Code, _ bool, _ uint16) error {
	f.sent = append(f.sent, w)
	return nil
}

func (f *fakeX) Close() error { return nil }

type notes []string

func (n *notes) add(s string, _ bool) { *n = append(*n, s) }

func deliver(t *testing.T, d *deliverer, origin broadcast.WindowID, targets ...broadcast.WindowID) (int, notes) {
	t.Helper()
	var ns notes
	reached, err := d.Deliver(context.Background(), broadcast.Delivery{
		Event: keys.Event{Code: 17, State: keys.Down}, Targets: targets, Origin: origin, Notify: ns.add,
	})
	if err != nil {
		t.Fatal(err)
	}
	return len(reached), ns
}

func lookupOf(m map[broadcast.WindowID]Target) func(broadcast.WindowID) Target {
	return func(id broadcast.WindowID) Target { return m[id] }
}

func TestXsendAimsEachTargetAtItsOwnWindow(t *testing.T) {
	x := &fakeX{wins: map[int]xproto.Window{100: 12, 200: 22}}
	d := newDeliverer(x, lookupOf(map[broadcast.WindowID]Target{
		"g1": {wow, 100}, "g2": {wow, 200},
	}))
	if n, ns := deliver(t, d, "", "g1", "g2"); n != 2 || len(ns) != 0 {
		t.Fatalf("delivered %d, notices %q", n, ns)
	}
	if fmt.Sprint(x.sent) != "[12 22]" {
		t.Fatalf("sent to %v, want [12 22]", x.sent)
	}
}

// Kamino 202: two targets on one X window are one Wine virtual desktop seen
// twice; neither gets the key, and the notice names both.
func TestXsendRefusesTargetsOnOneXWindow(t *testing.T) {
	x := &fakeX{wins: map[int]xproto.Window{100: 12, 200: 22}}
	d := newDeliverer(x, lookupOf(map[broadcast.WindowID]Target{
		"g1": {wow, 100}, "g2": {wow, 100}, "g3": {wow, 200},
	}))
	n, ns := deliver(t, d, "", "g1", "g2", "g3")
	if n != 1 || fmt.Sprint(x.sent) != "[22]" {
		t.Fatalf("delivered %d to %v, want 1 to [22]", n, x.sent)
	}
	if len(ns) != 1 || !strings.Contains(ns[0], "g1, g2") {
		t.Fatalf("notices %q, want one naming g1, g2", ns)
	}
}

// Several games inside one desktop: refused, not guessed.
func TestXsendRefusesAnAmbiguousDesktop(t *testing.T) {
	x := &fakeX{wins: map[int]xproto.Window{100: 12}, count: map[int]int{100: 2}}
	d := newDeliverer(x, lookupOf(map[broadcast.WindowID]Target{"g1": {wow, 100}}))
	n, ns := deliver(t, d, "", "g1")
	if n != 0 || len(x.sent) != 0 || len(ns) != 1 || !strings.Contains(ns[0], "virtual desktop") {
		t.Fatalf("delivered %d to %v, notices %q", n, x.sent, ns)
	}
}

// Kamino 202: the origin is skipped by X window. g2 is the master's own X
// window under another KWin id, so sending there would double its key.
func TestXsendSkipsTheOriginsXWindow(t *testing.T) {
	x := &fakeX{wins: map[int]xproto.Window{100: 12, 200: 22}}
	d := newDeliverer(x, lookupOf(map[broadcast.WindowID]Target{
		"g1": {wow, 100}, "g2": {wow, 100}, "g3": {wow, 200},
	}))
	n, ns := deliver(t, d, "g1", "g1", "g2", "g3")
	if n != 1 || fmt.Sprint(x.sent) != "[22]" || len(ns) != 0 {
		t.Fatalf("delivered %d to %v, notices %q; want 1 to [22], none", n, x.sent, ns)
	}
}

// Kamino 202: the cache re-resolves when the target list changes, and when a
// target's pid or title does, not only after a failed send.
func TestXsendReResolvesWhenTheWindowsChange(t *testing.T) {
	x := &fakeX{wins: map[int]xproto.Window{100: 12, 200: 22}}
	targets := map[broadcast.WindowID]Target{"g1": {wow, 100}, "g2": {wow, 200}}
	d := newDeliverer(x, lookupOf(targets))

	deliver(t, d, "", "g1")
	deliver(t, d, "", "g1")
	if x.finds != 1 {
		t.Fatalf("same list: %d finds, want 1 (cached)", x.finds)
	}

	// The game recreated its window; a new list must not reuse the old one.
	x.wins[100] = 13
	x.sent = nil
	deliver(t, d, "", "g1", "g2")
	if fmt.Sprint(x.sent) != "[13 22]" {
		t.Fatalf("after the list changed sent to %v, want [13 22]", x.sent)
	}

	// The client restarted under a new pid, same KWin id and list.
	x.wins[300] = 30
	targets["g1"] = Target{wow, 300}
	x.sent = nil
	deliver(t, d, "", "g1", "g2")
	if fmt.Sprint(x.sent) != "[30 22]" {
		t.Fatalf("after the pid changed sent to %v, want [30 22]", x.sent)
	}
}

// A target KWin no longer lists, or lists with no pid, is reported and skipped.
func TestXsendReportsATargetItCannotPlace(t *testing.T) {
	x := &fakeX{wins: map[int]xproto.Window{100: 12}}
	d := newDeliverer(x, lookupOf(map[broadcast.WindowID]Target{"g1": {wow, 0}}))
	n, ns := deliver(t, d, "", "g1", "gone")
	if n != 0 || len(x.sent) != 0 || len(ns) != 2 || x.finds != 0 {
		t.Fatalf("delivered %d to %v, %d finds, notices %q", n, x.sent, x.finds, ns)
	}
}
