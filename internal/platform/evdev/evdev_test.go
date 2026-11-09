//go:build linux

package evdev

import (
	"os"
	"strings"
	"testing"
	"time"

	ev "github.com/holoplot/go-evdev"
	"golang.org/x/sys/unix"

	"github.com/malahmen/clonecast/internal/keys"
)

func dupSource() *Source {
	return &Source{lastSrc: make(map[dupKey]dupSeen)}
}

// A physical keyboard exposing two event nodes delivers each press twice; the
// second must be dropped.
func TestDuplicateFromOtherDeviceIsDropped(t *testing.T) {
	s := dupSource()
	a, b := 3, 4
	k := dupKey{code: ev.KEY_ENTER, state: keys.State(1)}

	if s.duplicate(a, k) {
		t.Fatal("first event from device A was treated as a duplicate")
	}
	if !s.duplicate(b, k) {
		t.Fatal("same key from device B within the window was not treated as a duplicate")
	}
	if got := s.Coalesced(); got != 1 {
		t.Fatalf("Coalesced() = %d, want 1", got)
	}
}

// A third node reporting the same press must also be suppressed, not revive it.
func TestThirdNodeAlsoDropped(t *testing.T) {
	s := dupSource()
	a, b, c := 3, 4, 5
	k := dupKey{code: ev.KEY_A, state: keys.State(1)}
	_ = s.duplicate(a, k)
	if !s.duplicate(b, k) || !s.duplicate(c, k) {
		t.Fatal("a third node's copy of the same press was not suppressed")
	}
}

// The single-device case must be untouched: repeated presses from one device
// are real presses, however fast.
func TestSameDeviceNeverSuppressed(t *testing.T) {
	s := dupSource()
	a := 3
	k := dupKey{code: ev.KEY_J, state: keys.State(1)}
	for i := 0; i < 5; i++ {
		if s.duplicate(a, k) {
			t.Fatalf("press %d from the same device was suppressed", i+1)
		}
	}
	if got := s.Coalesced(); got != 0 {
		t.Fatalf("Coalesced() = %d, want 0", got)
	}
}

// Beyond the window it is a second press, not a duplicate.
func TestOutsideWindowIsNotDuplicate(t *testing.T) {
	s := dupSource()
	a, b := 3, 4
	k := dupKey{code: ev.KEY_SPACE, state: keys.State(1)}
	_ = s.duplicate(a, k)
	time.Sleep(dupWindow + 5*time.Millisecond)
	if s.duplicate(b, k) {
		t.Fatal("event after the window was treated as a duplicate")
	}
}

// Different keys, and key-up versus key-down, are independent.
func TestDifferentKeyAndStateAreIndependent(t *testing.T) {
	s := dupSource()
	a, b := 3, 4
	_ = s.duplicate(a, dupKey{code: ev.KEY_A, state: keys.State(1)})
	if s.duplicate(b, dupKey{code: ev.KEY_B, state: keys.State(1)}) {
		t.Fatal("a different key was treated as a duplicate")
	}
	if s.duplicate(b, dupKey{code: ev.KEY_A, state: keys.State(0)}) {
		t.Fatal("key-up was treated as a duplicate of key-down")
	}
}

// fifoDev is a grabbed device for a Source over a FIFO: the FIFO stands in for
// the event node's fd, and the fakeDev for its key state and grab.
type fifoDev struct {
	*fakeDev
	fd int
}

func (f fifoDev) Close() error { return unix.Close(f.fd) }

// fifoSource starts a Source reading the FIFOs at paths, each with its fakeDev
// (nil: no key is ever held). The writers it returns are open, so a reader
// sees a hang-up only when the test closes one.
func fifoSource(t *testing.T, lost func(string, error, int), paths []string, devs ...*fakeDev) (*Source, []*os.File) {
	t.Helper()
	s := newSource(lost)
	var ws []*os.File
	for i, p := range paths {
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		w, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Close() })
		ws = append(ws, w)
		d := &fakeDev{held: []int{0}}
		if i < len(devs) && devs[i] != nil {
			d = devs[i]
		}
		s.devs = append(s.devs, captured{path: p, fd: fd, dev: fifoDev{fakeDev: d, fd: fd}})
		s.paths = append(s.paths, p)
	}
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	return s, ws
}

func recv(t *testing.T, ch <-chan keys.Event, within time.Duration) (keys.Event, bool) {
	t.Helper()
	select {
	case e := <-ch:
		return e, true
	case <-time.After(within):
		return keys.Event{}, false
	}
}

