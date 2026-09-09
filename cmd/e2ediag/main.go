//go:build linux

// Command e2ediag drives the real production stack — internal/platform/evdev,
// internal/platform/kwin, and internal/broadcast.Engine, wired exactly as
// cmd/clonecast/platform_linux.go and main.go do it — but feeds it synthetic
// key events through a throwaway uinput device instead of your real
// keyboard, and drives targets/filter/enabled programmatically instead of
// through the TUI.
//
// Built to test what cmd/kwindiag's KWin-only tests (bounce, dance) cannot:
// whether real evdev capture + real uinput passthrough + the real engine,
// end to end, actually deliver keys — as opposed to whether KWin's own
// focus-switching alone is reliable. Every KWin-only test so far has come
// back clean, which points at either a regression somewhere in the
// passthrough/injection path, or something specific to genuine key-event
// timing that isolated Activate() calls don't exercise.
//
// Usage:
//
//	e2ediag <origin-id> <target-id> [rounds] [gapMs] [key]
//
// origin-id must be the window currently focused when this starts (it
// becomes "origin" from the engine's point of view). target-id is the
// broadcast target. rounds (default 5) keystrokes of key (default "space" —
// makes a WoW character jump, so success/failure is visible on the actual
// game client, not just inferred from logs) are injected, one every gapMs
// (default 300, i.e. roughly real ability-rotation pace), with broadcasting
// enabled and the filter set to {key} only.
//
// Requires the real input/uinput permissions clonecast itself needs (see
// scripts/setup-bazzite.sh) and a KWin Scripting session.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	ev "github.com/holoplot/go-evdev"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
	"github.com/malahmen/clonecast/internal/platform/evdev"
	"github.com/malahmen/clonecast/internal/platform/kwin"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func findDeviceByName(name string) (string, error) {
	paths, err := ev.ListDevicePaths()
	if err != nil {
		return "", err
	}
	for _, p := range paths {
		if p.Name == name {
			return p.Path, nil
		}
	}
	return "", fmt.Errorf("no /dev/input device named %q found", name)
}

// x11ActiveName is the _NET_ACTIVE_WINDOW hint (what the WM/compositor
// reports as active) — what activateViaXdotool's windowactivate sets.
func x11ActiveName() string {
	out, err := exec.Command("xdotool", "getactivewindow", "getwindowname").Output()
	if err != nil {
		return "ERR:" + err.Error()
	}
	return strings.TrimSpace(string(out))
}

