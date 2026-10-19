package tui

import (
	"context"
	"testing"

	"github.com/malahmen/clonecast/internal/broadcast"
)

// laggingWM reports the previous window as active for the first few reads
// after an Activate, as KWin and the X focus can.
type laggingWM struct {
	prev, active broadcast.WindowID
	lag, reads   int
}

func (w *laggingWM) List(context.Context) ([]broadcast.Window, error) { return nil, nil }
func (w *laggingWM) Activate(_ context.Context, id broadcast.WindowID) error {
	w.prev, w.active, w.reads = w.active, id, 0
	return nil
}
func (w *laggingWM) Active(context.Context) (broadcast.WindowID, error) {
	w.reads++
	if w.reads <= w.lag {
		return w.prev, nil
	}
	return w.active, nil
}

// Kamino item 110: focusing a window from the TUI must report that window as
// the master once the switch lands, not the one it read straight after.
func TestFocusWindowWaitsForTheSwitch(t *testing.T) {
	wm := &laggingWM{active: "marx", lag: 3}
	msg := Model{wm: wm}.focusWindow("malahmen")().(masterMsg)
	if msg.err != nil || msg.id != "malahmen" {
		t.Fatalf("masterMsg = %+v, want malahmen", msg)
	}
}

// A switch that never lands is reported as it stands, not waited on forever.
func TestFocusWindowReportsAStuckSwitch(t *testing.T) {
	wm := &laggingWM{active: "marx", lag: 1000}
	msg := Model{wm: wm}.focusWindow("malahmen")().(masterMsg)
	if msg.id != "marx" {
		t.Fatalf("masterMsg = %+v, want the window that is really active, marx", msg)
	}
}
