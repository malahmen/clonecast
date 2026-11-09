package agent

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// testToken is the token startRegistry's registry holds.
const testToken = "t3st-token"

// dialAgent connects as a genuine agent at h.Version: a v2 hello is sent as
// it is (with whatever token it carries), and a v3 one runs the agent's own
// handshake (agentwire.AgentAuth) with testToken, failing the test if that
// does not complete.
func dialAgent(t *testing.T, addr string, h agentwire.Hello) *testClient {
	t.Helper()
	c := dialRaw(t, addr)
	if h.Version == 2 {
		if err := h.Write(c.conn); err != nil {
			t.Fatalf("hello: %v", err)
		}
		return c
	}
	auth, err := agentwire.NewAgentAuth(testToken, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h.Version, h.Token, h.Nonce = 3, "", auth.Nonce()
	if err := h.Write(c.conn); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := auth.Run(c.sc, c.conn); err != nil {
		t.Fatalf("v3 handshake: %v", err)
	}
	_ = c.conn.SetReadDeadline(time.Time{})
	return c
}

func dialRaw(t *testing.T, addr string) *testClient {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &testClient{t: t, conn: c, sc: bufio.NewScanner(c)}
}

// dialFakeV3 is a v3 agent with the wrong token. It cannot check the
// challenge, so it answers it with the MAC its token gives, computed here
// from the documented format rather than through agentwire. It returns the
// challenge it was sent.
func dialFakeV3(t *testing.T, addr, token string, h agentwire.Hello) (*testClient, agentwire.Challenge) {
	t.Helper()
	c := dialRaw(t, addr)
	nonce, _ := agentwire.NewNonce(rand.Reader)
	h.Version, h.Nonce = 3, nonce
	if err := h.Write(c.conn); err != nil {
		t.Fatalf("hello: %v", err)
	}
	ch := c.challenge()
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("agent|" + ch.Nonce + "|" + nonce))
	if err := (agentwire.Response{MAC: hex.EncodeToString(m.Sum(nil))}).Write(c.conn); err != nil {
		t.Fatalf("response: %v", err)
	}
	return c, ch
}

// challenge reads the registry's C line.
func (c *testClient) challenge() agentwire.Challenge {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if !c.sc.Scan() {
		c.t.Fatalf("no challenge: %v", c.sc.Err())
	}
	m, err := agentwire.Decode(c.sc.Text())
	if err != nil || m.Kind != agentwire.KindChallenge {
		c.t.Fatalf("want a challenge, got %q (%v)", c.sc.Text(), err)
	}
	_ = c.conn.SetReadDeadline(time.Time{})
	return m.Challenge
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
	return startRegistryNotify(t, wins, procRoot, func(string, bool) {})
}

