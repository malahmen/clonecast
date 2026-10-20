//go:build linux

package kwin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// watchSeq tags the persistent script's pushes, so Result tells them from the
// one-shot scripts' replies, whose sequence numbers are never negative.
const watchSeq = "-1"

// watchPlugin is the persistent script's plugin name.
const watchPlugin = "clonecast-watch"

// rewatchEvery is the least time between two attempts to load the script
// again after it stopped (KWin restarted) or never loaded.
const rewatchEvery = 5 * time.Second

// watchScript pushes KWin's active window and the normal windows' pids at
// load and whenever a window is activated, added or removed. Active used to
// load a script for every key, and with the X focus correction that script
// serialized the whole window list too (Kamino item 14).
const watchScript = `
function push() {
	const a = workspace.activeWindow;
	callDBus(%q, %q, %q, "Result", %q, JSON.stringify({
		active: a ? {id: String(a.internalId), pid: a.pid} : null,
		windows: workspace.windowList()
			.filter(w => w.normalWindow)
			.map(w => ({id: String(w.internalId), pid: w.pid})),
	}));
}
workspace.windowActivated.connect(push);
workspace.windowAdded.connect(push);
workspace.windowRemoved.connect(push);
push();
`

// watcher is the cache the persistent script fills.
type watcher struct {
	mu      sync.Mutex
	st      activeState
	ok      bool      // a push arrived since the script was (re)loaded
	tried   time.Time // the last load attempt
	loaded  bool
	signals chan *dbus.Signal
}

// update takes one push. A payload that does not decode leaves the cache
// unusable rather than wrong, so Active asks KWin directly.
func (c *watcher) update(payload string) {
	var st activeState
	err := json.Unmarshal([]byte(payload), &st)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st, c.ok = st, err == nil
}

// state is the cached answer, and whether there is one.
func (c *watcher) state() (activeState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.st, c.ok
}

// invalidate drops the cache: KWin went away, and its script with it.
func (c *watcher) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ok, c.loaded = false, false
}

// startWatch loads the persistent script and follows KWin's bus name, so a
// KWin restart, which unloads every script, empties the cache instead of
// leaving it stale. On any failure Active works as before, a script per call.
func (w *WM) startWatch() {
	if err := w.conn.AddMatchSignal(
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
		dbus.WithMatchArg(0, kwinService),
	); err == nil {
		w.watch.signals = make(chan *dbus.Signal, 4)
		w.conn.Signal(w.watch.signals)
		go func() {
			for sig := range w.watch.signals {
				if sig.Name == "org.freedesktop.DBus.NameOwnerChanged" {
					w.watch.invalidate()
				}
			}
		}()
	}
	w.rewatch()
}

// rewatch loads the persistent script unless it is loaded, or was tried less
// than rewatchEvery ago.
func (w *WM) rewatch() {
	w.watch.mu.Lock()
	if w.watch.loaded || time.Since(w.watch.tried) < rewatchEvery {
		w.watch.mu.Unlock()
		return
	}
	w.watch.tried = time.Now()
	w.watch.mu.Unlock()

	if err := w.loadWatch(); err != nil {
		return
	}
	w.watch.mu.Lock()
	w.watch.loaded = true
	w.watch.mu.Unlock()
}

func (w *WM) loadWatch() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	scripting := w.conn.Object(kwinService, scriptingPath)
	scripting.Call(scriptingIfce+".unloadScript", 0, watchPlugin) // a leftover from a crashed run
	path := filepath.Join(w.tmpDir, watchPlugin+".js")
	src := fmt.Sprintf(watchScript, busName, objPath, ifce, watchSeq)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		return err
	}
	var id int32
	if err := scripting.Call(scriptingIfce+".loadScript", 0, path, watchPlugin).Store(&id); err != nil {
		return err
	}
	if id < 0 {
		return fmt.Errorf("kwin refused the watch script (id %d)", id)
	}
	obj := w.conn.Object(kwinService, dbus.ObjectPath(fmt.Sprintf("%s/Script%d", scriptingPath, id)))
	return obj.Call(scriptIfce+".run", 0).Err
}

// stopWatch unloads the persistent script.
func (w *WM) stopWatch() {
	// The signal channel is left to the connection: closing it, Close
	// closes the channel and the goroutine reading it ends.
	w.conn.Object(kwinService, scriptingPath).Call(scriptingIfce+".unloadScript", 0, watchPlugin)
}
