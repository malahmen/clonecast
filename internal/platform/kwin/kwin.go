//go:build linux

// Package kwin implements broadcast.WindowManager for KDE Plasma 6 on
// Wayland, which is what Bazzite ships.
//
// Wayland gives clients no way to enumerate or focus other clients' windows.
// KWin, however, exposes a scripting API over DBus: a JavaScript snippet can
// be loaded, run inside the compositor, and call back out over DBus. clonecast
// uses that exactly as kdotool does: each operation writes a small script to a
// temp file, asks KWin to load and run it, and receives the result on a DBus
// object clonecast exports on the session bus.
//
// This is the slow path (a script load per operation, typically a few
// milliseconds). It is good enough to prove the concept; see REFERENCE.md for
// the planned optimisations.
package kwin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/godbus/dbus/v5"

	"github.com/malahmen/clonecast/internal/broadcast"
)

const (
	kwinService   = "org.kde.KWin"
	scriptingPath = "/Scripting"
	scriptingIfce = "org.kde.kwin.Scripting"
	scriptIfce    = "org.kde.kwin.Script"

	// Our own bus identity, used by the KWin script to call back.
	busName = "org.malahmen.clonecast"
	objPath = "/org/malahmen/clonecast"
	ifce    = "org.malahmen.clonecast"
)

// WM talks to KWin. Create with New, release with Close.
type WM struct {
	conn    *dbus.Conn
	seq     atomic.Int64
	mu      sync.Mutex
	pending chan string
	tmpDir  string

	// haveXdotool caches whether `xdotool` is on PATH, checked once at New.
	// See Activate's doc for what it's used for and why it's optional.
	haveXdotool bool
}

// callback is the object exported on the session bus for the KWin script to
// hand results back. Method names must be exported and end with *dbus.Error.
type callback struct{ wm *WM }

func (c *callback) Result(payload string) *dbus.Error {
	select {
	case c.wm.pending <- payload:
	default:
	}
	return nil
}

// New connects to the session bus and registers the callback object.
func New() (*WM, error) {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf("session bus: %w", err)
	}
	reply, err := conn.RequestName(busName, dbus.NameFlagDoNotQueue)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("request name %s: %w", busName, err)
	}
	if reply != dbus.RequestNameReplyPrimaryOwner {
		_ = conn.Close()
		return nil, fmt.Errorf("%s already owned: is another clonecast running?", busName)
	}
	wm := &WM{conn: conn, pending: make(chan string, 1)}
	if err := conn.Export(&callback{wm}, objPath, ifce); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("export callback: %w", err)
	}
	wm.tmpDir, err = os.MkdirTemp("", "clonecast-kwin-")
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := exec.LookPath("xdotool"); err == nil {
		wm.haveXdotool = true
	}
	return wm, nil
}

// Close releases the bus name and temp files.
func (w *WM) Close() error {
	_, _ = w.conn.ReleaseName(busName)
	_ = os.RemoveAll(w.tmpDir)
	return w.conn.Close()
}

type wire struct {
	ID     string `json:"id"`
	Title  string `json:"caption"`
	Class  string `json:"class"`
	PID    int    `json:"pid"`
	Active bool   `json:"active"`
}

// List returns normal, non-minimised-to-tray windows across all desktops.
func (w *WM) List(ctx context.Context) ([]broadcast.Window, error) {
	out, err := w.run(ctx, `
		const wins = workspace.windowList()
			.filter(w => w.normalWindow && !w.skipTaskbar)
			.map(w => ({
				id: String(w.internalId),
				caption: w.caption,
				class: w.resourceClass,
				pid: w.pid,
				active: w === workspace.activeWindow,
			}));
		reply(JSON.stringify(wins));
	`)
	if err != nil {
		return nil, err
	}
	var raw []wire
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("decode window list: %w", err)
	}
	wins := make([]broadcast.Window, len(raw))
	for i, r := range raw {
		wins[i] = broadcast.Window{
			ID: broadcast.WindowID(r.ID), Title: r.Title, Class: r.Class, PID: r.PID, Focused: r.Active,
		}
	}
	return wins, nil
}

// Active returns the focused window's ID, or "" when nothing is focused.
func (w *WM) Active(ctx context.Context) (broadcast.WindowID, error) {
	out, err := w.run(ctx, `
		const a = workspace.activeWindow;
		reply(a ? String(a.internalId) : "");
	`)
	return broadcast.WindowID(out), err
}

