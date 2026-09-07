// Package mock is a fake platform so the TUI can be developed and demoed on
// any OS, including the macOS box this project is written on. It generates a
// slow stream of synthetic key events, keeps a static list of pretend windows,
// and logs injections instead of performing them.
package mock

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
)

// Source emits A, B, C, D presses on a timer.
type Source struct {
	ch   chan keys.Event
	stop chan struct{}
	once sync.Once
}

// NewSource starts the synthetic stream. Every `every` interval it emits a
// Down then an Up for the next key in the cycle.
func NewSource(every time.Duration) *Source {
	s := &Source{ch: make(chan keys.Event, 16), stop: make(chan struct{})}
	go s.loop(every)
	return s
}

func (s *Source) loop(every time.Duration) {
	cycle := []string{"a", "b", "c", "d"}
	codes := make([]keys.Code, len(cycle))
	for i, n := range cycle {
		codes[i], _ = keys.Parse(n)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-s.stop:
			close(s.ch)
			return
		case <-t.C:
			c := codes[i%len(codes)]
			i++
			s.ch <- keys.Event{Code: c, State: keys.Down}
			s.ch <- keys.Event{Code: c, State: keys.Up}
		}
	}
}

func (s *Source) Events() <-chan keys.Event { return s.ch }
func (s *Source) Close() error {
	s.once.Do(func() { close(s.stop) })
	return nil
}

// Injector records emits. Sink, when set, receives a line per emit.
type Injector struct {
	Sink func(string)
}

func (i *Injector) Emit(ev keys.Event) error {
	if i.Sink != nil {
		i.Sink(fmt.Sprintf("inject %v", ev))
	}
	return nil
}
func (i *Injector) Close() error { return nil }

// WM is a static window list with an active window pointer.
type WM struct {
	mu      sync.Mutex
	windows []broadcast.Window
	active  broadcast.WindowID
	Sink    func(string)
}

// NewWM returns a WM with a plausible Bazzite desktop.
func NewWM() *WM {
	return &WM{
		windows: []broadcast.Window{
			{ID: "w1", Title: "clonecast — konsole", Class: "org.kde.konsole", PID: 4242},
			{ID: "w2", Title: "Steam", Class: "steam", PID: 5100},
			{ID: "w3", Title: "Game — instance 1", Class: "steam_app_1234", PID: 5200},
			{ID: "w4", Title: "Game — instance 2", Class: "steam_app_1234", PID: 5300},
			{ID: "w5", Title: "Firefox", Class: "firefox", PID: 6000},
		},
		active: "w1",
	}
}

func (w *WM) List(context.Context) ([]broadcast.Window, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]broadcast.Window, len(w.windows))
	for i, win := range w.windows {
		win.Focused = win.ID == w.active
		out[i] = win
	}
	return out, nil
}

func (w *WM) Active(context.Context) (broadcast.WindowID, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active, nil
}

func (w *WM) Activate(_ context.Context, id broadcast.WindowID) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, win := range w.windows {
		if win.ID == id {
			w.active = id
			if w.Sink != nil {
				w.Sink(fmt.Sprintf("activate %s (%s)", id, win.Title))
			}
			return nil
		}
	}
	return fmt.Errorf("no window %q", id)
}
