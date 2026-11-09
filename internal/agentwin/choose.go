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

import "time"

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