// captionOf looks up a window by internalId and returns its caption,
// without touching focus at all — used by Activate to decide which
// mechanism to use before committing to either one.
func (w *WM) captionOf(ctx context.Context, id broadcast.WindowID) (string, error) {
	q, _ := json.Marshal(string(id))
	out, err := w.run(ctx, fmt.Sprintf(`
		const target = workspace.windowList().find(w => String(w.internalId) === %s);
		reply(target ? target.caption : "");
	`, q))
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("window %s not found", id)
	}
	return out, nil
}

// activateViaKWin is the original mechanism (see REFERENCE.md 4.10): ask
// KWin's scripting API to switch, trusting its report. Kept only as the
// fallback for native Wayland windows (see xwaylandXID's doc) — for every
// XWayland-bridged window, activateViaXdotool below is used instead; see
// REFERENCE.md 4.11 for why.
func (w *WM) activateViaKWin(ctx context.Context, id broadcast.WindowID) error {
	q, _ := json.Marshal(string(id))
	out, err := w.run(ctx, fmt.Sprintf(`
		const target = workspace.windowList().find(w => String(w.internalId) === %s);
		if (target) { workspace.activeWindow = target; reply("ok"); } else { reply("missing"); }
	`, q))
	if err != nil {
		return err
	}
	if out != "ok" {
		return fmt.Errorf("window %s not found", id)
	}
	return nil
}

// activateViaXdotool switches focus via `xdotool windowactivate --sync <xid>`
// — an EWMH _NET_ACTIVE_WINDOW client message handled BY the window manager
// (KWin), as opposed to `windowfocus`'s raw XSetInputFocus which talks
// straight to XWayland's X server and bypasses KWin entirely.
//
// See REFERENCE.md 4.12: `windowfocus` was tried first (4.11) since its
// XSetInputFocus is independently verifiable via getwindowfocus, and that
// verification did pass reliably — but a real uinput-injected key, sent
// while that verified focus held, still never reached the target in game.
// The reason: XSetInputFocus only updates the XWayland X server's own
// bookkeeping (which is why asking it back confirms success) — it does not
// tell KWin's compositor to redirect real input routing, and for XWayland
// clients that compositor-level routing, not the X11 focus window, is what
// actually decides where genuine evdev/uinput-sourced key events go.
// windowactivate goes through the window manager instead of around it, on
// the theory that only the WM can actually move that compositor-level
// routing. Verified against realActiveTitle (_NET_ACTIVE_WINDOW), the exact
// property windowactivate sets — not realFocusedTitle, which is the wrong
// signal per the above.
func activateViaXdotool(ctx context.Context, xid, wantCaption string) error {
	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := exec.CommandContext(ctx, "xdotool", "windowactivate", "--sync", xid).Run(); err != nil {
			return fmt.Errorf("xdotool windowactivate %s: %w", xid, err)
		}
		if realActiveTitle() == wantCaption {
			return nil
		}
	}
	return fmt.Errorf("xid %s (%q): windowactivate --sync reported success but real active window never followed after %d attempts",
		xid, wantCaption, maxAttempts)
}