func startRegistryNotify(t *testing.T, wins []broadcast.Window, procRoot string, notify func(string, bool)) (*Registry, context.CancelFunc) {
	t.Helper()
	reg := NewRegistry(Options{
		Windows: func(context.Context) ([]broadcast.Window, error) {
			return append([]broadcast.Window(nil), wins...), nil
		},
		PrefixOf: func(pid int) (string, error) { return prefix.PrefixOfPIDIn(procRoot, pid) },
		Notify:   notify,
		Token:    testToken,
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
	reached, err := d.Deliver(context.Background(), broadcast.Delivery{
		Event:   keys.Event{Code: space, State: keys.Down},
		Targets: []broadcast.WindowID{"w1", "w2", "w3"},
		Origin:  "w2",
		Notify:  func(s string, _ bool) { notices = append(notices, s) },
	})
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if len(reached) != 1 || reached[0] != "w3" {
		t.Fatalf("reached %v, want [w3] (w2 is the master, w1 has no agent)", reached)
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
	var mu sync.Mutex
	var sawErr bool
	reg, _ := startRegistryNotify(t, wins, t.TempDir(), func(s string, isErr bool) {
		mu.Lock()
		defer mu.Unlock()
		sawErr = sawErr || (isErr && strings.Contains(s, "dropped"))
	})
	dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Game"})
	waitFor(t, "the agent", func() bool { return reg.Paired("w1") })

	a := reg.agentFor("w1")
	if a == nil {
		t.Fatal("no agent for w1")
	}
	// Writes fail while the read side stays up, so the writer, not the read
	// loop, is what notices.
	if err := a.conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}

	d := New(reg)
	code, _ := keys.Parse("a")
	if _, err := d.Deliver(context.Background(), broadcast.Delivery{
		Event:   keys.Event{Code: code, State: keys.Down},
		Targets: []broadcast.WindowID{"w1"},
		Origin:  "w9",
		Notify:  func(string, bool) {},
	}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	waitFor(t, "the dead agent to be dropped and reported", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return sawErr && reg.Count() == 0 && !reg.Paired("w1")
	})
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

// TestRegistryHelloRaceOnReannounce: an agent re-announces whenever WoW
// recreates its top-level window (REFERENCE.md 4.14) — every login and every
// loading screen — while the pairing pass and the TUI's snapshot are reading
// what it announced. Run with -race this reported a write/read data race:
// the write took r.mu and the readers (Agents, Label, both pairing passes)
// did not.
//
// Nothing here asserts a value: -race is the assertion. It fails the test by
// itself when the field is unsynchronised.
func TestRegistryHelloRaceOnReannounce(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	reg, _ := startRegistry(t, wins, t.TempDir())
	c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Game"})
	waitFor(t, "the agent", func() bool { return reg.Count() == 1 })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// The agent, re-announcing as fast as the socket takes it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			h := agentwire.Hello{
				Version: agentwire.Version,
				Title:   "Game",
				Prefix:  fmt.Sprintf("/bottles/b%d", i%3),
				PID:     i,
				HWND:    uint64(i),
			}
			if err := h.Write(c.conn); err != nil {
				return
			}
		}
	}()

	// clonecast, reading it: the TUI's snapshot, the log label, and the
	// pairing pass the repair loop runs.
	for _, read := range []func(){
		func() { _ = reg.Agents() },
		func() { _ = reg.Paired("w1") },
		func() {
			if a := reg.agentFor("w1"); a != nil {
				_ = a.Label()
			}
		},
	} {
		wg.Add(1)
		go func(fn func()) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				fn()
			}
		}(read)
	}

	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestAgentHelloIsReadAsOneValue: Hello() must hand back a snapshot, so a
// caller comparing two of its fields cannot see half of one announcement and
// half of the next — a pairing decision about an agent that never existed.
func TestAgentHelloIsReadAsOneValue(t *testing.T) {
	a := &Agent{ID: 1}
	if got := a.Hello(); got.Title != "" || got.Prefix != "" {
		t.Fatalf("a never-announced agent should read as a zero Hello, got %+v", got)
	}
	first := agentwire.Hello{Version: agentwire.Version, Title: "A", Prefix: "/bottles/a", PID: 1}
	a.setHello(first)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20000; i++ {
			a.setHello(agentwire.Hello{Version: agentwire.Version, Title: "B", Prefix: "/bottles/b", PID: 2})
			a.setHello(first)
		}
	}()
	for i := 0; i < 20000; i++ {
		h := a.Hello()
		// Every stored value is internally consistent; a torn read would
		// pair one announcement's title with another's prefix.
		if (h.Title == "A") != (h.Prefix == "/bottles/a") {
			t.Fatalf("torn Hello: %+v", h)
		}
	}
	<-done
}

