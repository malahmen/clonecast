//go:build windows

// Command clonecast-agent is the in-bottle delivery helper for clonecast's
// "agent" backend (REFERENCE.md 4.12/7.9). One runs inside each Wine prefix.
// For each key frame clonecast sends it calls PostMessage(WM_KEYDOWN/UP) on
// the game window — delivering to the window's message queue with no focus
// change and no cursor movement, exactly like HotkeyNet's SendWinM but
// WITHOUT a keyboard hook (HotkeyNet's hook corrupts the user's real typing
// under Wine; this installs none).
//
// It dials clonecast; clonecast does not dial it (REFERENCE.md 4.17). On
// connect it announces its WINEPREFIX, its window title, its Windows pid and
// the resolved HWND, and clonecast pairs that announcement with a window in
// the compositor's list — so the user ticks a window in the TUI and nothing
// else. Before this, every agent listened on a hand-picked port and the user
// had to repeat the target list as --agent "Title=port,...". The dial is
// retried with backoff forever, because the agent normally starts *first*: it
// is autostarted at prefix boot, long before clonecast may be running.
//
// Where the port comes from, in order: CLONECAST_PORT in the environment
// (Wine passes Unix environment variables through unchanged, so this is the
// per-launch override a launcher can set, e.g. a Steam launch option), then
// -port on the command line (what `clonecast agent install` bakes into the
// autostart entry), then HKCU\Software\clonecast\Port, then 48800.
//
// It deliberately avoids the two Win32 calls that misbehave under Wine in a
// live wineserver: EnumWindows (its callback spins) and FindWindow (hangs when
// a busy game shares the wineserver). The game window is located via a
// GetWindow tree-walk (callback-free) and the HWND cached; thereafter only
// PostMessage is used.
//
// Lifetime belongs to the prefix, not to clonecast and not to the launcher
// (REFERENCE.md 4.16/7.11). `clonecast agent install --prefix <path>` copies
// this binary into <prefix>/drive_c/clonecast and registers it under
// HKLM\Software\Microsoft\Windows\CurrentVersion\RunServices, which Wine's
// implicit `wineboot --init` runs on every prefix boot under every launcher.
// Consequences visible here: it starts long before the game (so the window
// wait is unbounded), it is started twice on a 64-bit prefix (so it takes a
// named mutex), it has no console (so it logs to a file beside itself), and it
// dies with the wineserver (so nothing on the host ever signals it — doing so
// killed the game, 4.14).
//
// Usage:
//
//	clonecast-agent [-port N] [-host 127.0.0.1] [-title "World of Warcraft"] [-log PATH]
//
// Build: GOOS=windows GOARCH=386 CGO_ENABLED=0 go build ./cmd/clonecast-agent
// (386 because vanilla WoW / its Wine is 32-bit and 64-bit Go PE has been seen
// to fail to load under this Wine; 386 loads fine.)
package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/redial"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procGetDesktopWindow = user32.NewProc("GetDesktopWindow")
	procGetWindow        = user32.NewProc("GetWindow")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procPeekMessageW     = user32.NewProc("PeekMessageW")
	procIsWindow         = user32.NewProc("IsWindow")
	procIsWindowVisible  = user32.NewProc("IsWindowVisible")
	procGetWindowRect    = user32.NewProc("GetWindowRect")

	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex = kernel32.NewProc("CreateMutexW")
)

const (
	gwHWNDNEXT = 2
	gwCHILD    = 5

	wmKEYDOWN = 0x0100
	wmKEYUP   = 0x0101

	errAlreadyExists = syscall.Errno(183) // ERROR_ALREADY_EXISTS

	// defaultPort must match internal/platform/agent.DefaultListen. It is
	// repeated rather than imported because this binary is built for Windows
	// and that package is the Linux side.
	defaultPort = 48800
	defaultHost = "127.0.0.1"

	// portKey is the per-prefix override `clonecast agent install` can write
	// and a user can set by hand with `reg add`.
	portKey   = `Software\clonecast`
	portValue = "Port"
)

