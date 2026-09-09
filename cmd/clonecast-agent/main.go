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
// Usage:
//
//	clonecast-agent [-port N] [-title "World of Warcraft"]
//
// Build: GOOS=windows GOARCH=386 CGO_ENABLED=0 go build ./cmd/clonecast-agent
// (386 because vanilla WoW / its Wine is 32-bit and 64-bit Go PE has been seen
// to fail to load under this Wine; 386 loads fine.)
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
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
)

const (
	gwHWNDNEXT = 2
	gwCHILD    = 5

	wmKEYDOWN = 0x0100
	wmKEYUP   = 0x0101
)

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[clonecast-agent] "+format+"\n", a...)
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

// resolveHWND blocks until the game window appears (or ~60s elapse), so the
// agent can be launched before the game finished creating its window.
func resolveHWND(title string) uintptr {
	// A stray PeekMessage so this thread has a message queue, matching what a
	// normal GUI process does before touching window APIs.
	var msg [12]uintptr
	procPeekMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, 0)
	for i := 0; i < 120; i++ {
		if h := findByTitle(title); h != 0 {
			return h
		}
		time.Sleep(500 * time.Millisecond)
	}
	return 0
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

func post(hwnd uintptr, f agentwire.Frame) {
	vk, scan, ext, ok := agentwire.WinKey(f.Code)
	if !ok {
		logf("unmapped evdev code %d, ignored", f.Code)
		return
	}
	msg := uintptr(wmKEYUP)
	if f.Down {
		msg = wmKEYDOWN
	}
	procPostMessageW.Call(hwnd, msg, uintptr(vk), lparam(scan, ext, !f.Down))
}

func main() {
	port := flag.Int("port", 48900, "loopback TCP port to listen on")
	title := flag.String("title", "World of Warcraft", "exact window title to deliver keys to")
	flag.Parse()

	logf("locating window %q ...", *title)
	hwnd := resolveHWND(*title)
	if hwnd == 0 {
		logf("window %q not found after timeout; exiting", *title)
		os.Exit(1)
	}
	logf("window found: hwnd=0x%x", hwnd)

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
			_ = agentwire.Read(c, func(f agentwire.Frame) { post(hwnd, f) })
			logf("client disconnected")
		}(conn)
	}
}
