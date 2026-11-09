package tui

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
	"github.com/malahmen/clonecast/internal/platform/mock"
)

type chanSource struct{ ch chan keys.Event }

func (s *chanSource) Events() <-chan keys.Event { return s.ch }
func (s *chanSource) Close() error              { return nil }

// upDeliverer records the targets of every Up and skips the unreachable
// windows, as the agent backend skips a window with no agent.
type upDeliverer struct {
	mu          sync.Mutex
	unreachable map[broadcast.WindowID]bool
	ups         [][]broadcast.WindowID
}

func (d *upDeliverer) MovesFocus() bool { return false }
func (d *upDeliverer) Close() error     { return nil }

func (d *upDeliverer) Deliver(_ context.Context, req broadcast.Delivery) ([]broadcast.WindowID, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var reached []broadcast.WindowID
	for _, t := range req.Targets {
		if t != req.Origin && !d.unreachable[t] {
			reached = append(reached, t)
		}
	}
	if req.Event.State == keys.Up {
		d.ups = append(d.ups, append([]broadcast.WindowID(nil), req.Targets...))
	}
	return reached, nil
}

func (d *upDeliverer) set(id broadcast.WindowID, unreachable bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.unreachable[id] = unreachable
}

// upsSince returns the targets of every Up delivered after the first n.
func (d *upDeliverer) upsSince(n int) [][]broadcast.WindowID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([][]broadcast.WindowID(nil), d.ups[n:]...)
}

func (d *upDeliverer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.ups)
}

func has(ids []broadcast.WindowID, id broadcast.WindowID) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// Kamino 278: a window that closes while its release could not reach it is
// forgotten when the window list no longer has it, so the next Up of the key
// is not sent to it again. A window only unticked, still in the list, keeps
// its held key and still gets the Up.
func TestAWindowGoneFromTheListIsForgotten(t *testing.T) {
	wm := mock.NewWM() // w1 (the terminal) is focused
	src := &chanSource{ch: make(chan keys.Event, 4)}
	engine := broadcast.New(src, &mock.Injector{}, wm, broadcast.DefaultConfig())
	d := &upDeliverer{unreachable: map[broadcast.WindowID]bool{}}
	engine.SetDeliverer(d)
	engine.SetFilter(keys.All())
	engine.SetEnabled(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = engine.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	m := New(engine, wm, nil, nil, PrefixPaneConfig{}, nil)
	all, _ := wm.List(ctx)
	next, _ := m.Update(windowsMsg{wins: all})
	m = next.(Model)
	for _, id := range []broadcast.WindowID{"w1", "w3", "w4"} {
		m.selected[id] = true
	}
	m.pushTargets()

	a, _ := keys.Parse("a")
	settle := func() { time.Sleep(60 * time.Millisecond) }
	src.ch <- keys.Event{Code: a, State: keys.Down}
	settle()

	// w3 closes and w4 is unticked; neither release gets through.
	d.set("w3", true)
	d.set("w4", true)
	m.selected["w4"] = false
	var left []broadcast.Window
	for _, w := range all {
		if w.ID != "w3" {
			left = append(left, w)
		}
	}
	next, _ = m.Update(windowsMsg{wins: left})
	m = next.(Model)
	settle()

	d.set("w4", false)
	before := d.count()
	src.ch <- keys.Event{Code: a, State: keys.Up}
	settle()
	sent := d.upsSince(before)
	if len(sent) != 1 {
		t.Fatalf("the user's Up was delivered %d time(s), want once: %v", len(sent), sent)
	}
	got := sent[0]
	if has(got, "w3") {
		t.Errorf("the Up went to %v: w3 is gone and must not be retried", got)
	}
	if !has(got, "w4") {
		t.Errorf("the Up went to %v: w4 still exists and still holds A", got)
	}
}
