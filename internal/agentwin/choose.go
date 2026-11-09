// Package agentwin is how the agent picks its prefix's game window out of the
// windows it can see. It is plain Go with no build tag so it is tested on
// Linux; cmd/clonecast-agent (Windows only) walks the window tree, describes
// each window as a Candidate and lets Choose decide.
//
// The agent used to keep the first window it found for as long as that window
// stayed alive and visible (Kamino 202). The Battle.net launcher, or a
// dialog, is up before the game's own window, so an agent asked for a window
// at that moment kept the launcher: every key went there, the game got
// nothing, and nothing ever re-chose. Now the choice is made again whenever
// the chosen window stops qualifying and, while it does, every Recheck.Every,
// and an owned window (a dialog, a popup) loses to any unowned one.
package agentwin

import (
	"time"

	"github.com/malahmen/clonecast/internal/lookup"
)

// MinSide keeps out the IME and helper popups Wine creates, the same floor the
// launcher's own window search uses.
const MinSide = 100

// Candidate is one window the agent can see.
type Candidate struct {
	Handle  uintptr
	Visible bool
	W, H    int64
	// Owned is a window with an owner: dialogs, message boxes, popups. A
	// game's main window has none.
	Owned bool
	// Tool is a WS_EX_TOOLWINDOW: a floating palette or a helper window,
	// never the game.
	Tool bool
	// Minimized is an iconic window. Its rectangle is the icon's, so its size
	// says nothing about it.
	Minimized bool
}

// Usable reports whether c can be the game window at all.
func (c Candidate) Usable() bool {
	return c.Handle != 0 && c.Visible && !c.Minimized && c.W >= MinSide && c.H >= MinSide
}

// better reports whether a ranks above b: an unowned, non-tool window first,
// then the larger.
func better(a, b Candidate) bool {
	if pa, pb := a.primary(), b.primary(); pa != pb {
		return pa
	}
	return a.W*a.H > b.W*b.H
}

func (c Candidate) primary() bool { return !c.Owned && !c.Tool }

// Choose returns the game window among cands, or 0 when none is usable. The
// game is the largest visible unowned window: each Wine prefix has its own
// wineserver and so its own window list, so every window here is this
// prefix's, and the game's is the big one.
//
// current is the window already in use. It is kept unless another ranks
// strictly above it, so two equal windows do not make the agent flip between
// them on every recheck, and kept while it is minimized.
func Choose(cands []Candidate, current uintptr) uintptr {
	var best Candidate
	found := false
	for _, c := range cands {
		if !c.Usable() {
			continue
		}
		if !found || better(c, best) {
			best, found = c, true
		}
	}
	for _, c := range cands {
		if current == 0 || c.Handle != current {
			continue
		}
		if c.Usable() && !better(best, c) {
			return current
		}
		// A minimized game is still the game: minimizing it must not hand
		// its keys to the launcher.
		if c.Visible && c.Minimized && c.primary() {
			return current
		}
	}
	return best.Handle
}

// Recheck paces the re-choosing of a window that is still valid: a full walk
// of the window tree on every key would be wasteful, and never doing it is
// what kept the launcher.
type Recheck struct {
	Every time.Duration
	last  time.Time
}

// Due reports whether a recheck is due at now, and if so starts the next
// interval.
func (r *Recheck) Due(now time.Time) bool {
	if !r.last.IsZero() && now.Sub(r.last) < r.Every {
		return false
	}
	r.last = now
	return true
}

// Done records a full walk made for another reason (the window was gone), so
// the next recheck is an interval after it.
func (r *Recheck) Done(now time.Time) { r.last = now }

// Windows is what a Tracker asks of the window system. The agent implements
// it with Win32 calls; the tests with a fake.
type Windows interface {
	// Alive reports whether h is still a window (IsWindow).
	Alive(h uintptr) bool
	// Visible reports whether h is visible.
	Visible(h uintptr) bool
	// Title is h's caption.
	Title(h uintptr) string
	// ByTitle walks the window tree for a window with this caption, or 0.
	ByTitle(title string) uintptr
	// Game walks the window tree for the prefix's game window (Choose), with
	// current the window in use, or 0.
	Game(current uintptr) uintptr
}

// Tracker keeps the window the agent delivers to. It caches it and checks
// it cheaply on every key; the tree is walked only when it goes stale (at
// most every Pace.Every while nothing is found) and, while it is valid, every
// Recheck.Every. WoW recreates its top-level window across state changes
// (login, character select, world, loading screens), so a cached handle
// does go stale. Not safe for concurrent use; the agent's resolver holds its
// own lock around it.
type Tracker struct {
	// Title, when set, names the window to deliver to. When no window has it
	// the prefix-wide choice (Windows.Game) is used instead, so a wrong or
	// stale title degrades to "this prefix's game" and not to nothing.
	Title   string
	Pace    lookup.Pacer
	Recheck Recheck

	hwnd uintptr
	// fallback is a window found by the prefix-wide search although Title is
	// set. It is checked as a prefix-wide window is (alive and visible), not
	// against Title: it never had the title, so that check failed on every
	// key and walked the whole tree for every key (Kamino 278). Title is
	// looked for again on the recheck instead.
	fallback bool
}

// Result is what Current did, for the agent's log lines.
type Result struct {
	// HWND is the window to deliver to, or 0 when there is none.
	HWND uintptr
	// Replaced is the window a recheck moved away from, or 0.
	Replaced uintptr
	// Searched is set when the cached window was stale and the tree walked.
	Searched bool
	// Dropped is, when a search found a window after some found nothing, how
	// many keys were dropped in between.
	Dropped int
}

// prefixWide reports whether the window in use was chosen by the prefix-wide
// search: always without a title, and for the fallback with one.
func (t *Tracker) prefixWide() bool { return t.Title == "" || t.fallback }

func (t *Tracker) valid(w Windows) bool {
	if t.hwnd == 0 || !w.Alive(t.hwnd) {
		return false
	}
	if t.prefixWide() {
		// Any live, visible window is still the one we found: WoW keeps one
		// top-level window per state, and the recheck finds a better one.
		return w.Visible(t.hwnd)
	}
	return w.Title(t.hwnd) == t.Title
}

// Current returns the window to deliver to at now.
func (t *Tracker) Current(now time.Time, w Windows) Result {
	if t.valid(w) {
		res := Result{HWND: t.hwnd}
		// An explicit -title names the window; only the prefix-wide choice
		// can have picked the wrong one.
		if t.prefixWide() && t.Recheck.Due(now) {
			h := uintptr(0)
			if t.fallback {
				if h = w.ByTitle(t.Title); h != 0 {
					t.fallback = false
				}
			}
			if h == 0 {
				h = w.Game(t.hwnd)
			}
			if h != 0 && h != t.hwnd {
				res.Replaced, res.HWND, t.hwnd = t.hwnd, h, h
			}
		}
		return res
	}
	t.fallback = false
	if !t.Pace.Due(now) {
		t.hwnd = 0
		return Result{}
	}
	t.Recheck.Done(now)
	if t.Title == "" {
		t.hwnd = w.Game(0)
	} else if t.hwnd = w.ByTitle(t.Title); t.hwnd == 0 {
		t.hwnd = w.Game(0)
		t.fallback = t.hwnd != 0
	}
	res := Result{HWND: t.hwnd, Searched: true}
	if t.hwnd == 0 {
		t.Pace.Missed(now)
	} else {
		res.Dropped = t.Pace.Found()
	}
	return res
}