func TestSourceForwardsOnlyKeyEvents(t *testing.T) {
	p := fifos(t, 1)
	s, w := fifoSource(t, nil, p)
	defer func() { _ = s.Close() }()
	for _, r := range [][]byte{event(4, 4, 7), event(1, uint16(ev.KEY_W), 1), event(0, 0, 0)} {
		_, _ = w[0].Write(r)
	}
	if e, ok := recv(t, s.Events(), time.Second); !ok || e != (keys.Event{Code: ev.KEY_W, State: keys.Down}) {
		t.Fatalf("got %v (%v), want W down", e, ok)
	}
	if e, ok := recv(t, s.Events(), 100*time.Millisecond); ok {
		t.Fatalf("got %v, want nothing: only EV_KEY is forwarded", e)
	}
}

// Kamino 186: Close must wake a reader waiting on a quiet device, so that no
// reader is left to forward the next event of a device already released.
func TestSourceCloseWakesAWaitingReader(t *testing.T) {
	p := fifos(t, 2)
	s, _ := fifoSource(t, nil, p)
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return: a reader waiting on a quiet device was not woken")
	}
	if _, open := <-s.Events(); open {
		t.Fatal("Events still open after Close: a reader is still running")
	}
}

// Kamino 202: an unplugged keyboard (or one asleep) is reported, and the keys
// it held are released, since their releases will never come.
func TestSourceReportsALostDeviceAndReleasesItsKeys(t *testing.T) {
	p := fifos(t, 2)
	type loss struct {
		path string
		left int
	}
	lost := make(chan loss, 2)
	s, w := fifoSource(t, func(path string, _ error, left int) { lost <- loss{path, left} }, p)
	defer func() { _ = s.Close() }()
	_, _ = w[0].Write(event(1, uint16(ev.KEY_W), 1))
	if e, ok := recv(t, s.Events(), time.Second); !ok || e.State != keys.Down {
		t.Fatalf("got %v (%v), want W down", e, ok)
	}
	_ = w[0].Close() // the device goes away
	if e, ok := recv(t, s.Events(), time.Second); !ok || e != (keys.Event{Code: ev.KEY_W, State: keys.Up}) {
		t.Fatalf("got %v (%v), want W released", e, ok)
	}
	select {
	case l := <-lost:
		if l.path != p[0] || l.left != 1 {
			t.Fatalf("lost %+v, want %s with 1 left", l, p[0])
		}
	case <-time.After(time.Second):
		t.Fatal("the lost device was not reported")
	}
	if s.Lost() != 1 {
		t.Fatalf("Lost() = %d, want 1", s.Lost())
	}
}

// Kamino 138: the wait before a release reads every grabbed device.
func TestSourceKeysUpWaitsForEveryDevice(t *testing.T) {
	p := fifos(t, 2)
	a, b := &fakeDev{held: []int{0}}, &fakeDev{held: []int{1, 1, 0}}
	s, _ := fifoSource(t, nil, p, a, b)
	defer func() { _ = s.Close() }()
	if err := s.KeysUp(time.Second); err != nil {
		t.Fatal(err)
	}
	if b.calls < 3 {
		t.Fatalf("second device read %d times, want until its key went up", b.calls)
	}
	c := &fakeDev{held: []int{2}}
	s2, _ := fifoSource(t, nil, fifos(t, 1), c)
	defer func() { _ = s2.Close() }()
	if err := s2.KeysUp(50 * time.Millisecond); err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("err = %v, want a still-held error", err)
	}
}

// Kamino 202: the virtual keyboard has the keys of every candidate, not just
// the first device's, and no mouse buttons from a macro interface.
func TestVirtualKeyboardHasEveryCandidatesKeys(t *testing.T) {
	tmpl := map[ev.EvType][]ev.EvCode{
		ev.EV_SYN: {0},
		ev.EV_KEY: {ev.KEY_A, ev.KEY_B},
		ev.EV_MSC: {ev.MSC_SCAN},
	}
	got := withKeys(tmpl, [][]ev.EvCode{
		{ev.KEY_B, ev.KEY_F13, ev.BTN_LEFT},
		{ev.KEY_KP1, ev.BTN_TRIGGER_HAPPY1, ev.KEY_OK},
	})
	want := map[ev.EvCode]bool{ev.KEY_A: true, ev.KEY_B: true, ev.KEY_F13: true, ev.KEY_KP1: true, ev.KEY_OK: true}
	if len(got[ev.EV_KEY]) != len(want) {
		t.Fatalf("EV_KEY = %v, want each of %v once", got[ev.EV_KEY], want)
	}
	for _, c := range got[ev.EV_KEY] {
		if !want[c] {
			t.Fatalf("EV_KEY has %v, want only %v", c, want)
		}
	}
	if len(got[ev.EV_MSC]) != 1 || len(got[ev.EV_SYN]) != 1 {
		t.Fatalf("other types changed: %v", got)
	}
	if len(tmpl[ev.EV_KEY]) != 2 {
		t.Fatal("the template's capabilities were modified")
	}
}
