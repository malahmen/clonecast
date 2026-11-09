package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/platform/agent"
	"github.com/malahmen/clonecast/internal/platform/mock"
)

// testToken is the token registryWithWindows' registry holds.
const testToken = "t3st-token"

// registryWithWindows builds the registry the way run() does, over the mock
// desktop, listening on a free port: with a token, at protocol 3.
func registryWithWindows(t *testing.T) *agent.Registry {
	t.Helper()
	wm := mock.NewWM()
	reg := agent.NewRegistry(agent.Options{Windows: wm.List, Token: testToken})
	if err := reg.Listen("127.0.0.1:0"); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go reg.Run(ctx)
	t.Cleanup(func() { cancel(); _ = reg.Close() })
	return reg
}

func connectAgent(t *testing.T, reg *agent.Registry, title string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", reg.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	auth, err := agentwire.NewAgentAuth(testToken, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := (agentwire.Hello{Version: agentwire.Version, Title: title, Nonce: auth.Nonce()}).Write(c); err != nil {
		t.Fatalf("hello: %v", err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := auth.Run(bufio.NewScanner(c), c); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reg.Count() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the agent never registered")
}

func newTestEngine() *broadcast.Engine {
	wm := mock.NewWM()
	return broadcast.New(mock.NewSource(time.Hour), &mock.Injector{}, wm, broadcast.DefaultConfig())
}

// TestAgentBackendAvailabilityFollowsRegistrations is the rule that replaced
// "the agent backend is available only when --agent is set" (REFERENCE.md
// 4.17): there is nothing left to configure, so availability is simply
// whether an agent showed up.
func TestAgentBackendAvailabilityFollowsRegistrations(t *testing.T) {
	reg := registryWithWindows(t)
	bs := newBackends(newTestEngine(), mock.NewWM(), reg, "")
	t.Cleanup(bs.Close)

	for _, name := range bs.Names() {
		if name == deliverAgent {
			t.Fatalf("agent is offered with nothing registered: %v", bs.Names())
		}
	}

	connectAgent(t, reg, "Game — instance 1")

	var found bool
	for _, name := range bs.Names() {
		found = found || name == deliverAgent
	}
	if !found {
		t.Fatalf("agent is not offered although one registered: %v", bs.Names())
	}
	if err := bs.Use(deliverAgent); err != nil {
		t.Fatalf("Use(agent): %v", err)
	}
	if bs.Current() != deliverAgent {
		t.Fatalf("current backend = %q", bs.Current())
	}
}

// TestAgentBackendErrorIsActionable: selecting the agent backend with nothing
// registered must say what to do about it, not just fail.
func TestAgentBackendErrorIsActionable(t *testing.T) {
	reg := registryWithWindows(t)
	bs := newBackends(newTestEngine(), mock.NewWM(), reg, "")
	t.Cleanup(bs.Close)

	err := bs.Use(deliverAgent)
	if err == nil {
		t.Fatal("Use(agent) with no agents: want an error")
	}
	for _, want := range []string{reg.Addr(), "clonecast agent install", "skipped", "never falls back to moving focus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%s", want, err)
		}
	}
	// Kamino 187: it used to tell the user to run --deliver dance.
	if strings.Contains(err.Error(), "--deliver dance") {
		t.Errorf("the error still recommends the focus dance:\n%s", err)
	}
	if bs.Current() == deliverAgent {
		t.Error("the refused backend was selected anyway")
	}
}

// TestStartWarnsInsteadOfRefusing: at startup the same condition is a warning.
// An agent registers on its own schedule (its prefix may still be booting), so
// quitting over "no agents yet" would quit over something that fixes itself.
func TestStartWarnsInsteadOfRefusing(t *testing.T) {
	reg := registryWithWindows(t)
	bs := newBackends(newTestEngine(), mock.NewWM(), reg, "")
	t.Cleanup(bs.Close)

	warn, err := bs.Start(deliverAgent)
	if err != nil {
		t.Fatalf("Start(agent): %v", err)
	}
	if !strings.Contains(warn, "clonecast agent install") {
		t.Errorf("no actionable warning: %q", warn)
	}
	if bs.Current() != deliverAgent {
		t.Fatalf("current backend = %q, want agent", bs.Current())
	}

	// And with an agent connected there is nothing to warn about.
	connectAgent(t, reg, "Game — instance 1")
	bs2 := newBackends(newTestEngine(), mock.NewWM(), reg, "")
	t.Cleanup(bs2.Close)
	warn, err = bs2.Start(deliverAgent)
	if err != nil || warn != "" {
		t.Fatalf("Start with an agent connected: warn=%q err=%v", warn, err)
	}
}

// TestDefaultDeliver: the default is the agent backend on every platform,
// the mock one included (Kamino 187: it was the focus dance there).
func TestDefaultDeliver(t *testing.T) {
	for _, tc := range []struct{ deliver, want string }{
		{"", deliverAgent},
		{deliverXsend, deliverXsend},
		{deliverAgent, deliverAgent},
	} {
		if got := defaultDeliver(tc.deliver); got != tc.want {
			t.Errorf("defaultDeliver(%q) = %q, want %q", tc.deliver, got, tc.want)
		}
	}
}

// Kamino 187: the focus dance (activate each target, inject, restore focus)
// is not a delivery backend. The TUI's delivery row cycles through Names, so
// it must never be offered there: with no agent registered, Names on main was
// [dance xsend] and one space on the row selected it.
func TestNoBackendSwitchesFocus(t *testing.T) {
	reg := registryWithWindows(t)
	bs := newBackends(newTestEngine(), mock.NewWM(), reg, "")
	t.Cleanup(bs.Close)

	check := func(when string) {
		t.Helper()
		for _, name := range bs.Names() {
			if name != deliverAgent && name != deliverXsend {
				t.Errorf("%s: Names() offers %q, which is not a per-window backend: %v", when, name, bs.Names())
			}
		}
	}
	check("no agent registered")
	connectAgent(t, reg, "Game — instance 1")
	check("an agent registered")
}

// Kamino 187: --deliver dance (and the TUI, should anything still ask for it)
// is refused with the reason and the per-window replacements, not silently
// accepted and not a bare "unknown backend".
func TestDanceIsRefused(t *testing.T) {
	reg := registryWithWindows(t)
	bs := newBackends(newTestEngine(), mock.NewWM(), reg, "")
	t.Cleanup(bs.Close)

	if _, err := bs.Start(removedDance); err == nil {
		t.Fatal("Start(dance): want a refusal")
	} else {
		for _, want := range []string{"removed", "never moves focus", "--deliver agent", "--deliver xsend"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q:\n%s", want, err)
			}
		}
	}
	if err := bs.Use(removedDance); err == nil {
		t.Fatal("Use(dance): want a refusal")
	}
	if bs.Current() == removedDance {
		t.Fatal("the dance was selected anyway")
	}
}