// out is where logf writes. Started from RunServices there is no console at
// all, so the log file next to the exe inside the prefix is the only way to
// see what the agent did (and when: it should appear before the game window
// exists).
var out io.Writer = os.Stderr

// noWindowOnce keeps the "no window" explanation to a single line.
var noWindowOnce sync.Once

func logf(format string, a ...any) {
	fmt.Fprintf(out, "%s [clonecast-agent] "+format+"\n",
		append([]any{time.Now().Format("15:04:05.000")}, a...)...)
}

// setupLog tees the log to a file. Failure is not fatal: the agent is more
// useful running without a log than not running at all.
func setupLog(path string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logf("log file %s: %v (continuing without one)", path, err)
		return
	}
	// NOT io.MultiWriter: it stops at the first writer that errors, and built
	// with -H windowsgui there is no console, so every write would fail at
	// stderr and never reach the file — the log would be silently empty, which
	// is the one thing it exists to prevent.
	out = tolerantTee{os.Stderr, f}
}

// defaultLogPath puts the log next to the agent binary, i.e. inside the prefix
// (C:\clonecast\clonecast-agent.log for an installed agent).
func defaultLogPath() string {
	self, err := os.Executable()
	if err != nil {
		return "clonecast-agent.log"
	}
	return filepath.Join(filepath.Dir(self), "clonecast-agent.log")
}

// singleInstance takes a named mutex and reports whether this process is the
// first holder. It must be called before anything else: `wineboot --init`
// processes the Run keys twice on a 64-bit prefix (it re-opens them with
// KEY_WOW64_32KEY and this key is not redirected), so one RunServices entry
// starts two agents. The second must exit quietly — otherwise both of them
// register with clonecast and every key is posted to the window twice.
//
// The handle is deliberately never closed: it is released when the process
// exits, which is exactly the lifetime we want.
func singleInstance(name string) bool {
	p, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return true
	}
	h, _, callErr := procCreateMutex.Call(0, 0, uintptr(unsafe.Pointer(p)))
	if h == 0 {
		logf("CreateMutex(%s) failed: %v (continuing anyway)", name, callErr)
		return true
	}
	return callErr != errAlreadyExists
}

func getWindowText(hwnd uintptr) string {
	var buf [256]uint16
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return syscall.UTF16ToString(buf[:n])
}

// findByTitle walks the window tree from the desktop (depth-limited,
// callback-free) and returns the first window whose title matches want.
func findByTitle(want string) uintptr {
	desktop, _, _ := procGetDesktopWindow.Call()
	var walk func(hwnd uintptr, depth int) uintptr
	walk = func(hwnd uintptr, depth int) uintptr {
		if depth > 4 {
			return 0
		}
		child, _, _ := procGetWindow.Call(hwnd, gwCHILD)
		for child != 0 {
			if getWindowText(child) == want {
				return child
			}
			if got := walk(child, depth+1); got != 0 {
				return got
			}
			child, _, _ = procGetWindow.Call(child, gwHWNDNEXT)
		}
		return 0
	}
	return walk(desktop, 0)
}

