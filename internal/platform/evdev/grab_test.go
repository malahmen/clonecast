//go:build linux

package evdev

import (
	"strings"
	"testing"
	"time"

	ev "github.com/holoplot/go-evdev"
)

// fakeDev reports the held keys from a script, one entry per State call
// (the last entry repeats), and records grabs.
type fakeDev struct {
	held    []int
	calls   int
	grabbed bool
	grabs   int
	ungrabs int
}

func (f *fakeDev) State(ev.EvType) (ev.StateMap, error) {
	n := f.held[min(f.calls, len(f.held)-1)]
	f.calls++
	st := ev.StateMap{}
	for i := 0; i < n; i++ {
		st[ev.EvCode(i+1)] = true
	}
	return st, nil
}
func (f *fakeDev) Grab() error   { f.grabbed = true; f.grabs++; return nil }
func (f *fakeDev) Ungrab() error { f.grabbed = false; f.ungrabs++; return nil }

// The Shift pressed for the picker is still down when the grab comes: the
// grab must wait for it, or the compositor never sees it released.
func TestGrabWaitsForHeldKeys(t *testing.T) {
	f := &fakeDev{held: []int{1, 1, 0}}
	if err := grabKeysUp(f, time.Second); err != nil {
		t.Fatal(err)
	}
	if !f.grabbed || f.grabs != 1 {
		t.Fatalf("grabbed=%v grabs=%d, want one grab after the key went up", f.grabbed, f.grabs)
	}
}

// A key that goes down between the check and the grab: ungrab so its release
// reaches the compositor, then try again.
func TestGrabRetriesWhenAKeyWentDownDuringIt(t *testing.T) {
	f := &fakeDev{held: []int{0, 1, 0, 0}}
	if err := grabKeysUp(f, time.Second); err != nil {
		t.Fatal(err)
	}
	if !f.grabbed || f.grabs != 2 || f.ungrabs != 1 {
		t.Fatalf("grabbed=%v grabs=%d ungrabs=%d, want grab, ungrab, grab", f.grabbed, f.grabs, f.ungrabs)
	}
}

func TestGrabGivesUpWhileAKeyStaysHeld(t *testing.T) {
	f := &fakeDev{held: []int{1}}
	err := grabKeysUp(f, 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "still held") {
		t.Fatalf("err = %v, want a still-held error", err)
	}
	if f.grabbed {
		t.Fatal("left the device grabbed")
	}
}
