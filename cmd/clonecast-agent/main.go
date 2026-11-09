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
// connect it announces its prefix, its window's caption, its Windows pid and
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
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"

	"github.com/malahmen/clonecast/internal/agentkeys"
	"github.com/malahmen/clonecast/internal/agentwin"
	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/logcap"
	"github.com/malahmen/clonecast/internal/lookup"
	"github.com/malahmen/clonecast/internal/redial"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procGetDesktopWindow = user32.NewProc("GetDesktopWindow")
	procGetWindow        = user32.NewProc("GetWindow")
	procGetWindowTextW   = user32.NewProc("GetWindowTextW")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procIsWindow         = user32.NewProc("IsWindow")
	procIsWindowVisible  = user32.NewProc("IsWindowVisible")
	procIsIconic         = user32.NewProc("IsIconic")
	procGetWindowRect    = user32.NewProc("GetWindowRect")
	procGetWindowLongW   = user32.NewProc("GetWindowLongW")

	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procCreateMutex         = kernel32.NewProc("CreateMutexW")
	procTerminate           = kernel32.NewProc("TerminateProcess")
	procWineGetUnixFileName = kernel32.NewProc("wine_get_unix_file_name")
	procGetProcessHeap      = kernel32.NewProc("GetProcessHeap")
	procHeapFree            = kernel32.NewProc("HeapFree")

	ntdll                       = syscall.NewLazyDLL("ntdll.dll")
	procNtSetInformationProcess = ntdll.NewProc("NtSetInformationProcess")
)

// processWineMakeProcessSystem is Wine's private NtSetInformationProcess
// class (winternl.h), the one services.exe and explorer.exe use.
const processWineMakeProcessSystem = 1000