// findGameWindow picks this prefix's game window without being told a title.
//
// Addressing by title does not survive contact with multiboxing: every agent is
// installed with the same default ("World of Warcraft"), so on three clients
// two find nothing and the third posts into a window belonging to a different
// box. A launcher that renames windows per instance makes it worse, not better,
// because then NO agent's title matches. Observed exactly that: all three
// agents receiving keys and logging "target window not found".
//
// The prefix is the right unit of addressing, and it needs no cooperation: each
// Wine prefix has its own wineserver and therefore its own window list, so the
// windows this agent can see are by construction its own prefix's. The game is
// the largest visible top-level window among them. The agent itself has no
// window (-H windowsgui, no console), so there is nothing of its own to exclude.
func findGameWindow() uintptr {
	desktop, _, _ := procGetDesktopWindow.Call()
	var best uintptr
	var bestArea int64
	seen, considered := 0, 0

	// Recursive, like findByTitle: a game window is not necessarily a direct
	// child of the desktop — Wine nests it under its own desktop/explorer
	// window depending on how the prefix was launched. A one-level sweep found
	// nothing at all, which is why this exists in this shape.
	var walk func(hwnd uintptr, depth int)
	walk = func(hwnd uintptr, depth int) {
		if depth > 4 {
			return
		}
		child, _, _ := procGetWindow.Call(hwnd, gwCHILD)
		for child != 0 {
			seen++
			var r struct{ Left, Top, Right, Bottom int32 }
			visible, _, _ := procIsWindowVisible.Call(child)
			if ok, _, _ := procGetWindowRect.Call(child, uintptr(unsafe.Pointer(&r))); ok != 0 && visible != 0 {
				w, h := int64(r.Right-r.Left), int64(r.Bottom-r.Top)
				// 100x100 keeps out the IME and helper popups Wine creates,
				// the same floor the launcher's own window search uses.
				if w >= 100 && h >= 100 {
					considered++
					if w*h > bestArea {
						bestArea, best = w*h, child
					}
				}
			}
			walk(child, depth+1)
			child, _, _ = procGetWindow.Call(child, gwHWNDNEXT)
		}
	}
	walk(desktop, 0)

	if best == 0 {
		// Said once rather than on every dropped key: without it a failure here
		// is indistinguishable from the window genuinely being gone, which cost
		// a lot of guessing.
		noWindowOnce.Do(func() {
			logf("no game window found in this prefix: walked %d window(s), %d of a usable size. "+
				"Keys will be dropped until one appears.", seen, considered)
		})
	}
	return best
}

// resolver caches the game window's HWND but re-resolves it when it goes
// stale. WoW recreates its top-level window across state changes (login ->
// char-select -> world, loading screens), so a once-cached HWND becomes a dead
// handle and PostMessage silently goes nowhere. Validity is checked cheaply
// (IsWindow + the title still matches) on every send; only when that fails is
// the full GetWindow tree-walk re-run.
type resolver struct {
	title string
	mu    sync.Mutex
	hwnd  uintptr
}

func (r *resolver) valid(h uintptr) bool {
	if h == 0 {
		return false
	}
	if ok, _, _ := procIsWindow.Call(h); ok == 0 {
		return false
	}
	if r.title == "" {
		// Addressing by prefix: any live, visible window of a usable size is
		// still the one we found. Re-walking on every key would be wasteful,
		// and WoW keeps one top-level window per state.
		visible, _, _ := procIsWindowVisible.Call(h)
		return visible != 0
	}
	return getWindowText(h) == r.title
}

// current returns a live HWND for the target window, re-walking if the cached
// one is stale. Returns 0 if the window can't be found right now.
func (r *resolver) current() uintptr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.valid(r.hwnd) {
		return r.hwnd
	}
	if r.title == "" {
		r.hwnd = findGameWindow()
	} else {
		// An explicit title still wins, for a prefix with more than one
		// window where the operator knows which. Falling back to the
		// prefix-wide search means a wrong or stale title degrades to
		// "deliver to this prefix's game" instead of to nothing at all.
		if r.hwnd = findByTitle(r.title); r.hwnd == 0 {
			r.hwnd = findGameWindow()
		}
	}
	return r.hwnd
}

func lparam(scan uint16, extended, up bool) uintptr {
	lp := uint32(1) | uint32(scan)<<16 // repeat count 1, scancode
	if extended {
		lp |= 1 << 24
	}
	if up {
		lp |= 1<<30 | 1<<31 // previous-down + transition (key release)
	}
	return uintptr(lp)
}

func post(r *resolver, f agentwire.Frame) {
	vk, scan, ext, ok := agentwire.WinKey(f.Code)
	if !ok {
		logf("unmapped evdev code %d, ignored", f.Code)
		return
	}
	hwnd := r.current()
	if hwnd == 0 {
		logf("target window not found (gone?), dropping evdev %d", f.Code)
		return
	}
	msg := uintptr(wmKEYUP)
	if f.Down {
		msg = wmKEYDOWN
	}
	procPostMessageW.Call(hwnd, msg, uintptr(vk), lparam(scan, ext, !f.Down))
}