// Kamino item 44: sends used to be sequential, so an agent that stopped
// reading held every other agent's keys for up to its 2 s write deadline.
func TestAStuckAgentDoesNotDelayTheOthers(t *testing.T) {
	stuckEnd, stuckPeer := net.Pipe() // nobody reads stuckPeer: writes block
	okEnd, okPeer := net.Pipe()
	t.Cleanup(func() { _ = stuckEnd.Close(); _ = stuckPeer.Close(); _ = okEnd.Close(); _ = okPeer.Close() })
	stuck, ok := newAgent(1, stuckEnd), newAgent(2, okEnd)
	go stuck.run(func(error) {})
	go ok.run(func(error) {})
	t.Cleanup(func() { stuck.stop(); ok.stop() })

	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(okPeer).ReadString('\n')
		got <- line
	}()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if !stuck.enqueue(agentwire.Frame{Code: 30, Down: true}) {
			t.Fatal("enqueue refused with room in the queue")
		}
	}
	ok.enqueue(agentwire.Frame{Code: 30, Down: true})
	select {
	case line := <-got:
		if line != "D 30\n" {
			t.Errorf("the healthy agent got %q", line)
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("the healthy agent waited %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("the healthy agent got nothing while another was stuck")
	}
}

func TestAFullQueueIsRefused(t *testing.T) {
	end, peer := net.Pipe()
	t.Cleanup(func() { _ = end.Close(); _ = peer.Close() })
	a := newAgent(1, end) // no writer: nothing leaves the queue
	for i := 0; i < queueLen; i++ {
		if !a.enqueue(agentwire.Frame{Code: 30}) {
			t.Fatalf("frame %d refused", i)
		}
	}
	if a.enqueue(agentwire.Frame{Code: 30}) {
		t.Error("a frame past the queue's length was accepted")
	}
	a.stop()
	<-a.out
	if a.enqueue(agentwire.Frame{Code: 30}) {
		t.Error("a frame was accepted for a removed agent")
	}
}

// Kamino item 2: any local process could register as an agent and be sent
// every broadcast key. Only an agent sending the install's token registers.
func TestRegistryRefusesAnAgentWithoutTheToken(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	reg := NewRegistry(Options{Token: "s3cret", Protocol: 2, Notify: func(s string, _ bool) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, s)
	}})
	if err := reg.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	var held []*testClient
	for _, tok := range []string{"", "", "b4d-t0ken"} {
		c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: 2, Title: "Game", Token: tok})
		held = append(held, c)
		if reg.Count() != 0 {
			t.Fatalf("token %q: the agent registered", tok)
		}
	}
	waitFor(t, "the refusals", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(lines) >= 2
	})
	dialAgent(t, reg.Addr(), agentwire.Hello{Version: 2, Title: "Game", Token: "s3cret"})
	waitFor(t, "the agent with the token", func() bool { return reg.Count() == 1 })

	// Held open, not closed: an agent built before 2026-10-09 redials at
	// once when a connection ends, about 500 times a second.
	for _, c := range held {
		_ = c.conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if c.sc.Scan() {
			t.Fatalf("a refused agent was sent %q", c.sc.Text())
		}
		var ne net.Error
		if !errors.As(c.sc.Err(), &ne) || !ne.Timeout() {
			t.Fatalf("a refused connection was closed (%v); want it held open", c.sc.Err())
		}
	}
	_ = reg.Close()
	for _, c := range held {
		// Read the conn itself: the scanner keeps its timeout error.
		_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("Close left a refused connection open (%v)", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	refusals := 0
	for _, l := range lines {
		if strings.Contains(l, "refused") {
			refusals++
		}
		if strings.Contains(l, "s3cret") || strings.Contains(l, "b4d-t0ken") {
			t.Errorf("a token reached the log: %q", l)
		}
	}
	if refusals != 2 {
		t.Errorf("%d refusals logged, want 2 (no token once, the wrong token once): %q", refusals, lines)
	}
}

func TestRegistryListensOnLoopbackOnly(t *testing.T) {
	reg := NewRegistry(Options{})
	t.Cleanup(func() { _ = reg.Close() })
	for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0", "192.0.2.1:48800"} {
		if err := reg.Listen(addr); err == nil {
			t.Errorf("listened on %s", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		if err := loopbackOnly(addr); err != nil {
			t.Errorf("%s refused: %v", addr, err)
		}
	}
}

// Kamino 202: the engine releases held keys at quit just before the registry
// closes. Close used to close every socket at once, under the writers, and
// those Ups were lost. The writer is held off (its send lock taken) so the
// frame is certainly still queued when Close starts.
func TestCloseSendsWhatIsQueued(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	reg, _ := startRegistry(t, wins, t.TempDir())
	c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Game"})
	waitFor(t, "the agent", func() bool { return reg.Paired("w1") })
	a := reg.agentFor("w1")

	a.mu.Lock()
	if !a.enqueue(agentwire.Frame{Code: 17, Down: false}) {
		a.mu.Unlock()
		t.Fatal("enqueue refused")
	}
	closed := make(chan struct{})
	go func() { _ = reg.Close(); close(closed) }()
	time.Sleep(50 * time.Millisecond)
	a.mu.Unlock()

	if got := c.nextFrame(); got != (agentwire.Frame{Code: 17, Down: false}) {
		t.Fatalf("agent got %+v, want the queued W up", got)
	}
	<-closed
}

// refusedClient dials as an agent the registry refuses for its token, at
// protocol proto: a v2 hello without one, or a v3 agent with the wrong one
// (Kamino 275: the cap and the deadline hold for both).
func refusedClient(t *testing.T, reg *Registry, proto int) *testClient {
	t.Helper()
	if proto == 2 {
		return dialAgent(t, reg.Addr(), agentwire.Hello{Version: 2, Title: "Game"})
	}
	c, _ := dialFakeV3(t, reg.Addr(), "b4d-t0ken", agentwire.Hello{Title: "Game"})
	return c
}

// closedWithin reports whether the registry closes c within d.
func closedWithin(c *testClient, d time.Duration) bool {
	_ = c.conn.SetReadDeadline(time.Now().Add(d))
	_, err := c.conn.Read(make([]byte, 1))
	return errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || (err != nil && !isTimeout(err))
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// Kamino 139: refused connections were held open with no limit, a goroutine
// and a socket each for as long as the peer liked. Past the cap a refusal is
// closed at once; the ones held stay held.
func TestRefusedConnectionsAreCapped(t *testing.T) {
	for _, proto := range []int{2, 3} {
		t.Run(fmt.Sprintf("v%d", proto), func(t *testing.T) { testRefusedConnectionsAreCapped(t, proto) })
	}
}

func testRefusedConnectionsAreCapped(t *testing.T, proto int) {
	reg := NewRegistry(Options{Token: "s3cret", Protocol: proto})
	reg.maxHeld = 2
	if err := reg.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	first := []*testClient{refusedClient(t, reg, proto), refusedClient(t, reg, proto)}
	waitFor(t, "two refusals held", func() bool {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return len(reg.held) == 2
	})
	extra := refusedClient(t, reg, proto)
	if !closedWithin(extra, 2*time.Second) {
		t.Fatal("a refusal past the cap was held open")
	}
	for i, c := range first {
		if closedWithin(c, 100*time.Millisecond) {
			t.Fatalf("held refusal %d was closed by the one past the cap", i)
		}
	}
	reg.mu.Lock()
	n := len(reg.held)
	reg.mu.Unlock()
	if n != 2 {
		t.Fatalf("%d refusals held, want 2", n)
	}
}

// Kamino 139: a held refusal has a deadline. A peer that never closes, or
// streams bytes at the registry, is closed when it passes.
func TestRefusedConnectionsHaveADeadline(t *testing.T) {
	for _, proto := range []int{2, 3} {
		t.Run(fmt.Sprintf("v%d", proto), func(t *testing.T) { testRefusedConnectionsHaveADeadline(t, proto) })
	}
}

func testRefusedConnectionsHaveADeadline(t *testing.T, proto int) {
	reg := NewRegistry(Options{Token: "s3cret", Protocol: proto})
	reg.holdFor = 300 * time.Millisecond
	if err := reg.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	quiet := refusedClient(t, reg, proto)
	chatty := refusedClient(t, reg, proto)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				if _, err := chatty.conn.Write([]byte("keepalive\n")); err != nil {
					return
				}
			}
		}
	}()
	if closedWithin(quiet, 50*time.Millisecond) {
		t.Fatal("a refusal was closed at once; want it held until the deadline")
	}
	for name, c := range map[string]*testClient{"quiet": quiet, "chatty": chatty} {
		if !closedWithin(c, 3*time.Second) {
			t.Fatalf("the %s refusal was still open 3s after a 300ms hold", name)
		}
	}
	waitFor(t, "no refusals held", func() bool {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		return len(reg.held) == 0
	})
}

