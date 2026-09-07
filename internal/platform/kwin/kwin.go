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
	"path/filepath"
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

// Activate gives focus to the window with the given internalId.
func (w *WM) Activate(ctx context.Context, id broadcast.WindowID) error {
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
