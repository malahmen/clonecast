package broadcast

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/malahmen/clonecast/internal/keys"
)

type fakeSource struct{ ch chan keys.Event }

func (f *fakeSource) Events() <-chan keys.Event { return f.ch }
func (f *fakeSource) Close() error              { return nil }

// recorder captures the exact sequence of injector and window manager calls.
type recorder struct {
	mu     sync.Mutex
	log    []string
	active WindowID
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.log = append(r.log, s)
	r.mu.Unlock()
}

func (r *recorder) Emit(ev keys.Event) error {
	r.add("emit@" + string(r.active) + ":" + keys.Name(ev.Code) + "/" + ev.State.String())
	return nil
}
func (r *recorder) Close() error { return nil }

func (r *recorder) List(context.Context) ([]Window, error) { return nil, nil }
func (r *recorder) Active(context.Context) (WindowID, error) {
	return r.active, nil
}
func (r *recorder) Activate(_ context.Context, id WindowID) error {
	r.active = id
	r.add("activate:" + string(id))
	return nil
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.log...)
}

func run(t *testing.T, e *Engine, src *fakeSource, events ...keys.Event) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = e.Run(ctx); close(done) }()
	for _, ev := range events {
		src.ch <- ev
	}
	// Let the engine drain: it processes strictly in order, so a final
	// synthetic event with a fresh channel would be over-engineering here.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
}

func newEngine(rec *recorder, src *fakeSource) *Engine {
	cfg := DefaultConfig()
	cfg.SettleDelay = 0
	return New(src, rec, rec, cfg)
}

func TestDisabledOnlyPassesThrough(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	e.SetTargets([]WindowID{"t1"})
	e.SetFilter(keys.All())

	a, _ := keys.Parse("a")
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	want := []string{"emit@origin:A/down"}
	assertLog(t, rec.snapshot(), want)
}

func TestBroadcastFocusDance(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	e.SetTargets([]WindowID{"t1", "origin", "t2"})
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)

	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	want := []string{
		"emit@origin:A/down",
		"activate:t1", "emit@t1:A/down",
		"activate:t2", "emit@t2:A/down",
		"activate:origin",
	}
	assertLog(t, rec.snapshot(), want)
}

func TestFilterAndRepeatAreNotBroadcast(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	e.SetTargets([]WindowID{"t1"})
	a, _ := keys.Parse("a")
	b, _ := keys.Parse("b")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)

	run(t, e, src,
		keys.Event{Code: b, State: keys.Down},   // filtered out
		keys.Event{Code: a, State: keys.Repeat}, // repeats never broadcast
	)

	want := []string{"emit@origin:B/down", "emit@origin:A/repeat"}
	assertLog(t, rec.snapshot(), want)
}

func TestToggleKeyIsConsumed(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)
	toggle := e.cfg.ToggleKey

	run(t, e, src,
		keys.Event{Code: toggle, State: keys.Down},
		keys.Event{Code: toggle, State: keys.Up},
	)

	if !e.Enabled() {
		t.Fatal("toggle key should have enabled broadcasting")
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("toggle key leaked to injector: %v", got)
	}
}

func assertLog(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d calls %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}
