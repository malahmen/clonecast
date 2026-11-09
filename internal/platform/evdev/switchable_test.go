//go:build linux

package evdev

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ev "github.com/holoplot/go-evdev"

	"github.com/malahmen/clonecast/internal/keys"
)

// Harmless devices to grab in a test: a power button and a video bus produce
// nothing on their own and no desktop depends on them, unlike a keyboard.
const (
	devA = "/dev/input/event0"
	devB = "/dev/input/event1"
)

func requireGrabbable(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not present", p)
		}
		if err := CanGrab(p); err != nil {
			t.Skipf("%s not grabbable here (%v)", p, err)
		}
	}
}

func TestSwitchableKeepsOneChannelAcrossSwitches(t *testing.T) {
	requireGrabbable(t, devA, devB)

	s, err := OpenSwitchable(devA)
	if err != nil {
		t.Fatalf("OpenSwitchable: %v", err)
	}
	defer func() { _ = s.Close() }()

	before := s.Events()
	if got := s.Paths(); len(got) != 1 || got[0] != devA {
		t.Fatalf("Paths() = %v, want [%s]", got, devA)
	}

	if err := s.Switch(devB); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	if got := s.Paths(); len(got) != 1 || got[0] != devB {
		t.Fatalf("after Switch, Paths() = %v, want [%s]", got, devB)
	}
	// The engine selects on this channel; a Switch must not replace it.
	if s.Events() != before {
		t.Fatal("Events() returned a different channel after Switch")
	}
}

