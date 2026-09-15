package agent

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
	"github.com/malahmen/clonecast/internal/prefix"
)

// fakeProc writes a procfs-shaped tree: <root>/<pid>/environ with a
// NUL-separated environment block, exactly as PrefixOfPIDIn reads it.
func fakeProc(t *testing.T, env map[int][]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, kv := range env {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if kv == nil {
			continue // a process whose environ we may not read
		}
		blob := strings.Join(kv, "\x00") + "\x00"
		if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(blob), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// testClient is a real TCP client standing in for an in-bottle agent: it
// dials, says hello and reads frames. The registry is driven through a real
// socket on purpose — the whole point of 4.17 is the connection direction, and
// a mock would not exercise it.
type testClient struct {
	t    *testing.T
	conn net.Conn
	sc   *bufio.Scanner
}

func dialAgent(t *testing.T, addr string, h agentwire.Hello) *testClient {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if err := h.Write(c); err != nil {
		t.Fatalf("hello: %v", err)
	}
	return &testClient{t: t, conn: c, sc: bufio.NewScanner(c)}
}

// nextFrame reads one frame, failing the test if none arrives.
func (c *testClient) nextFrame() agentwire.Frame {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	for c.sc.Scan() {
		msg, err := agentwire.Decode(c.sc.Text())
		if err != nil {
			c.t.Fatalf("decode %q: %v", c.sc.Text(), err)
		}
		if msg.Kind == agentwire.KindFrame {
			return msg.Frame
		}
	}
	c.t.Fatalf("no frame: %v", c.sc.Err())
	return agentwire.Frame{}
}

// waitFor polls cond until it holds, so tests never sleep on a fixed guess.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func startRegistry(t *testing.T, wins []broadcast.Window, procRoot string) (*Registry, context.CancelFunc) {
	t.Helper()
	reg := NewRegistry(Options{
		Windows: func(context.Context) ([]broadcast.Window, error) {
			return append([]broadcast.Window(nil), wins...), nil
		},
		PrefixOf: func(pid int) (string, error) { return prefix.PrefixOfPIDIn(procRoot, pid) },
		Notify:   func(string, bool) {},
	})
	if err := reg.Listen("127.0.0.1:0"); err != nil { // :0 — never collide with a real clonecast
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go reg.Run(ctx)
	t.Cleanup(func() {
		cancel()
		_ = reg.Close()
	})
	return reg, cancel
}

// TestRegistryPairsByPrefixAndDeliversFrames is the end-to-end shape of
// REFERENCE.md 4.17: an agent dials in, announces its WINEPREFIX, is paired to
// the KWin window whose host pid runs in that prefix, receives keys, and
// disappears from the registry when it disconnects.
func TestRegistryPairsByPrefixAndDeliversFrames(t *testing.T) {
	bottleA, bottleB := t.TempDir(), t.TempDir()
	procRoot := fakeProc(t, map[int][]string{
		5200: {"LANG=C", "WINEPREFIX=" + bottleA},
		5300: {"WINEPREFIX=" + bottleB},
		4242: {"HOME=/home/u"}, // the terminal: a real pid, no prefix
	})
	wins := []broadcast.Window{
		{ID: "w1", Title: "clonecast — konsole", PID: 4242},
		// Both game windows carry the SAME caption on purpose: title
		// matching cannot tell them apart, the prefix join can.
		{ID: "w2", Title: "World of Warcraft", PID: 5200},
		{ID: "w3", Title: "World of Warcraft", PID: 5300},
	}
	reg, _ := startRegistry(t, wins, procRoot)

	a := dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Prefix: bottleA, Title: "World of Warcraft", PID: 11, HWND: 0xabc})
	b := dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Prefix: bottleB, Title: "World of Warcraft", PID: 12, HWND: 0xdef})

	waitFor(t, "two agents registered and paired", func() bool {
		return reg.Count() == 2 && reg.Paired("w2") && reg.Paired("w3")
	})
	if reg.Paired("w1") {
		t.Error("the terminal window must not be paired with an agent")
	}

	infos := reg.Agents()
	if len(infos) != 2 {
		t.Fatalf("Agents() = %d entries, want 2", len(infos))
	}
	byWindow := map[broadcast.WindowID]Info{}
	for _, in := range infos {
		byWindow[in.Window] = in
	}
	if byWindow["w2"].Prefix != bottleA || byWindow["w3"].Prefix != bottleB {
		t.Fatalf("pairing crossed the prefixes: %+v", infos)
	}

	// Deliver a key to both game windows with w2 as the master (origin), and
	// check the frames arrive on the right sockets and not on the master's.
	d := New(reg)
	var notices []string
	space, err := keys.Parse("space")
	if err != nil {
		t.Fatal(err)
	}
	n, err := d.Deliver(context.Background(), broadcast.Delivery{
		Event:   keys.Event{Code: space, State: keys.Down},
		Targets: []broadcast.WindowID{"w1", "w2", "w3"},
		Origin:  "w2",
		Notify:  func(s string, _ bool) { notices = append(notices, s) },
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n != 1 {
		t.Fatalf("delivered to %d targets, want 1 (w3 only: w2 is the master, w1 has no agent)", n)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "no agent has registered") {
		t.Fatalf("want one actionable notice about the agent-less target, got %q", notices)
	}
	if got := b.nextFrame(); got != (agentwire.Frame{Code: uint16(space), Down: true}) {
		t.Fatalf("agent B got %+v, want space down", got)
	}

	// The master's agent must be untouched: passthrough already typed there.
	_ = a.conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if a.sc.Scan() {
		t.Fatalf("agent A (the master) received %q; delivery must skip the origin", a.sc.Text())
	}

	// Disconnect: the agent leaves the registry and its window stops being
	// marked as covered.
	_ = b.conn.Close()
	waitFor(t, "the disconnected agent to be dropped", func() bool {
		return reg.Count() == 1 && !reg.Paired("w3")
	})
	if !reg.Paired("w2") {
		t.Error("the surviving agent lost its pairing when the other one left")
	}
}

// TestRegistryTitleFallback covers the path that keeps today's behaviour
// working where the pid join cannot: no pid from the compositor, or an environ
// we cannot read.
func TestRegistryTitleFallback(t *testing.T) {
	procRoot := fakeProc(t, map[int][]string{
		777: nil, // the process exists but its environ cannot be read
	})
	wins := []broadcast.Window{
		{ID: "w1", Title: "Malahmen", PID: 0},   // no pid at all (no XRes)
		{ID: "w2", Title: "Marx", PID: 777},     // pid known, environ unreadable
		{ID: "w3", Title: "Firefox", PID: 4242}, // nothing to pair with
	}
	reg, _ := startRegistry(t, wins, procRoot)

	dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Prefix: "/bottles/Malahmen", Title: "Malahmen"})
	dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Marx"}) // no WINEPREFIX in its env either

	waitFor(t, "both agents paired by title", func() bool {
		return reg.Paired("w1") && reg.Paired("w2")
	})
	if reg.Paired("w3") {
		t.Error("Firefox must not be paired")
	}
}