// realFocusedTitle shells out to `xdotool getwindowfocus getwindowname` —
// the real XSetInputFocus target, not the window-manager-level
// _NET_ACTIVE_WINDOW hint getactivewindow reads (see REFERENCE.md 4.11 for
// why that distinction turned out to matter). Empty on any failure (no
// window focused, no xdotool, etc.).
func realFocusedTitle() string {
	out, err := exec.Command("xdotool", "getwindowfocus", "getwindowname").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// realActiveTitle shells out to `xdotool getactivewindow getwindowname` —
// the window-manager-level _NET_ACTIVE_WINDOW hint. Kept separate from
// realFocusedTitle (see REFERENCE.md 4.11): the two can disagree, and
// verifying activateViaXdotool needs the real focus target specifically,
// not this hint.
func realActiveTitle() string {
	out, err := exec.Command("xdotool", "getactivewindow", "getwindowname").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// xwaylandXID returns the X11 window id for the window with exactly this
// caption, and whether one was found at all — i.e. whether it's
// XWayland-bridged in the first place, as opposed to a native Wayland
// client.
//
// xdotool (and X11 generally) only has visibility into XWayland-bridged
// clients — confirmed by direct testing: Konsole, VS Code, and other native
// Wayland apps on this KDE Plasma 6 session don't appear in `xdotool search
// --name "."` at all, while Wine/Proton games (genuinely XWayland, since
// Wine has no native Wayland backend) do. Activate uses this to decide
// which mechanism a given window can even use: without it, either verifying
// or driving focus via xdotool against a native Wayland window — most
// commonly the terminal clonecast itself is running in, which is exactly
// where "origin" often is — would see every xdotool query return "" (not
// because anything failed, but because xdotool structurally cannot name
// that window) and either report a false failure or never find an xid to
// focus at all.
func xwaylandXID(caption string) (xid string, ok bool) {
	if caption == "" {
		return "", false
	}
	pattern := "^" + regexp.QuoteMeta(caption) + "$"
	out, err := exec.Command("xdotool", "search", "--name", pattern).Output()
	if err != nil {
		return "", false
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return "", false
	}
	return lines[0], true
}

// Activate gives focus to the window with the given internalId.
//
// UNRESOLVED as of REFERENCE.md 4.11: switching to xdotool's
// `windowfocus --sync` (raw XSetInputFocus, bypassing KWin's scripting
// bridge) measured far more reliable at the X11-focus level — independently
// confirmed via getwindowfocus — than KWin's own workspace.activeWindow
// (4.10). But live, visually-confirmed testing (sending a key that produces
// an unmistakable in-game effect, then watching whether it actually
// happened) showed a case where X11-level focus was independently confirmed
// correct on the intended target and the key still didn't register in
// game. That means neither of the two focus mechanisms tried so far is
// sufficient on its own to explain or fix delivery — the real gap may be in
// how Wine's own input handling (or XWayland's event delivery to it)
// tracks focus, separately from what the X server itself reports. Both
// mechanisms are kept here (xdotool preferred for XWayland-visible windows,
// KWin as the fallback for native Wayland ones) as the best currently
// understood options, not as a confirmed fix. See REFERENCE.md 4.11 before
// assuming this function's success return means a key will actually land.
func (w *WM) Activate(ctx context.Context, id broadcast.WindowID) error {
	caption, err := w.captionOf(ctx, id)
	if err != nil {
		return err
	}
	if w.haveXdotool {
		if xid, ok := xwaylandXID(caption); ok {
			return activateViaXdotool(ctx, xid, caption)
		}
	}
	return w.activateViaKWin(ctx, id)
}

// run loads body as a KWin script, executes it, waits for exactly one reply()
// call, and unloads the script. Operations are serialised: KWin scripts share
// one callback object.
func (w *WM) run(ctx context.Context, body string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := w.seq.Add(1)
	plugin := fmt.Sprintf("clonecast-%d-%d", os.Getpid(), n)
	path := filepath.Join(w.tmpDir, plugin+".js")

	src := fmt.Sprintf(`function reply(s) { callDBus(%q, %q, %q, "Result", String(s)); }
%s`, busName, objPath, ifce, strings.TrimSpace(body))
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		return "", err
	}
	defer os.Remove(path)

	// drain a stale reply, if any
	select {
	case <-w.pending:
	default:
	}

	scripting := w.conn.Object(kwinService, scriptingPath)
	var id int32
	if err := scripting.CallWithContext(ctx, scriptingIfce+".loadScript", 0, path, plugin).Store(&id); err != nil {
		return "", fmt.Errorf("kwin loadScript: %w", err)
	}
	defer scripting.Call(scriptingIfce+".unloadScript", 0, plugin)
	if id < 0 {
		return "", fmt.Errorf("kwin refused script (id %d)", id)
	}

	// Plasma 6 exposes the loaded script at /Scripting/Script<id>.
	scriptObj := w.conn.Object(kwinService, dbus.ObjectPath(fmt.Sprintf("%s/Script%d", scriptingPath, id)))
	if err := scriptObj.CallWithContext(ctx, scriptIfce+".run", 0).Err; err != nil {
		return "", fmt.Errorf("kwin run: %w", err)
	}

	select {
	case out := <-w.pending:
		return out, nil
	case <-ctx.Done():
		return "", fmt.Errorf("kwin script did not reply: %w", ctx.Err())
	}
}
