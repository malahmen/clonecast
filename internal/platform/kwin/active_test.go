//go:build linux

package kwin

import (
	"testing"

	"github.com/malahmen/clonecast/internal/broadcast"
)

// The three clients and the terminal as the box reported them on 2026-10-18,
// pids included: Malahmen, Marx and Almerinda are XWayland (Wine) clients,
// Konsole is native Wayland, 4115 owns an X client but no normal window.
var (
	malahmen  = pidWindow{ID: "{malahmen}", PID: 1890200}
	marx      = pidWindow{ID: "{marx}", PID: 1891544}
	almerinda = pidWindow{ID: "{almerinda}", PID: 1889512}
	konsole   = pidWindow{ID: "{konsole}", PID: 7001}
	all       = []pidWindow{malahmen, marx, almerinda, konsole}
	xPIDs     = map[int]bool{malahmen.PID: true, marx.PID: true, almerinda.PID: true, 4115: true}
)

func state(active *pidWindow) activeState { return activeState{Active: active, Windows: all} }

func TestPickActive(t *testing.T) {
	cases := []struct {
		name     string
		active   *pidWindow
		focusPID int
		xPIDs    map[int]bool
		want     broadcast.WindowID
	}{
		{"KWin and X agree", &marx, marx.PID, xPIDs, "{marx}"},
		// The bug: KWin still on Malahmen, keys going to Almerinda (the probe's
		// 22:24:01.8 line). The master is where the keys go.
		{"KWin behind the X focus between two clients", &malahmen, almerinda.PID, xPIDs, "{almerinda}"},
		// KWin lagging the other way round (22:23:58.1): it said Almerinda,
		// X had moved to Marx.
		{"KWin lagging a click", &almerinda, marx.PID, xPIDs, "{marx}"},
		// The terminal is never overridden, so the origin gate still sees it
		// while the X focus is left on a game.
		{"native Wayland window active", &konsole, malahmen.PID, xPIDs, "{konsole}"},
		{"no X focus", &malahmen, 0, xPIDs, "{malahmen}"},
		{"X focus on a client with no normal window", &malahmen, 4115, xPIDs, "{malahmen}"},
		{"X side unavailable", &malahmen, 0, nil, "{malahmen}"},
		{"X pids unknown but a focus pid given", &malahmen, almerinda.PID, nil, "{malahmen}"},
		{"nothing active", nil, malahmen.PID, xPIDs, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickActive(state(c.active), c.focusPID, c.xPIDs); got != c.want {
				t.Fatalf("pickActive = %q, want %q", got, c.want)
			}
		})
	}
}
