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
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procGetDesktopWindow = user32.NewProc("GetDesktopWindow")
	procGetWindow        = user32.NewProc("GetWindow")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procPeekMessageW     = user32.NewProc("PeekMessageW")
	procIsWindow         = user32.NewProc("IsWindow")

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
	out = io.MultiWriter(os.Stderr, f)
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

// resolveHWND blocks until the game window appears. It never gives up: since
// the agent is autostarted by the prefix itself (RunServices, see
// `clonecast agent install`), it runs *before* the launcher starts the game,
// and the wait is as long as the user takes to get to the character screen —
// minutes, not the 60 s the hand-launched agent used to allow. The prefix owns
// the agent's lifetime now, so "wait forever" costs nothing: the agent dies
// with the wineserver.
//
// The poll starts fast (the game may already be up when a user installs and
// relaunches) and slows to 5 s, so an idle agent is invisible in a busy
// wineserver — the tree-walk is the only Win32 work it does while waiting.
//
// The wait happens before the first dial because the hello announces the
// window: an agent with no window has nothing for clonecast to pair a target
// with, and would only show up in the TUI as an agent for nothing.
func resolveHWND(title string) uintptr {
	// A stray PeekMessage so this thread has a message queue, matching what a
	// normal GUI process does before touching window APIs.
	var msg [12]uintptr
	procPeekMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, 0)

	wait := 500 * time.Millisecond
	const maxWait = 5 * time.Second
	for i := 0; ; i++ {
		if h := findByTitle(title); h != 0 {
			return h
		}
		if i == 20 || (i > 20 && i%60 == 0) { // ~10 s, then every ~5 min
			logf("still waiting for a window titled %q ...", title)
		}
		time.Sleep(wait)
		if wait < maxWait {
			wait += 250 * time.Millisecond
		}
	}
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
	ok, _, _ := procIsWindow.Call(h)
	return ok != 0 && getWindowText(h) == r.title
}

// current returns a live HWND for the target window, re-walking if the cached
// one is stale. Returns 0 if the window can't be found right now.
func (r *resolver) current() uintptr {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.valid(r.hwnd) {
		return r.hwnd
	}
	r.hwnd = findByTitle(r.title)
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
	const (
		minBackoff = 500 * time.Millisecond
		maxBackoff = 30 * time.Second
	)
	backoff := minBackoff
	complained := false
	for {
		c, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			if !complained {
				logf("cannot reach clonecast on %s yet (%v); retrying, up to every %s", addr, err, maxBackoff)
				complained = true // once per outage, not once per retry
			}
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		backoff, complained = minBackoff, false
		session(c, res, title)
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
	title := flag.String("title", "World of Warcraft", "exact window title to deliver keys to")
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

	logf("starting (pid %d), will dial clonecast at %s (port from %s), locating window %q ...", os.Getpid(), addr, from, *title)
	hwnd := resolveHWND(*title)
	logf("window found: hwnd=0x%x", hwnd)

	connectLoop(addr, &resolver{title: *title, hwnd: hwnd}, *title)
}
