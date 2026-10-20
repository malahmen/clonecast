//go:build linux

package x11

import (
	"testing"
	"time"
)

// The steals traced on the box (2026-10-19, clonecast closed), and the cases
// that must be left alone.
func TestShouldCorrect(t *testing.T) {
	const kwin, almerinda, malahmen, marx = 3798, 73006, 73601, 74196
	cases := []struct {
		name                    string
		wantPID, havePID, wmPID int
		want                    bool
	}{
		{"the client clicked away from took it back", malahmen, almerinda, kwin, true},
		{"a client nobody clicked took it", malahmen, marx, kwin, true},
		{"focus is on the active client", malahmen, malahmen, kwin, false},
		// KWin leaves the property on the last X window when the terminal
		// is active; the X focus is then KWin's.
		{"a Wayland window is active", malahmen, kwin, kwin, false},
		{"nobody has the focus", malahmen, 0, kwin, false},
		{"owner of the active window unknown", 0, almerinda, kwin, false},
		{"KWin's own window is active", kwin, almerinda, kwin, false},
	}
	for _, c := range cases {
		if got := shouldCorrect(c.wantPID, c.havePID, c.wmPID); got != c.want {
			t.Errorf("%s: shouldCorrect = %v, want %v", c.name, got, c.want)
		}
	}
}

// Traced with the guard on (2026-10-19): a click moved the X focus up to about
// a second before KWin named the clicked game active, and correcting at once
// handed the keyboard back to the game clicked away from.
func TestCorrectNow(t *testing.T) {
	cases := []struct {
		name       string
		underMouse bool
		diverged   time.Duration
		want       bool
	}{
		{"a steal by a game away from the mouse", false, 0, true},
		{"a click KWin has not published yet", true, clickGrace / 2, false},
		{"under the mouse but KWin never followed", true, clickGrace, true},
	}
	for _, c := range cases {
		if got := correctNow(c.underMouse, c.diverged); got != c.want {
			t.Errorf("%s: correctNow = %v, want %v", c.name, got, c.want)
		}
	}
}

// Traced 2026-10-20: a click on a title bar left the pointer on KWin's
// decoration, and the guard took the keyboard back from the clicked game.
func TestIsUnderMouse(t *testing.T) {
	const kwin, malahmen, marx = 3798, 2695299, 2708670
	cases := []struct {
		name                    string
		havePID, pointerPID, wm int
		want                    bool
	}{
		{"pointer on the game that has the focus", malahmen, malahmen, kwin, true},
		{"pointer on a title bar", malahmen, kwin, kwin, true},
		{"pointer on another game", malahmen, marx, kwin, false},
		{"pointer unknown", malahmen, 0, kwin, false},
		{"pointer unknown, KWin unknown", malahmen, 0, 0, false},
	}
	for _, c := range cases {
		if got := isUnderMouse(c.havePID, c.pointerPID, c.wm); got != c.want {
			t.Errorf("%s: isUnderMouse = %v, want %v", c.name, got, c.want)
		}
	}
}

// Traced 2026-10-20: KWin did not activate the game clicked, and following
// KWin handed the keyboard back to the game clicked away from.
func TestClickedGame(t *testing.T) {
	const kwin, malahmen, marx = 3798, 2695299, 2708670
	cases := []struct {
		name                    string
		havePID, pointerPID, wm int
		want                    bool
	}{
		{"click on a game that took the focus", marx, marx, kwin, true},
		{"click on a game KWin left the focus away from", malahmen, marx, kwin, true},
		{"click on a title bar", marx, kwin, kwin, false},
		{"click on the terminal", kwin, marx, kwin, false},
		{"nobody has the focus", 0, marx, kwin, false},
		{"pointer unknown", malahmen, 0, kwin, false},
	}
	for _, c := range cases {
		if got := clickedGame(c.havePID, c.pointerPID, c.wm); got != c.want {
			t.Errorf("%s: clickedGame = %v, want %v", c.name, got, c.want)
		}
	}
}