const (
	gwHWNDNEXT = 2
	gwOWNER    = 4
	gwCHILD    = 5

	gwlEXSTYLE     = ^uintptr(19) // GWL_EXSTYLE, -20
	wsExTOOLWINDOW = 0x00000080

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

// searchEvery is the least time between two window searches that find
// nothing, so keys arriving while the game is gone do not each walk the whole
// window tree (Kamino item 39).
const searchEvery = time.Second

// recheckEvery is how often a window that is still valid is compared with the
// others again, so a launcher or a dialog chosen before the game's window
// existed gives way to it (Kamino 202). The keepalive ticker calls current()
// every agentwire.KeepaliveInterval, so a recheck happens at least that often
// without a key.
const recheckEvery = agentwire.KeepaliveInterval

func logf(format string, a ...any) {
	fmt.Fprintf(out, "%s [clonecast-agent] "+format+"\n",
		append([]any{time.Now().Format("15:04:05.000")}, a...)...)
}

// logCap is the log's size cap; the full one is kept as <log>.1. An agent
// writes a few lines per game session, and there is one per prefix.
const logCap = 1 << 20

// setupLog tees the log to a file. Failure is not fatal: the agent is more
// useful running without a log than not running at all.
func setupLog(path string) {
	if path == "" {
		return
	}
	f, err := logcap.Open(path, logCap)
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

// systemOnce runs exitWithThePrefix once, when the game window is first found.
var systemOnce sync.Once

// exitWithThePrefix makes the agent a Wine system process, like services.exe,
// and exits when Wine shuts the prefix down. Wine keeps a prefix up while any
// ordinary process runs, so an ordinary agent kept it up after the game had
// closed or crashed: the wineserver and services stayed, the launcher thought
// the game still ran, and installing refused the "running" prefix. As a
// system process the agent no longer counts, and Wine signals the event once
// the last ordinary process (the game) is gone.
//
// Only once the game's window exists. Wine does not count a system process as
// a user of the desktop, and the agent starts at boot, seconds before the
// game's first window: made a system process then, it was the desktop's only
// would-be user, Wine closed that desktop, the game got a new one, and the
// agent saw no windows at all and dropped every key (2026-10-20).
func exitWithThePrefix() {
	var ev syscall.Handle
	const currentProcess = ^uintptr(0) // the pseudo-handle -1
	st, _, _ := procNtSetInformationProcess.Call(currentProcess, processWineMakeProcessSystem,
		uintptr(unsafe.Pointer(&ev)), unsafe.Sizeof(ev))
	if st != 0 {
		logf("could not become a Wine system process (NTSTATUS 0x%x); this agent will keep the prefix up after the game exits", st)
		return
	}
	go func() {
		_, _ = syscall.WaitForSingleObject(ev, syscall.INFINITE)
		logf("the prefix is shutting down (its last program exited); exiting")
		// Not os.Exit: its ExitProcess hung under Wine here, the main
		// thread spinning at full CPU after the wineserver had gone.
		_, _, _ = procTerminate.Call(currentProcess, 0)
		os.Exit(0)
	}()
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
// windows this agent can see are by construction its own prefix's. The agent
// itself has no window (-H windowsgui, no console), so there is nothing of its
// own to exclude. Which of them is the game is agentwin.Choose's call: the
// largest visible unowned one, keeping current on a tie.
func findGameWindow(current uintptr) uintptr {
	desktop, _, _ := procGetDesktopWindow.Call()
	var cands []agentwin.Candidate
	seen := 0

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
			cands = append(cands, describe(child))
			walk(child, depth+1)
			child, _, _ = procGetWindow.Call(child, gwHWNDNEXT)
		}
	}
	walk(desktop, 0)

	best := agentwin.Choose(cands, current)
	if best == 0 {
		considered := 0
		for _, c := range cands {
			if c.Usable() {
				considered++
			}
		}
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

// describe reads what agentwin.Choose needs to know about a window.
func describe(h uintptr) agentwin.Candidate {
	c := agentwin.Candidate{Handle: h}
	visible, _, _ := procIsWindowVisible.Call(h)
	c.Visible = visible != 0
	var r struct{ Left, Top, Right, Bottom int32 }
	if ok, _, _ := procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r))); ok != 0 {
		c.W, c.H = int64(r.Right-r.Left), int64(r.Bottom-r.Top)
	}
	owner, _, _ := procGetWindow.Call(h, gwOWNER)
	c.Owned = owner != 0
	ex, _, _ := procGetWindowLongW.Call(h, gwlEXSTYLE)
	c.Tool = ex&wsExTOOLWINDOW != 0
	iconic, _, _ := procIsIconic.Call(h)
	c.Minimized = iconic != 0
	return c
}

// resolver caches the game window's HWND. Which window, and when to look
// again, is agentwin.Tracker's call (tested there): the cached window is
// checked cheaply on every key, the tree re-walked only when it goes stale,
// at most every searchEvery while that finds nothing, and a valid
// prefix-wide choice compared with the others every recheckEvery, because
// valid only says it is alive: a launcher found before the game's window
// existed stayed valid, and kept every key (Kamino 202). With -title set and
// no window of that title, the prefix-wide fallback is kept the same way and
// the title looked for on the recheck, not on every key (Kamino 278).
type resolver struct {
	mu sync.Mutex
	t  agentwin.Tracker
}

// win32 is agentwin.Windows over the Win32 calls.
type win32 struct{}

func (win32) Alive(h uintptr) bool {
	ok, _, _ := procIsWindow.Call(h)
	return ok != 0
}

func (win32) Visible(h uintptr) bool {
	v, _, _ := procIsWindowVisible.Call(h)
	return v != 0
}

func (win32) Title(h uintptr) string       { return getWindowText(h) }
func (win32) ByTitle(title string) uintptr { return findByTitle(title) }
func (win32) Game(current uintptr) uintptr { return findGameWindow(current) }

