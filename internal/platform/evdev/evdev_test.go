package evdev

import (
	"testing"
	"time"

	ev "github.com/holoplot/go-evdev"

	"github.com/malahmen/clonecast/internal/keys"
)

func newSource() *Source {
	return &Source{lastSrc: make(map[dupKey]dupSeen)}
}

// A physical keyboard exposing two event nodes delivers each press twice; the
// second must be dropped.
func TestDuplicateFromOtherDeviceIsDropped(t *testing.T) {
	s := newSource()
	a, b := &ev.InputDevice{}, &ev.InputDevice{}
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
	s := newSource()
	a, b, c := &ev.InputDevice{}, &ev.InputDevice{}, &ev.InputDevice{}
	k := dupKey{code: ev.KEY_A, state: keys.State(1)}
	_ = s.duplicate(a, k)
	if !s.duplicate(b, k) || !s.duplicate(c, k) {
		t.Fatal("a third node's copy of the same press was not suppressed")
	}
}

// The single-device case must be untouched: repeated presses from one device
// are real presses, however fast.
func TestSameDeviceNeverSuppressed(t *testing.T) {
	s := newSource()
	a := &ev.InputDevice{}
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
	s := newSource()
	a, b := &ev.InputDevice{}, &ev.InputDevice{}
	k := dupKey{code: ev.KEY_SPACE, state: keys.State(1)}
	_ = s.duplicate(a, k)
	time.Sleep(dupWindow + 5*time.Millisecond)
	if s.duplicate(b, k) {
		t.Fatal("event after the window was treated as a duplicate")
	}
}

// Different keys, and key-up versus key-down, are independent.
func TestDifferentKeyAndStateAreIndependent(t *testing.T) {
	s := newSource()
	a, b := &ev.InputDevice{}, &ev.InputDevice{}
	_ = s.duplicate(a, dupKey{code: ev.KEY_A, state: keys.State(1)})
	if s.duplicate(b, dupKey{code: ev.KEY_B, state: keys.State(1)}) {
		t.Fatal("a different key was treated as a duplicate")
	}
	if s.duplicate(b, dupKey{code: ev.KEY_A, state: keys.State(0)}) {
		t.Fatal("key-up was treated as a duplicate of key-down")
	}
}