// TestRegistryRejectsBadHello: a connection that does not open with a hello
// this build understands is closed, and never becomes a target.
func TestRegistryRejectsBadHello(t *testing.T) {
	reg, _ := startRegistry(t, nil, t.TempDir())

	for _, first := range []string{
		"H 99 prefix=/x\n", // a protocol version we do not speak
		"D 57\n",           // a frame where the hello belongs
		"garbage\n",
	} {
		c, err := net.DialTimeout("tcp", reg.Addr(), 2*time.Second)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if _, err := fmt.Fprint(c, first); err != nil {
			t.Fatalf("write %q: %v", first, err)
		}
		// The registry hangs up; a read must end rather than block.
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 1)
		if _, err := c.Read(buf); err == nil {
			t.Errorf("first line %q: connection stayed open", first)
		}
		_ = c.Close()
	}
	if reg.Count() != 0 {
		t.Fatalf("registry accepted %d bad connections", reg.Count())
	}
}

// TestRegistryRebind: the listen address is a runtime setting (the TUI's
// Settings pane), so rebinding must keep the agents that are already
// connected and start accepting on the new address.
func TestRegistryRebind(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game", PID: 0}}
	reg, _ := startRegistry(t, wins, t.TempDir())
	old := reg.Addr()

	dialAgent(t, old, agentwire.Hello{Version: agentwire.Version, Title: "Game"})
	waitFor(t, "the first agent", func() bool { return reg.Count() == 1 && reg.Paired("w1") })

	if err := reg.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if reg.Addr() == old {
		t.Fatal("rebind kept the old address")
	}
	if reg.Count() != 1 {
		t.Fatal("rebind dropped a connected agent")
	}
	if _, err := net.DialTimeout("tcp", old, 300*time.Millisecond); err == nil {
		t.Error("the old listener is still accepting")
	}

	// A second agent on the new address: two windows, two agents.
	dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Game 2"})
	waitFor(t, "the second agent", func() bool { return reg.Count() == 2 })
}

// TestRegistryDropsAgentOnSendFailure: when a send fails the agent is dropped
// rather than retried forever, and the window stops claiming to be covered.
func TestRegistryDropsAgentOnSendFailure(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	reg, _ := startRegistry(t, wins, t.TempDir())
	c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Game"})
	waitFor(t, "the agent", func() bool { return reg.Paired("w1") })

	a := reg.agentFor("w1")
	if a == nil {
		t.Fatal("no agent for w1")
	}
	_ = c.conn.Close()
	_ = a.conn.Close() // the socket is dead from clonecast's side too

	d := New(reg)
	code, _ := keys.Parse("a")
	var sawErr bool
	n, err := d.Deliver(context.Background(), broadcast.Delivery{
		Event:   keys.Event{Code: code, State: keys.Down},
		Targets: []broadcast.WindowID{"w1"},
		Origin:  "w9",
		Notify:  func(_ string, isErr bool) { sawErr = sawErr || isErr },
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if n != 0 || !sawErr {
		t.Fatalf("delivered %d with error reported = %v; want 0 and a reported error", n, sawErr)
	}
	waitFor(t, "the dead agent to be dropped", func() bool { return reg.Count() == 0 && !reg.Paired("w1") })
}

// TestRegistryKeepaliveKeepsAgent: an idle but live agent stays registered,
// and its keepalives are accepted (not mistaken for frames or garbage).
func TestRegistryKeepaliveKeepsAgent(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	reg, _ := startRegistry(t, wins, t.TempDir())
	c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Game"})
	waitFor(t, "the agent", func() bool { return reg.Count() == 1 })

	for i := 0; i < 3; i++ {
		if err := agentwire.WriteKeepalive(c.conn); err != nil {
			t.Fatalf("keepalive %d: %v", i, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reg.Count() != 1 || !reg.Paired("w1") {
		t.Fatalf("keepalives cost the agent its registration (count=%d)", reg.Count())
	}
}