func TestSwitchableFailedSwitchKeepsPreviousCapture(t *testing.T) {
	requireGrabbable(t, devA)

	s, err := OpenSwitchable(devA)
	if err != nil {
		t.Fatalf("OpenSwitchable: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Switch("/dev/input/event9999"); err == nil {
		t.Fatal("switching to a device that does not exist succeeded, want an error")
	}
	// The important half: a bad choice must not leave the engine deaf.
	if got := s.Paths(); len(got) != 1 || got[0] != devA {
		t.Fatalf("after a failed Switch, Paths() = %v, want the previous [%s]", got, devA)
	}
}

func TestSwitchableReleasesTheOldDeviceBeforeReturning(t *testing.T) {
	requireGrabbable(t, devA, devB)

	s, err := OpenSwitchable(devA)
	if err != nil {
		t.Fatalf("OpenSwitchable: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Switch(devB); err != nil {
		t.Fatalf("Switch: %v", err)
	}
	// devA must be free again the moment Switch returns, or switching back to
	// it would fail against our own lingering grab.
	if err := CanGrab(devA); err != nil {
		t.Fatalf("previous device still held after Switch returned: %v", err)
	}
	if err := s.Switch(devA); err != nil {
		t.Fatalf("switching back to the first device: %v", err)
	}
}

func TestSwitchableCloseReleasesAndRefusesFurtherSwitches(t *testing.T) {
	requireGrabbable(t, devA)

	s, err := OpenSwitchable(devA)
	if err != nil {
		t.Fatalf("OpenSwitchable: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The device must be free: that is what Close is for.
	if err := CanGrab(devA); err != nil {
		t.Fatalf("device still held after Close: %v", err)
	}
	if err := s.Switch(devA); err == nil {
		t.Fatal("Switch after Close succeeded, want an error")
	}
}

// Re-selecting exactly what is already captured must not fight this process's
// own grab — the overlap case that made a naive open-then-close fail.
func TestSwitchableToSameDeviceIsANoOp(t *testing.T) {
	requireGrabbable(t, devA)

	s, err := OpenSwitchable(devA)
	if err != nil {
		t.Fatalf("OpenSwitchable: %v", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.Switch(devA); err != nil {
		t.Fatalf("switching to the already-captured device: %v", err)
	}
	if got := s.Paths(); len(got) != 1 || got[0] != devA {
		t.Fatalf("Paths() = %v, want [%s]", got, devA)
	}
}

// fakeCapture is a capture whose events the test sends itself.
type fakeCapture struct {
	paths  []string
	ch     chan keys.Event
	closed atomic.Bool
}

func (f *fakeCapture) Events() <-chan keys.Event  { return f.ch }
func (f *fakeCapture) Paths() []string            { return f.paths }
func (f *fakeCapture) Skipped() []string          { return nil }
func (f *fakeCapture) KeysUp(time.Duration) error { return nil }
func (f *fakeCapture) Lost() int                  { return 0 }
func (f *fakeCapture) Close() error               { f.closed.Store(true); return nil }

// fakeSwitchable opens a new fakeCapture per Switch, kept by path.
func fakeSwitchable() (*Switchable, map[string]*fakeCapture) {
	opened := map[string]*fakeCapture{}
	s := newSwitchable(func(p []string) (capture, error) {
		f := &fakeCapture{paths: p, ch: make(chan keys.Event, 8)}
		opened[p[0]] = f
		return f, nil
	})
	return s, opened
}

// fifoSwitchable opens a Source over a FIFO per Switch, with the fakeDev the
// test gave for that path, and reports losses as OpenSwitchable does.
func fifoSwitchable(t *testing.T, devs map[string]*fakeDev) (*Switchable, map[string][]*os.File) {
	t.Helper()
	writers := map[string][]*os.File{}
	var s *Switchable
	s = newSwitchable(func(p []string) (capture, error) {
		src, w := fifoSource(t, s.deviceLost, p, devs[p[0]])
		writers[p[0]] = w
		return src, nil
	})
	s.keysUp = time.Second
	return s, writers
}

// Kamino 138: the Keyboards pane switches on the press of Enter; releasing the
// old keyboard before Enter comes up sends its release to the compositor and
// leaves the virtual Enter down.
func TestSwitchWaitsForHeldKeysBeforeUngrabbing(t *testing.T) {
	p := fifos(t, 2)
	old := &fakeDev{held: []int{1, 1, 1, 0}}
	s, _ := fifoSwitchable(t, map[string]*fakeDev{p[0]: old})
	defer func() { _ = s.Close() }()
	if err := s.Switch(p[0]); err != nil {
		t.Fatal(err)
	}
	if err := s.Switch(p[1]); err != nil {
		t.Fatal(err)
	}
	if old.ungrabs != 1 || old.ungrabAt < 4 {
		t.Fatalf("ungrabs=%d after %d state reads; want one ungrab once no key was down (4 reads)", old.ungrabs, old.ungrabAt)
	}
}

func TestSwitchRefusedWhileAKeyStaysHeld(t *testing.T) {
	p := fifos(t, 2)
	old := &fakeDev{held: []int{1}}
	s, _ := fifoSwitchable(t, map[string]*fakeDev{p[0]: old})
	s.keysUp = 50 * time.Millisecond
	defer func() { _ = s.Close() }()
	if err := s.Switch(p[0]); err != nil {
		t.Fatal(err)
	}
	err := s.Switch(p[1])
	if err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("err = %v, want a still-held refusal", err)
	}
	if old.ungrabs != 0 {
		t.Fatal("the keyboard was released with a key down")
	}
	if got := s.Paths(); len(got) != 1 || got[0] != p[0] {
		t.Fatalf("Paths() = %v, want the old keyboard kept", got)
	}
}

func TestCloseWaitsForHeldKeysThenReleasesAnyway(t *testing.T) {
	p := fifos(t, 2)
	soon, never := &fakeDev{held: []int{1, 1, 0}}, &fakeDev{held: []int{1}}
	s, _ := fifoSwitchable(t, map[string]*fakeDev{p[0]: soon, p[1]: never})
	if err := s.Switch(p[0]); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if soon.ungrabs != 1 || soon.ungrabAt < 3 {
		t.Fatalf("ungrabs=%d after %d state reads; want one ungrab once the key went up", soon.ungrabs, soon.ungrabAt)
	}

	s2, _ := fifoSwitchable(t, map[string]*fakeDev{p[1]: never})
	s2.keysUp = 50 * time.Millisecond
	if err := s2.Switch(p[1]); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	if never.ungrabs != 1 {
		t.Fatal("Close did not release a keyboard with a key stuck down: quitting must not be refused")
	}
}

// Kamino 186: once a capture is switched away from, nothing it still produces
// is forwarded. The probe: the old reader forwarded KEY_W down after the
// switch, and its release went to the compositor.
func TestRetiredCaptureForwardsNothing(t *testing.T) {
	s, opened := fakeSwitchable()
	defer func() { _ = s.Close() }()
	if err := s.Switch("A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Switch("B"); err != nil {
		t.Fatal(err)
	}
	if !opened["A"].closed.Load() {
		t.Fatal("the old capture was not closed")
	}
	opened["A"].ch <- keys.Event{Code: ev.KEY_W, State: keys.Down}
	if e, ok := recv(t, s.Events(), 200*time.Millisecond); ok {
		t.Fatalf("forwarded %v from the retired capture", e)
	}
	opened["B"].ch <- keys.Event{Code: ev.KEY_E, State: keys.Down}
	if e, ok := recv(t, s.Events(), time.Second); !ok || e.Code != ev.KEY_E {
		t.Fatalf("got %v (%v), want the new capture's E", e, ok)
	}
}

// A release still in flight when the old capture is retired would be dropped:
// the switch releases whatever that capture pressed and did not release.
func TestSwitchReleasesWhatTheOldCaptureLeftDown(t *testing.T) {
	s, opened := fakeSwitchable()
	defer func() { _ = s.Close() }()
	if err := s.Switch("A"); err != nil {
		t.Fatal(err)
	}
	opened["A"].ch <- keys.Event{Code: ev.KEY_W, State: keys.Down}
	opened["A"].ch <- keys.Event{Code: ev.KEY_Q, State: keys.Down}
	opened["A"].ch <- keys.Event{Code: ev.KEY_Q, State: keys.Up}
	for i := 0; i < 3; i++ {
		if _, ok := recv(t, s.Events(), time.Second); !ok {
			t.Fatal("the capture's events were not forwarded")
		}
	}
	if err := s.Switch("B"); err != nil {
		t.Fatal(err)
	}
	if e, ok := recv(t, s.Events(), time.Second); !ok || e != (keys.Event{Code: ev.KEY_W, State: keys.Up}) {
		t.Fatalf("got %v (%v), want W released", e, ok)
	}
	if e, ok := recv(t, s.Events(), 100*time.Millisecond); ok {
		t.Fatalf("got %v, want nothing more: Q was released already", e)
	}
}

// Kamino 202: a keyboard going away is a notice, not silence, and choosing it
// again re-grabs it.
func TestLostKeyboardIsANoticeAndCanBeChosenAgain(t *testing.T) {
	p := fifos(t, 1)
	s, writers := fifoSwitchable(t, map[string]*fakeDev{})
	defer func() { _ = s.Close() }()
	notices := make(chan string, 4)
	s.SetNotify(func(text string, isErr bool) {
		if isErr {
			notices <- text
		}
	})
	if err := s.Switch(p[0]); err != nil {
		t.Fatal(err)
	}
	first := writers[p[0]][0]
	_ = first.Close()
	select {
	case n := <-notices:
		if !strings.Contains(n, p[0]) || !strings.Contains(n, "nothing is captured") {
			t.Fatalf("notice %q, want the path and that nothing is captured", n)
		}
	case <-time.After(time.Second):
		t.Fatal("no notice for a keyboard that went away")
	}
	if err := s.Switch(p[0]); err != nil {
		t.Fatal(err)
	}
	if writers[p[0]][0] == first {
		t.Fatal("choosing the lost keyboard again did not re-grab it")
	}
}
