//go:build darwin || linux

package tui

import (
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/platform/mock"
)

// fakeAgents is a registry stand-in: one agent, covering the window whose id
// is in paired. cmd/clonecast passes the real *agent.Registry.
type fakeAgents struct {
	mu     sync.Mutex
	addr   string
	paired map[broadcast.WindowID]bool
}

func (f *fakeAgents) Addr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

func (f *fakeAgents) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.paired)
}

func (f *fakeAgents) Paired(id broadcast.WindowID) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.paired[id]
}

func (f *fakeAgents) Listen(addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addr = addr
	return nil
}

// terminal is the TUI running on a real pseudo-terminal: Bubble Tea renders as
// it would for a user (it checks for a TTY), and the test types into it.
type terminal struct {
	t      *testing.T
	master *os.File

	mu  sync.Mutex
	buf strings.Builder
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]|\x1b\\][^\x07]*\x07|\x1b[()][A-Z0-9]")

// text is everything rendered so far, with escape sequences removed so
// assertions read like what a user sees.
func (tm *terminal) text() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return ansi.ReplaceAllString(tm.buf.String(), "")
}

// typeKeys sends keystrokes the way a user would: one at a time, with a pause
// between them. Writing several in one go is not the same thing — Bubble Tea
// coalesces runes that arrive together into a single multi-rune key message
// (a paste), which matches none of the single-key bindings.
func (tm *terminal) typeKeys(s string) {
	tm.t.Helper()
	for _, r := range s {
		if _, err := tm.master.WriteString(string(r)); err != nil {
			tm.t.Fatalf("write %q to the terminal: %v", r, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitFor blocks until the rendered screen contains want.
func (tm *terminal) waitFor(what, want string) {
	tm.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(tm.text(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tm.t.Fatalf("timed out waiting for %s (%q) on screen. Last frame:\n%s", what, want, lastFrame(tm.text()))
}

// waitUntil blocks until cond holds, reporting the screen if it never does.
func (tm *terminal) waitUntil(what string, cond func() bool) {
	tm.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	tm.t.Fatalf("timed out waiting for %s. Last frame:\n%s", what, lastFrame(tm.text()))
}

func lastFrame(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	return strings.Join(lines, "\n")
}

func setWinsize(t *testing.T, f *os.File, rows, cols uint16) {
	t.Helper()
	ws := struct{ Row, Col, X, Y uint16 }{rows, cols, 0, 0}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		t.Fatalf("set window size: %v", e)
	}
}

// startTUI runs the model on a pty and returns the terminal to drive it with.
func startTUI(t *testing.T, m Model) *terminal {
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("no pty available here: %v", err)
	}
	setWinsize(t, slave, 40, 120)

	tm := &terminal{t: t, master: master}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := master.Read(b)
			if n > 0 {
				tm.mu.Lock()
				tm.buf.Write(b[:n])
				tm.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	prog := tea.NewProgram(m, tea.WithInput(slave), tea.WithOutput(slave))
	done := make(chan error, 1)
	go func() { _, err := prog.Run(); done <- err }()
	t.Cleanup(func() {
		prog.Quit()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			prog.Kill()
		}
		_ = slave.Close()
		_ = master.Close()
	})
	return tm
}

// TestTUITickingIsEnough drives the real TUI on a pseudo-terminal against the
// mock backend and checks the property REFERENCE.md 4.17 is for: ticking a
// window is the whole of target selection. Nothing in this test configures an
// endpoint, a port or a title map — the row is marked as covered because an
// agent registered, and ticking it is the only action.
func TestTUITickingIsEnough(t *testing.T) {
	wm := mock.NewWM()
	engine := broadcast.New(mock.NewSource(time.Hour), &mock.Injector{}, wm, broadcast.DefaultConfig())
	agents := &fakeAgents{addr: "127.0.0.1:48800", paired: map[broadcast.WindowID]bool{"w3": true}}

	tm := startTUI(t, New(engine, wm, nil, agents))
	tm.waitFor("the window list", "Game — instance 1")

	// The header and the Settings pane both surface the registry: how many
	// agents are connected and where they connect.
	tm.waitFor("the agent count in the header", "agents: 1")
	tm.waitFor("the listen address in Settings", "127.0.0.1:48800")
	tm.waitFor("the agent count in Settings", "1 agent")

	// The covered window carries the marker; nothing else does.
	screen := tm.text()
	covered := rowFor(t, screen, "Game — instance 1")
	if !strings.Contains(covered, AgentMark) {
		t.Errorf("the row with a live agent has no %q marker:\n%s", AgentMark, covered)
	}
	if other := rowFor(t, screen, "Firefox"); strings.Contains(other, AgentMark) {
		t.Errorf("a window with no agent is marked as covered:\n%s", other)
	}

	// Tick the covered window: two "j"s from the top row, then space. That is
	// the entire interaction — no flags, no map.
	tm.typeKeys("jj ")
	tm.waitUntil("the ticked window to reach the engine", func() bool {
		got := engine.Targets()
		return len(got) == 1 && got[0] == "w3"
	})

	// Tick a window with no agent: the row says so in place, not only in the
	// log (which scrolls away).
	tm.typeKeys("k ")
	tm.waitUntil("both windows ticked", func() bool { return len(engine.Targets()) == 2 })
	tm.waitFor("the in-place warning on the agent-less row", "no agent")

	// And the marker survives the ticking: a ticked, covered row keeps it.
	tm.waitUntil("the ticked covered row to keep its marker", func() bool {
		row := rowFor(t, tm.text(), "Game — instance 1")
		return strings.Contains(row, "[x]") && strings.Contains(row, AgentMark)
	})
}

// TestTUIKeysStillWork guards the existing bindings the agent work must not
// regress: `a` ticks everything, `f` makes a window the master (M marker),
// `g` flips the origin gate, and the Settings pane still cycles.
func TestTUIKeysStillWork(t *testing.T) {
	wm := mock.NewWM()
	engine := broadcast.New(mock.NewSource(time.Hour), &mock.Injector{}, wm, broadcast.DefaultConfig())
	agents := &fakeAgents{addr: "127.0.0.1:48800", paired: map[broadcast.WindowID]bool{"w3": true}}

	tm := startTUI(t, New(engine, wm, nil, agents))
	tm.waitFor("the window list", "Game — instance 2")

	// `a` on the Targets pane ticks every window.
	tm.typeKeys("a")
	tm.waitUntil("every window ticked by `a`", func() bool { return len(engine.Targets()) == 5 })
	tm.typeKeys("a")
	tm.waitUntil("every window unticked by `a`", func() bool { return len(engine.Targets()) == 0 })

	// `f` activates the highlighted window, which becomes the master.
	tm.typeKeys("jjf")
	tm.waitUntil("the master to move to the highlighted window", func() bool {
		id, err := wm.Active(t.Context())
		return err == nil && id == "w3"
	})
	tm.waitFor("the master line", "master: Game — instance 1")

	// `g` flips the origin gate from any pane.
	gate := engine.GateOrigin()
	tm.typeKeys("g")
	tm.waitUntil("the origin gate to flip", func() bool { return engine.GateOrigin() != gate })

	// The Settings pane still changes settings: tab twice to it, move to the
	// settle row and step it to the next preset.
	before := engine.Config().SettleDelay
	tm.typeKeys("\t\tjj ")
	tm.waitUntil("the settle delay to change", func() bool { return engine.Config().SettleDelay != before })
}

// rowFor returns the last rendered line containing want, i.e. that window's
// row as it currently looks.
func rowFor(t *testing.T, screen, want string) string {
	t.Helper()
	var got string
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, want) {
			got = line
		}
	}
	if got == "" {
		t.Fatalf("no row for %q on screen:\n%s", want, lastFrame(screen))
	}
	return got
}
