//go:build windows

// Command clonecast-agent is the in-bottle delivery helper for clonecast's
// "agent" backend (REFERENCE.md 4.12/7.9). One runs inside each Wine prefix,
// launched alongside the game. It listens on a loopback TCP port and, for each
// key frame clonecast sends, calls PostMessage(WM_KEYDOWN/UP) on the game
// window — delivering to the window's message queue with no focus change and
// no cursor movement, exactly like HotkeyNet's SendWinM but WITHOUT a keyboard
// hook (HotkeyNet's hook corrupts the user's real typing under Wine; this
// installs none).
//
// It deliberately avoids the two Win32 calls that misbehave under Wine in a
// live wineserver: EnumWindows (its callback spins) and FindWindow (hangs when
// a busy game shares the wineserver). The game window is located once via a
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
//	clonecast-agent [-port N] [-title "World of Warcraft"] [-log PATH]
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
	"sync"
	"syscall"
	"time"
	"unsafe"

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
// starts two agents. The second must exit quietly — otherwise it sits forever
// waiting for a window it will never be allowed to serve, and races the first
// for the listening port.
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

func main() {
	// Pin to a single OS thread/P. Go's multi-threaded scheduler (and its
	// sysmon/netpoller) has been observed to spin at ~100% CPU under Wine,
	// nondeterministically, in a busy wineserver (one agent came up fine, a
	// second spun). GOMAXPROCS(1) removes the multi-P scheduler contention
	// that triggers it. This process does very little work, so one P is
	// plenty.
	runtime.GOMAXPROCS(1)

	port := flag.Int("port", 48900, "loopback TCP port to listen on")
	title := flag.String("title", "World of Warcraft", "exact window title to deliver keys to")
	logPath := flag.String("log", defaultLogPath(), "log file (there is no console under RunServices autostart); empty to disable")
	flag.Parse()

	setupLog(*logPath)

	// One agent per port per prefix. See singleInstance: a RunServices entry
	// is started twice on a 64-bit prefix.
	mutexName := fmt.Sprintf(`Local\clonecast-agent-%d`, *port)
	if !singleInstance(mutexName) {
		logf("another agent already holds %s; exiting (this is the expected second start on a 64-bit prefix)", mutexName)
		return
	}

	logf("starting (pid %d, port %d), locating window %q ...", os.Getpid(), *port, *title)
	hwnd := resolveHWND(*title)
	logf("window found: hwnd=0x%x", hwnd)
	res := &resolver{title: *title, hwnd: hwnd}

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logf("listen %s: %v", addr, err)
		os.Exit(1)
	}
	logf("listening on %s, delivering to hwnd=0x%x", addr, hwnd)

	for {
		conn, err := ln.Accept()
		if err != nil {
			logf("accept: %v", err)
			continue
		}
		logf("client connected: %s", conn.RemoteAddr())
		go func(c net.Conn) {
			defer c.Close()
			_ = agentwire.Read(c, func(f agentwire.Frame) { post(res, f) })
			logf("client disconnected")
		}(conn)
	}
}