// x11FocusName is the real XGetInputFocus target — what windowfocus sets.
// Printed alongside x11ActiveName purely as a diagnostic: REFERENCE.md 4.12
// found real key delivery failed even when this and x11ActiveName agreed on
// the target, meaning neither X11-level signal actually proves a genuine
// evdev/uinput event will reach the window — only watching the game itself
// does.
func x11FocusName() string {
	out, err := exec.Command("xdotool", "getwindowfocus", "getwindowname").Output()
	if err != nil {
		return "ERR:" + err.Error()
	}
	return strings.TrimSpace(string(out))
}

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: e2ediag <origin-id> <target-id> [rounds] [gapMs]")
		os.Exit(2)
	}
	// origin-id is informational only (printed below so you can confirm it
	// against real X11 focus) — the engine determines "origin" dynamically
	// via wm.Active() at broadcast time, same as production.
	originID := os.Args[1]
	targetID := broadcast.WindowID(os.Args[2])
	rounds := 5
	if len(os.Args) > 3 {
		fmt.Sscanf(os.Args[3], "%d", &rounds)
	}
	gapMs := 300
	if len(os.Args) > 4 {
		fmt.Sscanf(os.Args[4], "%d", &gapMs)
	}
	keyName := "space"
	if len(os.Args) > 5 {
		keyName = os.Args[5]
	}
	keyCode, err := keys.Parse(keyName)
	if err != nil {
		fatalf("bad key %q: %v", keyName, err)
	}

	// --- create a throwaway synthetic keyboard, distinct from any real one ---
	id := ev.InputID{BusType: 0x03, Vendor: 0x1234, Product: 0x5678, Version: 1}
	caps := map[ev.EvType][]ev.EvCode{
		ev.EV_KEY: {keyCode, ev.KEY_ENTER, ev.KEY_SCROLLLOCK},
	}
	const fakeName = "e2ediag fake keyboard"
	fakeKbd, err := ev.CreateDevice(fakeName, id, caps)
	if err != nil {
		fatalf("create fake keyboard: %v (is /dev/uinput writable? see scripts/setup-bazzite.sh)", err)
	}
	defer ev.DestroyDevice(fakeKbd)
	time.Sleep(500 * time.Millisecond) // let udev/evdev notice it before we look for its /dev/input/eventN

	// CreateDevice's own Path() is just /dev/uinput (the control node used to
	// create it), not the resulting readable event device — find that the
	// same way evdev.Discover() finds any keyboard: by scanning device names.
	fakeKbdPath, err := findDeviceByName(fakeName)
	if err != nil {
		fatalf("find fake keyboard's /dev/input node: %v", err)
	}
	fmt.Println("fake keyboard created at", fakeKbdPath)

	// --- wire the real production stack, pointed only at the fake device ---
	src, err := evdev.Open(fakeKbdPath)
	if err != nil {
		fatalf("evdev.Open(fake device): %v", err)
	}
	defer src.Close()

	inj, err := evdev.NewInjector(src.Devices()[0])
	if err != nil {
		fatalf("evdev.NewInjector: %v", err)
	}
	defer inj.Close()
	time.Sleep(500 * time.Millisecond) // same wait cmd/clonecast does before first passthrough

	wm, err := kwin.New()
	if err != nil {
		fatalf("kwin.New: %v", err)
	}
	defer wm.Close()

	cfg := broadcast.DefaultConfig()
	engine := broadcast.New(src, inj, wm, cfg)
	engine.SetTargets([]broadcast.WindowID{targetID})
	engine.SetFilter(keys.Of(keyCode))
	engine.SetEnabled(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engineErr := make(chan error, 1)
	go func() { engineErr <- engine.Run(ctx) }()

	go func() {
		for n := range engine.Notices() {
			tag := "info"
			if n.Err {
				tag = "ERROR"
			}
			fmt.Printf("[engine %s] %s\n", tag, n.Text)
		}
	}()

	// Make sure we actually start on origin, per real X11 focus, not just
	// by assertion. originID (the arg you passed) is only a label here —
	// compare it by eye against what X11 actually reports.
	fmt.Println("expected origin id (informational):", originID)
	if got := x11ActiveName(); strings.HasPrefix(got, "ERR:") {
		fmt.Println("warning: could not read initial X11 active window (is xdotool installed?)")
	} else {
		fmt.Println("real X11 active window at test start:", got)
	}

	for i := 0; i < rounds; i++ {
		fmt.Printf("\n--- round %d: injecting %q via fake keyboard ---\n", i, keyName)
		fmt.Printf("before: active=%q focus=%q\n", x11ActiveName(), x11FocusName())

		if err := fakeKbd.WriteOne(&ev.InputEvent{Type: ev.EV_KEY, Code: keyCode, Value: 1}); err != nil {
			fatalf("write key down: %v", err)
		}
		if err := fakeKbd.WriteOne(&ev.InputEvent{Type: ev.EV_SYN, Code: ev.SYN_REPORT, Value: 0}); err != nil {
			fatalf("write syn: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := fakeKbd.WriteOne(&ev.InputEvent{Type: ev.EV_KEY, Code: keyCode, Value: 0}); err != nil {
			fatalf("write key up: %v", err)
		}
		if err := fakeKbd.WriteOne(&ev.InputEvent{Type: ev.EV_SYN, Code: ev.SYN_REPORT, Value: 0}); err != nil {
			fatalf("write syn: %v", err)
		}

		// Give the engine time to run the full dance (settle*targets plus
		// KWin round-trips) before we check anything.
		time.Sleep(300 * time.Millisecond)
		fmt.Printf("after-settle: active=%q focus=%q (should be back to origin)\n", x11ActiveName(), x11FocusName())

		time.Sleep(time.Duration(gapMs) * time.Millisecond)
	}

	cancel()
	select {
	case <-engineErr:
	case <-time.After(time.Second):
	}
	fmt.Printf("\ndone. Watch the target client for whether %q actually landed, and the [engine] lines above for what it believed happened.\n", keyName)
}
