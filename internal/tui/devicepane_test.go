package tui

import (
	"errors"
	"strings"
	"testing"
)

type fakeDevCtl struct {
	devices  []Device
	captured []string
	captures [][]string // every Capture call, in order
	saved    []string
	capErr   error
	listErr  error
}

func (f *fakeDevCtl) Devices() ([]Device, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.devices, nil
}
func (f *fakeDevCtl) Captured() []string { return f.captured }
func (f *fakeDevCtl) Capture(p []string) error {
	f.captures = append(f.captures, p)
	if f.capErr != nil {
		return f.capErr
	}
	f.captured = p
	return nil
}
func (f *fakeDevCtl) Save(p []string) error { f.saved = p; return nil }

func threeDevices() *fakeDevCtl {
	return &fakeDevCtl{
		devices: []Device{
			{Path: "/dev/input/event6", Name: "ASUS Strix"},
			{Path: "/dev/input/event19", Name: "Lofree", Busy: "BUSY (device or resource busy)"},
			{Path: "/dev/input/event20", Name: "keyd virtual keyboard"},
		},
		captured: []string{"/dev/input/event6"},
	}
}

// The list must say what exists AND which of them is live — the thing that was
// impossible to see before this pane.
func TestDevicePaneShowsWhatExistsAndWhatIsCaptured(t *testing.T) {
	ctl := threeDevices()
	p := NewDevicePane(ctl)
	p, _ = p.Update(deviceListMsg{devices: ctl.devices})

	view := p.View(70, 12)
	for _, want := range []string{"event6", "event19", "event20", "(busy)"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view is missing %q:\n%s", want, view)
		}
	}
	// Rows use the short device name; the panel shares a row with the log and
	// cannot afford "/dev/input/" on every line.
	if !strings.Contains(view, "[x] event6") {
		t.Fatalf("the captured device is not ticked:\n%s", view)
	}
	if !strings.Contains(view, "[ ] event20") {
		t.Fatalf("an uncaptured device is not shown unticked:\n%s", view)
	}
	// The full path of the highlighted device still has to be visible
	// somewhere, since that is what --keyboard and the config take.
	if !strings.Contains(view, "/dev/input/event6") {
		t.Fatalf("the highlighted device's full path is missing:\n%s", view)
	}
}

func TestDevicePaneSpaceCapturesAnother(t *testing.T) {
	ctl := threeDevices()
	p := NewDevicePane(ctl)
	p, _ = p.Update(deviceListMsg{devices: ctl.devices})

	p, _ = p.Update(keyRune('j')) // event19
	p, _ = p.Update(keyRune('j')) // event20
	_, cmd := p.Update(keyRune(' '))
	if cmd == nil {
		t.Fatal("space produced no command, want a re-grab")
	}
	cmd() // run the capture

	if len(ctl.captures) != 1 {
		t.Fatalf("Capture called %d times, want 1", len(ctl.captures))
	}
	got := ctl.captures[0]
	if len(got) != 2 || got[0] != "/dev/input/event6" || got[1] != "/dev/input/event20" {
		t.Fatalf("captured %v, want both event6 and event20", got)
	}
}

// Releasing the last one would mean "capture nothing", which further down means
// "discover everything" — the behaviour this pane exists to escape.
func TestDevicePaneRefusesToReleaseTheLastDevice(t *testing.T) {
	ctl := threeDevices()
	p := NewDevicePane(ctl)
	p, _ = p.Update(deviceListMsg{devices: ctl.devices})

	p, cmd := p.Update(keyRune(' ')) // cursor is on the only captured device
	if cmd != nil {
		t.Fatal("releasing the last captured device produced a command, want a refusal")
	}
	if !p.err || !strings.Contains(p.status, "at least one") {
		t.Fatalf("status = %q (err=%v), want a refusal naming the reason", p.status, p.err)
	}
	if len(ctl.captures) != 0 {
		t.Fatalf("Capture was called %d times, want 0", len(ctl.captures))
	}
}

// A failed re-grab must not leave the ticks claiming something that is not live.
func TestDevicePaneFailedCaptureRelists(t *testing.T) {
	ctl := threeDevices()
	ctl.capErr = errors.New("device or resource busy")
	p := NewDevicePane(ctl)
	p, _ = p.Update(deviceListMsg{devices: ctl.devices})

	p, _ = p.Update(deviceCaptureMsg{paths: []string{"/dev/input/event19"}, err: ctl.capErr})
	if !p.err || !strings.Contains(p.status, "busy") {
		t.Fatalf("status = %q (err=%v), want the failure reported", p.status, p.err)
	}
}

func TestDevicePaneSavesCurrentSelection(t *testing.T) {
	ctl := threeDevices()
	p := NewDevicePane(ctl)
	p, _ = p.Update(deviceListMsg{devices: ctl.devices})

	_, cmd := p.Update(keyRune('s'))
	if cmd == nil {
		t.Fatal("s produced no command, want a save")
	}
	cmd()
	if len(ctl.saved) != 1 || ctl.saved[0] != "/dev/input/event6" {
		t.Fatalf("saved %v, want the captured selection", ctl.saved)
	}
}

// With no backend (the mock run) the pane must say so, not crash.
func TestDevicePaneWithoutControllerReportsIt(t *testing.T) {
	p := NewDevicePane(nil)
	msg := p.Refresh()()
	lm, ok := msg.(deviceListMsg)
	if !ok || lm.err == nil {
		t.Fatalf("Refresh with no controller = %#v, want a deviceListMsg carrying an error", msg)
	}
	p, _ = p.Update(lm)
	if !p.err {
		t.Fatal("the missing backend was not reported as an error")
	}
}
