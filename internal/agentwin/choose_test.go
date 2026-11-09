package agentwin

import (
	"testing"
	"time"
)

var (
	launcher = Candidate{Handle: 1, Visible: true, W: 1280, H: 720}
	game     = Candidate{Handle: 2, Visible: true, W: 1920, H: 1080}
	dialog   = Candidate{Handle: 3, Visible: true, W: 2560, H: 1440, Owned: true}
	palette  = Candidate{Handle: 4, Visible: true, W: 2560, H: 1440, Tool: true}
	popup    = Candidate{Handle: 5, Visible: true, W: 300, H: 40}
	hidden   = Candidate{Handle: 6, Visible: false, W: 3840, H: 2160}
)

func TestChooseTheGameWindow(t *testing.T) {
	cases := []struct {
		name    string
		cands   []Candidate
		current uintptr
		want    uintptr
	}{
		{"nothing", nil, 0, 0},
		{"only unusable windows", []Candidate{popup, hidden}, 0, 0},
		{"the launcher alone, before the game is up", []Candidate{launcher}, 0, launcher.Handle},
		// The bug: the launcher was chosen first and kept while it stayed up.
		{"the game appears beside the chosen launcher", []Candidate{launcher, game}, launcher.Handle, game.Handle},
		{"a larger owned dialog does not win", []Candidate{game, dialog}, 0, game.Handle},
		{"a larger tool window does not win", []Candidate{palette, game}, 0, game.Handle},
		{"a dialog chosen while it was alone gives way", []Candidate{dialog, game}, dialog.Handle, game.Handle},
		{"a dialog is still better than nothing", []Candidate{dialog, popup}, 0, dialog.Handle},
		{"a hidden window is never chosen", []Candidate{hidden, launcher}, 0, launcher.Handle},
		{"the chosen game that went hidden is replaced", []Candidate{{Handle: 2, W: 1920, H: 1080}, launcher}, game.Handle, launcher.Handle},
		{"a vanished current window is replaced", []Candidate{launcher}, 99, launcher.Handle},
		{"a minimized game keeps its keys", []Candidate{{Handle: 2, Visible: true, W: 160, H: 28, Minimized: true}, launcher}, game.Handle, game.Handle},
		{"a minimized window is not newly chosen", []Candidate{{Handle: 2, Visible: true, W: 1920, H: 1080, Minimized: true}, launcher}, 0, launcher.Handle},
		{"a minimized dialog gives way", []Candidate{{Handle: 3, Visible: true, W: 160, H: 28, Minimized: true, Owned: true}, launcher}, dialog.Handle, launcher.Handle},
	}
	for _, c := range cases {
		if got := Choose(c.cands, c.current); got != c.want {
			t.Errorf("%s: Choose = %d, want %d", c.name, got, c.want)
		}
	}
}

// An equally good window does not displace the current one, so the agent
// does not flip between two game-sized windows on every recheck.
func TestChooseKeepsTheCurrentOnATie(t *testing.T) {
	twin := Candidate{Handle: 7, Visible: true, W: 1920, H: 1080}
	for _, order := range [][]Candidate{{game, twin}, {twin, game}} {
		if got := Choose(order, game.Handle); got != game.Handle {
			t.Errorf("Choose(%v, current=game) = %d, want the current game window", order, got)
		}
		if got := Choose(order, twin.Handle); got != twin.Handle {
			t.Errorf("Choose(%v, current=twin) = %d, want the current twin", order, got)
		}
	}
}

func TestRecheckPacing(t *testing.T) {
	r := Recheck{Every: 5 * time.Second}
	t0 := time.Unix(1000, 0)
	if !r.Due(t0) {
		t.Fatal("the first recheck is not due")
	}
	if r.Due(t0.Add(4 * time.Second)) {
		t.Error("a recheck 4s after the last is due, want every 5s")
	}
	if !r.Due(t0.Add(5 * time.Second)) {
		t.Error("a recheck 5s after the last is not due")
	}
	r.Done(t0.Add(9 * time.Second))
	if r.Due(t0.Add(12 * time.Second)) {
		t.Error("a recheck 3s after a full walk is due")
	}
	if !r.Due(t0.Add(14 * time.Second)) {
		t.Error("a recheck 5s after a full walk is not due")
	}
}