// current returns a live HWND for the target window, re-walking if the cached
// one is stale. Returns 0 if the window can't be found right now.
func (r *resolver) current() uintptr {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := r.t.Current(time.Now(), win32{})
	if res.Replaced != 0 {
		logf("a better game window appeared (hwnd=0x%x, was 0x%x); delivering there", res.HWND, res.Replaced)
	}
	if res.Searched && res.HWND != 0 {
		if res.Dropped > 0 {
			logf("game window found (hwnd=0x%x); %d key(s) were dropped while it was missing", res.HWND, res.Dropped)
		}
		systemOnce.Do(exitWithThePrefix)
	}
	return res.HWND
}

// dropped counts a key that had no window to go to, and reports whether it is
// the first since the window went missing.
func (r *resolver) dropped() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.t.Pace.Drop()
}

// unmapped remembers the codes already reported as unmapped, so a key the
// agent cannot post is logged once and not once per press: the log is not a
// record of what was typed (REFERENCE.md 8). clonecast no longer sends such a
// key (Kamino 185); this is for an older one.
var unmapped sync.Map

// post delivers one frame to the game window. st is the session's key state:
// it sets the repeat bit on a held key's repeats and remembers what is down,
// for release (Kamino 136).
func post(r *resolver, st *agentkeys.State, f agentwire.Frame) {
	if !agentwire.Mappable(f.Code) {
		if _, seen := unmapped.LoadOrStore(f.Code, true); !seen {
			logf("unmapped evdev code %d, ignored", f.Code)
		}
		return
	}
	hwnd := r.current()
	if hwnd == 0 {
		if r.dropped() {
			logf("target window not found (gone?); dropping keys until it is back, looking at most every %s", searchEvery)
		}
		return
	}
	m, _ := st.Frame(f)
	procPostMessageW.Call(hwnd, uintptr(m.Msg), uintptr(m.VK), uintptr(m.LParam))
}

// release posts a key-up for every key this session still holds down. When
// the session ends no Up is coming for them from clonecast, and the game would
// keep them held: autorun, a character walking into a wall (Kamino 136).
func release(r *resolver, st *agentkeys.State) {
	ups := st.Release()
	if len(ups) == 0 {
		return
	}
	hwnd := r.current()
	if hwnd == 0 {
		logf("connection ended with %d key(s) down and no game window to release them in", len(ups))
		return
	}
	for _, m := range ups {
		procPostMessageW.Call(hwnd, uintptr(m.Msg), uintptr(m.VK), uintptr(m.LParam))
	}
	logf("connection ended with %d key(s) down; released them", len(ups))
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

// readToken reads the secret `clonecast agent install` wrote beside this exe;
// clonecast refuses an agent without it (Kamino item 2). Read once, at start.
func readToken() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(self), "token"))
	if err != nil {
		logf("no token beside the agent (%v); clonecast will refuse it until it is installed again", err)
		return ""
	}
	return strings.TrimSpace(string(b))
}

// token is this install's secret, sent in every hello.
var token string

// caption is hwnd's window text, or "" for no window.
func caption(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	return getWindowText(hwnd)
}

// hello is what this agent announces on every (re)connection. The title is the
// found window's caption (agentwire.AnnouncedTitle): it is what tells the game
// from a launcher in the same prefix when clonecast pairs (Kamino 137).
func hello(title string, hwnd uintptr, caption string) agentwire.Hello {
	self, _ := os.Executable()
	return agentwire.Hello{
		Token:   token,
		Version: agentwire.Version,
		// The prefix is what clonecast joins against the host pid of the KWin
		// window (REFERENCE.md 4.17). Wine hands the Unix environment to
		// Windows processes unchanged, so WINEPREFIX is set under every
		// launcher that sets it; plain `wine` does not, and then the prefix
		// comes from the agent's own path (Kamino 276). When neither gives
		// one, clonecast falls back to Title.
		Prefix: announcedPrefix(),
		Title:  agentwire.AnnouncedTitle(title, caption),
		Exe:    self,
		PID:    os.Getpid(),
		HWND:   uint64(hwnd),
	}
}