// A key no agent can post is not sent and not reported reached: the agent
// would log it and drop it while clonecast counted it delivered (Kamino 185).
// The Deliverer says so up front, which is what sends it by passthrough to a
// master fed by its agent.
func TestDeliverSkipsAKeyTheAgentCannotPost(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	reg, _ := startRegistry(t, wins, t.TempDir())
	c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: agentwire.Version, Title: "Game"})
	waitFor(t, "the agent", func() bool { return reg.Paired("w1") })

	d := New(reg)
	pause, _ := keys.Parse("pause")
	a, _ := keys.Parse("a")
	if d.Carries(pause) {
		t.Fatal("Carries(PAUSE) = true; it has no Win32 mapping")
	}
	if !d.Carries(a) {
		t.Fatal("Carries(A) = false")
	}
	for _, code := range []keys.Code{pause, a} {
		reached, err := d.Deliver(context.Background(), broadcast.Delivery{
			Event:   keys.Event{Code: code, State: keys.Down},
			Targets: []broadcast.WindowID{"w1"},
			Notify:  func(string, bool) {},
		})
		if err != nil {
			t.Fatal(err)
		}
		if code == pause && len(reached) != 0 {
			t.Errorf("PAUSE reached %v, want nothing", reached)
		}
		if code == a && len(reached) != 1 {
			t.Errorf("A reached %v, want [w1]", reached)
		}
	}
	// The first frame on the wire is A's: PAUSE never went out.
	if got := c.nextFrame(); got.Code != uint16(a) {
		t.Errorf("the agent's first frame is %+v, want A", got)
	}
}

