//go:build linux

package kwin

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// fakeKWin is KWin's scripting objects as a test needs them: loadScript and
// run answer, or hang until the call's context ends (or the test does, for a
// call made without one). It never pushes or replies on its own.
type fakeKWin struct {
	mu        sync.Mutex
	hang      map[string]bool // method name -> never answers
	calls     map[string]int  // method name -> calls
	inFlight  int             // calls not yet answered
	watchLoad int             // loadScript calls for the persistent script
	done      chan struct{}   // closed at test end, releasing hung calls
}

func newFakeKWin(t *testing.T, hang ...string) *fakeKWin {
	f := &fakeKWin{hang: map[string]bool{}, calls: map[string]int{}, done: make(chan struct{})}
	for _, m := range hang {
		f.hang[m] = true
	}
	t.Cleanup(func() { close(f.done) })
	return f
}

func (f *fakeKWin) wm(t *testing.T) *WM {
	w := &WM{pending: make(chan kwinReply, 8), tmpDir: t.TempDir()}
	w.object = func(path dbus.ObjectPath) dbus.BusObject { return fakeObject{f, path} }
	return w
}

func (f *fakeKWin) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *fakeKWin) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inFlight
}

func (f *fakeKWin) watchLoads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.watchLoad
}

func (f *fakeKWin) answer(ctx context.Context, method string, args ...interface{}) *dbus.Call {
	m := method[strings.LastIndex(method, ".")+1:]
	f.mu.Lock()
	f.calls[m]++
	if m == "loadScript" && len(args) == 2 && args[1] == watchPlugin {
		f.watchLoad++
	}
	hang := f.hang[m]
	f.inFlight++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	if hang {
		select {
		case <-ctx.Done():
			return &dbus.Call{Err: ctx.Err()}
		case <-f.done:
			return &dbus.Call{Err: context.Canceled}
		}
	}
	if m == "loadScript" {
		return &dbus.Call{Body: []interface{}{int32(7)}}
	}
	return &dbus.Call{}
}

// fakeObject implements only what the kwin package calls; the embedded nil
// interface panics on anything else.
type fakeObject struct {
	f    *fakeKWin
	path dbus.ObjectPath
}

func (o fakeObject) Call(method string, _ dbus.Flags, args ...interface{}) *dbus.Call {
	return o.f.answer(context.Background(), method, args...)
}

func (o fakeObject) CallWithContext(ctx context.Context, method string, _ dbus.Flags, args ...interface{}) *dbus.Call {
	return o.f.answer(ctx, method, args...)
}

func (o fakeObject) Go(method string, _ dbus.Flags, ch chan *dbus.Call, args ...interface{}) *dbus.Call {
	return o.GoWithContext(context.Background(), method, 0, ch, args...)
}

func (o fakeObject) GoWithContext(ctx context.Context, method string, _ dbus.Flags, ch chan *dbus.Call, args ...interface{}) *dbus.Call {
	go func() {
		c := o.f.answer(ctx, method, args...)
		if ch != nil {
			ch <- c
		}
	}()
	return &dbus.Call{}
}

func (o fakeObject) AddMatchSignal(string, string, ...dbus.MatchOption) *dbus.Call { panic("unused") }
func (o fakeObject) RemoveMatchSignal(string, string, ...dbus.MatchOption) *dbus.Call {
	panic("unused")
}
func (o fakeObject) GetProperty(string) (dbus.Variant, error) { panic("unused") }
func (o fakeObject) StoreProperty(string, interface{}) error  { panic("unused") }
func (o fakeObject) SetProperty(string, interface{}) error    { panic("unused") }
func (o fakeObject) Destination() string                      { return kwinService }
func (o fakeObject) Path() dbus.ObjectPath                    { return o.path }

// activeWithin calls Active with a ctx of budget and fails if it has not
// returned within limit: a call that ignores its context runs to the D-Bus
// timeout, and Active runs on the engine goroutine.
func activeWithin(t *testing.T, w *WM, budget, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		_, _ = w.Active(ctx)
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("Active did not return within %v of a %v context: a KWin call ignores it", limit, budget)
	}
}

// Kamino 139: a hung KWin (restarting, say) must cost Active its context, not
// the D-Bus timeout. Reloading the watch script after it stopped is the path
// that used no context at all.
func TestActiveHonoursItsContextWhenKWinHangs(t *testing.T) {
	for _, m := range []string{"unloadScript", "loadScript", "run"} {
		t.Run(m, func(t *testing.T) {
			f := newFakeKWin(t, m)
			activeWithin(t, f.wm(t), 100*time.Millisecond, 2*time.Second)
		})
	}
}

// Kamino 139: the script answers and runs, then no reply comes, so Active
// gives up at its deadline. The unload run defers must not then hold it for
// a call of its own, and that unload still ends at its own deadline rather
// than leaking a goroutine per hung call.
func TestRunDoesNotWaitForAHungUnload(t *testing.T) {
	old := kwinCallTimeout
	kwinCallTimeout = 200 * time.Millisecond
	t.Cleanup(func() { kwinCallTimeout = old })
	f := newFakeKWin(t, "unloadScript")
	w := f.wm(t)
	w.watch.loaded, w.watch.tried = true, time.Now() // only run's own calls
	activeWithin(t, w, 100*time.Millisecond, 2*time.Second)
	if f.count("loadScript") != 1 {
		t.Fatalf("loadScript called %d times, want 1 (run's)", f.count("loadScript"))
	}
	for deadline := time.Now().Add(2 * time.Second); f.pending() > 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the unload is still waiting on KWin after 2s; it has no deadline of its own")
		}
	}
}

// Kamino 139: the watch script loading is not the watch working. Until a
// push arrives, a later Active must try it again; after one, it must not.
func TestWatchRetriesUntilTheFirstPush(t *testing.T) {
	f := newFakeKWin(t)
	w := f.wm(t)
	activeWithin(t, w, 50*time.Millisecond, 2*time.Second)
	if f.watchLoads() != 1 {
		t.Fatalf("watch script loaded %d times, want 1", f.watchLoads())
	}

	// Loaded, but nothing pushed that decodes. Past rewatchEvery it is
	// tried again.
	_ = (&callback{w}).Result(watchSeq, `not json`)
	w.watch.mu.Lock()
	w.watch.tried = time.Now().Add(-2 * rewatchEvery)
	w.watch.mu.Unlock()
	activeWithin(t, w, 50*time.Millisecond, 2*time.Second)
	if f.watchLoads() != 2 {
		t.Fatalf("watch script loaded %d times after no push, want 2: a failed first push never retries", f.watchLoads())
	}

	// The first push lands, so the script works. A later payload that does
	// not decode empties the cache (Active asks KWin directly), but reloading
	// a working script would not mend it.
	cb := &callback{w}
	_ = cb.Result(watchSeq, `{"active":{"id":"{a}","pid":1},"windows":[]}`)
	_ = cb.Result(watchSeq, `not json`)
	w.watch.mu.Lock()
	w.watch.tried = time.Now().Add(-2 * rewatchEvery)
	w.watch.mu.Unlock()
	activeWithin(t, w, 50*time.Millisecond, 2*time.Second)
	if f.watchLoads() != 2 {
		t.Fatalf("watch script loaded %d times after a push, want still 2", f.watchLoads())
	}
}