// announcedPrefix is the prefix this agent announces, worked out once: its
// exe never moves while it runs.
var announcedPrefix = sync.OnceValue(func() string {
	env := os.Getenv("WINEPREFIX")
	if strings.TrimSpace(env) != "" {
		return env
	}
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	p := agentwire.AnnouncedPrefix(env, unixFileName(self))
	if p == "" {
		logf("no WINEPREFIX and %s is not an installed agent's path: clonecast will pair this agent by title only", self)
	}
	return p
})

// unixFileName is the host path of a Windows path, through Wine's own
// kernel32 export wine_get_unix_file_name (the call winepath -u makes), or ""
// when it fails or this is not Wine. The export is CDECL, which Go's syscall
// calls correctly: the stub restores the stack pointer after the call. Wine
// allocates the returned char* with HeapAlloc on the process heap and
// documents that the caller frees it, so it is copied and HeapFree'd.
func unixFileName(win string) string {
	if procWineGetUnixFileName.Find() != nil {
		return "" // real Windows: no such export
	}
	w, err := syscall.UTF16PtrFromString(win)
	if err != nil {
		return ""
	}
	r, _, _ := procWineGetUnixFileName.Call(uintptr(unsafe.Pointer(w)))
	if r == 0 {
		return ""
	}
	// Reinterpreting through &r rather than unsafe.Pointer(r) keeps vet's
	// uintptr check quiet; the memory is Wine's heap, which Go never moves.
	s := cString(*(*unsafe.Pointer)(unsafe.Pointer(&r)))
	if heap, _, _ := procGetProcessHeap.Call(); heap != 0 {
		procHeapFree.Call(heap, 0, r)
	}
	return s
}

// cString copies a NUL-terminated C string. Wine's Unix paths are bounded by
// PATH_MAX, so the scan is capped there as a guard against a missing NUL.
func cString(p unsafe.Pointer) string {
	const limit = 4096
	var b []byte
	for i := 0; i < limit; i++ {
		c := *(*byte)(unsafe.Add(p, i))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b)
}

// session runs one connection to clonecast: announce, keep alive, and post
// every frame that arrives until the connection ends. It returns when the
// connection is over; the caller redials.
func session(c net.Conn, res *resolver, title string) {
	defer c.Close()
	w := &conn{c: c}

	// Whatever ends the session, its keys still down are released: deferred
	// after c.Close, so it runs first.
	var st agentkeys.State
	defer release(res, &st)

	announced := res.current()
	announcedCaption := caption(announced)
	if err := w.writeHello(hello(title, announced, announcedCaption)); err != nil {
		logf("hello: %v", err)
		return
	}
	logf("registered with clonecast at %s (hwnd=0x%x, prefix=%q)", c.RemoteAddr(), announced, announcedPrefix())

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
				// Re-announce when the game recreated its window or its
				// caption changed: clonecast pairs on the caption (Kamino
				// 137), and the HWND, though diagnostic, sends people hunting
				// the wrong problem when stale.
				if h := res.current(); h != 0 {
					if text := caption(h); h != announced || text != announcedCaption {
						announced, announcedCaption = h, text
						if err := w.writeHello(hello(title, h, text)); err != nil {
							return // the read loop will see the same failure
						}
						logf("window changed, re-announced hwnd=0x%x", h)
						continue
					}
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
			post(res, &st, m.Frame)
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
	token = readToken()

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
	//
	// Recheck.Every was never set before Kamino 278, so the "every 5 s"
	// recheck ran on every key: a full tree walk per key in prefix mode too.
	res := &resolver{t: agentwin.Tracker{
		Title:   *title,
		Pace:    lookup.Pacer{Every: searchEvery},
		Recheck: agentwin.Recheck{Every: recheckEvery},
	}}
	// Look for the game window with or without clonecast, so the agent
	// becomes a system process (exitWithThePrefix) and never keeps the prefix
	// up after the game, even when clonecast never ran.
	go func() {
		for res.current() == 0 {
			time.Sleep(agentwire.KeepaliveInterval)
		}
	}()
	connectLoop(addr, res, *title)
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