// logLines collects a registry's notices for a test.
type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) notify(s string, _ bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, s)
}

func (l *logLines) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (l *logLines) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

func listen(t *testing.T, opts Options) *Registry {
	t.Helper()
	reg := NewRegistry(opts)
	if err := reg.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

// heldOpen reports whether the registry keeps c open (a refusal is held,
// Kamino 139) and sends nothing on it.
func heldOpen(c *testClient) bool {
	_ = c.conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	n, err := c.conn.Read(make([]byte, 1))
	return n == 0 && isTimeout(err)
}

// Kamino 275: in v3 the agent proves the token by challenge-response. One
// with the wrong token is refused through refuse (held, logged once), and
// the token reaches neither the wire nor the log.
func TestRegistryV3RefusesAnAgentThatCannotProveTheToken(t *testing.T) {
	var log logLines
	reg := listen(t, Options{Token: "s3cret", Notify: log.notify})

	var held []*testClient
	for i := 0; i < 2; i++ {
		c, _ := dialFakeV3(t, reg.Addr(), "b4d-t0ken", agentwire.Hello{Title: "Game"})
		held = append(held, c)
	}
	waitFor(t, "the refusal", func() bool { return log.count("does not prove") == 1 })
	for i, c := range held {
		if !heldOpen(c) {
			t.Fatalf("refusal %d was closed or written to; want it held open", i)
		}
	}
	if reg.Count() != 0 {
		t.Fatal("an agent with the wrong token registered")
	}
	time.Sleep(50 * time.Millisecond)
	if n := log.count("refused"); n != 1 {
		t.Errorf("%d refusals logged for one agent, want 1:\n%s", n, log.all())
	}
	if strings.Contains(log.all(), "s3cret") || strings.Contains(log.all(), "b4d-t0ken") {
		t.Errorf("a token reached the log:\n%s", log.all())
	}
}

// A recorded R is worth nothing on the next connection: the challenge is
// fresh each time, even for a replayed hello.
func TestRegistryV3RefusesAReplayedResponse(t *testing.T) {
	var log logLines
	reg := listen(t, Options{Token: "s3cret", Notify: log.notify})
	hello := agentwire.Hello{Version: 3, Title: "Game", Nonce: strings.Repeat("5a", 16)}

	// The genuine exchange, recorded.
	c1 := dialRaw(t, reg.Addr())
	_ = hello.Write(c1.conn)
	ch1 := c1.challenge()
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte("agent|" + ch1.Nonce + "|" + hello.Nonce))
	recorded := agentwire.Response{MAC: hex.EncodeToString(m.Sum(nil))}
	_ = recorded.Write(c1.conn)
	waitFor(t, "the genuine agent", func() bool { return reg.Count() == 1 })

	// The replay.
	c2 := dialRaw(t, reg.Addr())
	_ = hello.Write(c2.conn)
	if ch2 := c2.challenge(); ch2.Nonce == ch1.Nonce {
		t.Fatal("the registry reused its nonce")
	}
	_ = recorded.Write(c2.conn)
	waitFor(t, "the refusal", func() bool { return log.count("does not prove") == 1 })
	if reg.Count() != 1 {
		t.Fatalf("the replay registered (count %d)", reg.Count())
	}
}