// conn wraps the connection to clonecast so the keepalive goroutine and the
// (rare) re-announcement cannot interleave with each other mid-line.
type conn struct {
	mu sync.Mutex
	c  net.Conn
}

func (w *conn) writeHello(h agentwire.Hello) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return h.Write(w.c)
}

func (w *conn) writeKeepalive() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return agentwire.WriteKeepalive(w.c)
}

// registryPort reads the per-prefix port override. Missing is not an error
// worth reporting: most prefixes will not have it.
func registryPort() (int, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER, portKey, registry.QUERY_VALUE)
	if err != nil {
		return 0, false
	}
	defer k.Close()
	if n, _, err := k.GetIntegerValue(portValue); err == nil && n > 0 && n < 65536 {
		return int(n), true
	}
	// REG_SZ too: `reg add ... /t REG_SZ` is what a user types by hand, and
	// what the install command would write through Wine's own reg.exe.
	if s, _, err := k.GetStringValue(portValue); err == nil {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65536 {
			return n, true
		}
	}
	return 0, false
}

// resolvePort applies the documented precedence and says where the value came
// from, because "which port is this agent actually using" is the first
// question when nothing registers.
//
// The environment comes first on purpose: the command line is baked into the
// registry at install time and is the same for every launch, while the
// environment is per-launch and is the only lever a launcher (a Steam launch
// option, a Bottles env var) can pull.
func resolvePort(flagPort int, flagSet bool) (int, string) {
	if s := os.Getenv("CLONECAST_PORT"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 65536 {
			return n, "CLONECAST_PORT"
		}
		logf("ignoring CLONECAST_PORT=%q: not a port number", s)
	}
	if flagSet {
		return flagPort, "-port"
	}
	if n, ok := registryPort(); ok {
		return n, `HKCU\` + portKey + `\` + portValue
	}
	return defaultPort, "default"
}

func resolveHost(flagHost string) string {
	if s := os.Getenv("CLONECAST_HOST"); s != "" {
		return s
	}
	return flagHost
}

// hello is what this agent announces on every (re)connection.
func hello(title string, hwnd uintptr) agentwire.Hello {
	self, _ := os.Executable()
	return agentwire.Hello{
		Version: agentwire.Version,
		// WINEPREFIX is what clonecast joins against the host pid of the KWin
		// window (REFERENCE.md 4.17). Wine hands the Unix environment to
		// Windows processes unchanged, so this is set under every launcher
		// that sets it — and when it is not, clonecast falls back to Title.
		Prefix: os.Getenv("WINEPREFIX"),
		Title:  title,
		Exe:    self,
		PID:    os.Getpid(),
		HWND:   uint64(hwnd),
	}
}

// session runs one connection to clonecast: announce, keep alive, and post
// every frame that arrives until the connection ends. It returns when the
// connection is over; the caller redials.
func session(c net.Conn, res *resolver, title string) {
	defer c.Close()
	w := &conn{c: c}

	announced := res.current()
	if err := w.writeHello(hello(title, announced)); err != nil {
		logf("hello: %v", err)
		return
	}
	logf("registered with clonecast at %s (hwnd=0x%x, prefix=%q)", c.RemoteAddr(), announced, os.Getenv("WINEPREFIX"))

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(agentwire.KeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// Re-announce when the game recreated its window, so
				// clonecast's view of the HWND stays true (it is diagnostic,
				// but a stale one sends people hunting the wrong problem).
				if h := res.current(); h != 0 && h != announced {
					announced = h
					if err := w.writeHello(hello(title, h)); err != nil {
						return // the read loop will see the same failure
					}
					logf("window changed, re-announced hwnd=0x%x", h)
					continue
				}
				if err := w.writeKeepalive(); err != nil {
					return
				}
			}
		}
	}()

	// Frames (and clonecast's pings) until the connection drops. A malformed
	// line is skipped, never fatal: one corrupt line must not cost every
	// keystroke after it.
	err := agentwire.Read(c, func(m agentwire.Message) {
		if m.Kind == agentwire.KindFrame {
			post(res, m.Frame)
		}
	})
	if err != nil {
		logf("connection to clonecast ended: %v", err)
		return
	}
	logf("clonecast closed the connection")
}

// connectLoop dials clonecast forever, with backoff. clonecast may not be
// running yet (the normal case: the agent starts at prefix boot), may be
// restarted, or may be started only when the user sits down to play.
func connectLoop(addr string, res *resolver, title string) {
	backoff := redial.Min
	complained := false
	for {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			if !complained {
				logf("cannot reach clonecast on %s yet (%v); retrying, up to every %s", addr, err, redial.Max)
				complained = true // once per outage, not once per retry
			}
			time.Sleep(backoff)
			backoff = redial.Grow(backoff)
			continue
		}
		// Timed, because the backoff used to be reset here — before the
		// session had run — so a connection that died immediately redialled
		// with no sleep. See nextBackoff.
		start := time.Now()
		session(c, res, title)
		wait, reset := redial.Next(time.Since(start), backoff)
		if reset {
			backoff, complained = redial.Min, false
			continue
		}
		if !complained {
			logf("connection to %s ended after %s; backing off up to %s", addr, time.Since(start).Round(time.Millisecond), redial.Max)
			complained = true
		}
		time.Sleep(wait)
		backoff = redial.Grow(wait)
	}
}

func main() {
	// Pin to a single OS thread/P. Go's multi-threaded scheduler (and its
	// sysmon/netpoller) has been observed to spin at ~100% CPU under Wine,
	// nondeterministically, in a busy wineserver (one agent came up fine, a
	// second spun). GOMAXPROCS(1) removes the multi-P scheduler contention
	// that triggers it. This process does very little work, so one P is
	// plenty.
	runtime.GOMAXPROCS(1)

	port := flag.Int("port", defaultPort, "clonecast's listening port to dial (see also CLONECAST_PORT and HKCU\\Software\\clonecast\\Port)")
	host := flag.String("host", defaultHost, "clonecast's host; loopback is the host's loopback even from a Flatpak sandbox that shares the network")
	// Empty by default: deliver to this PREFIX's game window, whatever it is
	// called. A shared default title cannot work for several clients at once —
	// see findGameWindow.
	title := flag.String("title", "", `exact window title to deliver to; empty (the default) means this prefix's own game window`)
	logPath := flag.String("log", defaultLogPath(), "log file (there is no console under RunServices autostart); empty to disable")
	flag.Parse()

	portSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "port" {
			portSet = true
		}
	})

	setupLog(*logPath)

	p, from := resolvePort(*port, portSet)
	addr := net.JoinHostPort(resolveHost(*host), strconv.Itoa(p))

	// One agent per prefix per clonecast endpoint. See singleInstance: a
	// RunServices entry is started twice on a 64-bit prefix.
	mutexName := fmt.Sprintf(`Local\clonecast-agent-%d`, p)
	if !singleInstance(mutexName) {
		logf("another agent already holds %s; exiting (this is the expected second start on a 64-bit prefix)", mutexName)
		return
	}

	logf("starting (pid %d), will dial clonecast at %s (port from %s); delivering to window %q", os.Getpid(), addr, from, *title)

	// Register FIRST, resolve the window when a key actually has to be
	// delivered. Registration only says "an agent exists, and this is the
	// prefix it serves" — it needs no window. Blocking on the window meant an
	// agent whose game used a different title never registered at all, so
	// clonecast reported "no agent" for a prefix running a perfectly good one.
	// resolver.current() already re-walks on demand and returns 0 while there
	// is nothing to find, so starting from 0 is exactly what it expects.
	connectLoop(addr, &resolver{title: *title}, *title)
}

// tolerantTee writes to every writer and reports success if ANY of them took
// the bytes. io.MultiWriter is the wrong tool here: it returns on the first
// error, so one dead writer (a GUI binary's absent stderr) suppresses the rest.
type tolerantTee []io.Writer

func (t tolerantTee) Write(p []byte) (int, error) {
	wrote := false
	for _, w := range t {
		if w == nil {
			continue
		}
		if _, err := w.Write(p); err == nil {
			wrote = true
		}
	}
	if !wrote {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}
