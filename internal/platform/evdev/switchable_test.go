package evdev

import (
	"os"
	"testing"
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