// An agent that hangs up on the challenge (what a genuine agent with another
// token does) is logged once, saying what to do, however often it redials.
func TestRegistryV3AgentHangingUpOnTheChallenge(t *testing.T) {
	var log logLines
	reg := listen(t, Options{Token: "s3cret", Notify: log.notify})
	for i := 0; i < 3; i++ {
		c := dialRaw(t, reg.Addr())
		_ = (agentwire.Hello{Version: 3, Title: "Game", Nonce: strings.Repeat("5a", 16)}).Write(c.conn)
		c.challenge()
		_ = c.conn.Close()
	}
	waitFor(t, "the refusal", func() bool { return log.count("did not answer the challenge") >= 1 })
	time.Sleep(100 * time.Millisecond)
	if n := log.count("did not answer the challenge"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, log.all())
	}
}

// Each side accepts only the protocol it is set to, with no fallback, and
// the refusal says what to change: in clonecast's log, and to the agent as a
// V line its log can show (Kamino 275).
func TestRegistryRefusesTheOtherProtocol(t *testing.T) {
	for _, c := range []struct {
		setting int
		hello   agentwire.Hello
		want    string
	}{
		{3, agentwire.Hello{Version: 2, Title: "Game", Token: "s3cret"}, "this agent speaks protocol 2: reinstall it, or set the agent protocol to 2"},
		{2, agentwire.Hello{Version: 3, Title: "Game", Nonce: strings.Repeat("5a", 16)}, "this agent speaks protocol 3: set the agent protocol to 3, or reinstall it with --protocol 2"},
	} {
		var log logLines
		reg := listen(t, Options{Token: "s3cret", Protocol: c.setting, Notify: log.notify})
		cl := dialRaw(t, reg.Addr())
		_ = c.hello.Write(cl.conn)
		_ = cl.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		if !cl.sc.Scan() {
			t.Fatalf("set to %d: no version refusal: %v", c.setting, cl.sc.Err())
		}
		m, err := agentwire.Decode(cl.sc.Text())
		if err != nil || m.Kind != agentwire.KindVersion || m.Version != c.setting {
			t.Fatalf("set to %d: the agent was sent %q, want V %d", c.setting, cl.sc.Text(), c.setting)
		}
		waitFor(t, "the refusal", func() bool { return log.count(c.want) == 1 })
		if !heldOpen(cl) {
			t.Errorf("set to %d: the refusal was closed; want it held (Kamino 139)", c.setting)
		}
		if reg.Count() != 0 {
			t.Errorf("set to %d: the other protocol registered", c.setting)
		}
	}
}

// Both sides set to 2: v2 still pairs end to end, the token in the hello.
func TestRegistryV2StillPairs(t *testing.T) {
	wins := []broadcast.Window{{ID: "w1", Title: "Game"}}
	reg := listen(t, Options{
		Token:    "s3cret",
		Protocol: 2,
		Windows:  func(context.Context) ([]broadcast.Window, error) { return wins, nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reg.Run(ctx)
	c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: 2, Title: "Game", Token: "s3cret"})
	waitFor(t, "the v2 agent", func() bool { return reg.Paired("w1") })
	if !reg.agentFor("w1").enqueue(agentwire.Frame{Code: 30, Down: true}) {
		t.Fatal("enqueue refused")
	}
	if got := c.nextFrame(); got != (agentwire.Frame{Code: 30, Down: true}) {
		t.Fatalf("frame = %+v", got)
	}
}

// v3 proves the token, so a registry without one cannot run it: it refuses
// to be set to 3 and turns every v3 agent away, saying why.
func TestRegistryV3NeedsAToken(t *testing.T) {
	var log logLines
	reg := listen(t, Options{Notify: log.notify})
	if err := reg.SetProtocol(3); !errors.Is(err, agentwire.ErrNoToken) {
		t.Errorf("SetProtocol(3) with no token: err = %v", err)
	}
	c := dialRaw(t, reg.Addr())
	_ = (agentwire.Hello{Version: 3, Title: "Game", Nonce: strings.Repeat("5a", 16)}).Write(c.conn)
	waitFor(t, "the notice", func() bool { return log.count("token") == 1 })
	if !closedWithin(c, 2*time.Second) {
		t.Error("the connection stayed open")
	}
	if reg.Count() != 0 {
		t.Error("a v3 agent registered with a registry that has no token")
	}
}

// A v3 hello without a nonce cannot be challenged: refused, not registered.
func TestRegistryV3RefusesAHelloWithoutANonce(t *testing.T) {
	var log logLines
	reg := listen(t, Options{Token: "s3cret", Notify: log.notify})
	c := dialRaw(t, reg.Addr())
	_ = (agentwire.Hello{Version: 3, Title: "Game", Token: "s3cret"}).Write(c.conn)
	waitFor(t, "the refusal", func() bool { return log.count("no valid nonce") == 1 })
	if reg.Count() != 0 {
		t.Error("registered without the handshake")
	}
}

// The setting applies to new connections: an agent already registered keeps
// its session, and the next one must speak the new protocol.
func TestRegistrySetProtocolAppliesToNewConnections(t *testing.T) {
	reg, _ := startRegistry(t, nil, t.TempDir())
	dialAgent(t, reg.Addr(), agentwire.Hello{Version: 3, Title: "Game"})
	waitFor(t, "the v3 agent", func() bool { return reg.Count() == 1 })
	if err := reg.SetProtocol(2); err != nil {
		t.Fatal(err)
	}
	if reg.Protocol() != 2 {
		t.Fatalf("Protocol() = %d", reg.Protocol())
	}
	c := dialRaw(t, reg.Addr())
	_ = (agentwire.Hello{Version: 3, Title: "Game 2", Nonce: strings.Repeat("5a", 16)}).Write(c.conn)
	_ = c.conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if !c.sc.Scan() || c.sc.Text() != "V 2" {
		t.Fatalf("a v3 agent after the switch got %q, want V 2", c.sc.Text())
	}
	dialAgent(t, reg.Addr(), agentwire.Hello{Version: 2, Title: "Game 3", Token: testToken})
	waitFor(t, "the v2 agent", func() bool { return reg.Count() == 2 })
	for _, bad := range []int{0, 1, 4} {
		if err := reg.SetProtocol(bad); err == nil {
			t.Errorf("SetProtocol(%d) accepted", bad)
		}
	}
}

// In v3 a re-announce carries no token and no nonce, and is taken: the
// session was proven by the handshake.
func TestRegistryV3ReannounceWithoutAToken(t *testing.T) {
	reg, _ := startRegistry(t, nil, t.TempDir())
	c := dialAgent(t, reg.Addr(), agentwire.Hello{Version: 3, Title: "Launcher"})
	waitFor(t, "the agent", func() bool { return reg.Count() == 1 })
	_ = (agentwire.Hello{Version: 3, Title: "World of Warcraft", HWND: 0x42}).Write(c.conn)
	waitFor(t, "the re-announce", func() bool {
		a := reg.Agents()
		return len(a) == 1 && a[0].Title == "World of Warcraft" && a[0].HWND == 0x42
	})
}
