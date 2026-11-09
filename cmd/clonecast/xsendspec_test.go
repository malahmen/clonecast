package main

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/malahmen/clonecast/internal/broadcast"
)

// namedDeliverer is a Deliverer the engine is handed but never asked to
// deliver with: nothing is held, so SetDeliverer queues no releases.
type namedDeliverer struct {
	broadcast.Deliverer
	name string
}

// xsendBuilds stands in for setupXsend: each build is a namedDeliverer named
// after its spec, and every build and close lands in log.
type xsendBuilds struct {
	b    *backends
	mu   sync.Mutex
	log  []string
	fail bool
	// liveAtClose is what b.made held for xsend when a deliverer was closed.
	liveAtClose []string
}

func newXsendBuilds(t *testing.T, spec string) *xsendBuilds {
	t.Helper()
	x := &xsendBuilds{}
	x.b = newBackends(newTestEngine(), nil, nil, spec)
	x.b.setupXsend = x.setup
	if err := x.b.Use(deliverXsend); err != nil {
		t.Fatalf("Use(xsend): %v", err)
	}
	return x
}

func (x *xsendBuilds) setup(_ broadcast.WindowManager, spec string) (broadcast.Deliverer, func(), error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.fail {
		x.log = append(x.log, "fail "+spec)
		return nil, nil, errors.New("no X display")
	}
	x.log = append(x.log, "build "+spec)
	return &namedDeliverer{name: spec}, func() {
		x.mu.Lock()
		x.log = append(x.log, "close "+spec)
		x.mu.Unlock()
		// TryLock: a close made under b.mu cannot look, and says so.
		live := "(closed under b.mu)"
		if x.b.mu.TryLock() {
			if d, ok := x.b.made[deliverXsend].(*namedDeliverer); ok {
				live = d.name
			}
			x.b.mu.Unlock()
		}
		x.mu.Lock()
		x.liveAtClose = append(x.liveAtClose, live)
		x.mu.Unlock()
	}, nil
}

func (x *xsendBuilds) events() string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return strings.Join(x.log, "; ")
}

// Kamino 139: SetXsendSpec closed the xsend deliverer in use before building
// its replacement. The new one is built and swapped in first, and only then
// is the old one closed.
func TestSetXsendSpecBuildsBeforeItCloses(t *testing.T) {
	x := newXsendBuilds(t, "A=1")
	if err := x.b.SetXsendSpec("B=2"); err != nil {
		t.Fatalf("SetXsendSpec: %v", err)
	}
	if got, want := x.events(), "build A=1; build B=2; close A=1"; got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
	if len(x.liveAtClose) != 1 || x.liveAtClose[0] != "B=2" {
		t.Fatalf("when the old deliverer closed, the backend in place was %q, want the new one (B=2)", x.liveAtClose)
	}
	if x.b.Current() != deliverXsend || x.b.XsendSpec() != "B=2" {
		t.Fatalf("current %q spec %q, want xsend with B=2", x.b.Current(), x.b.XsendSpec())
	}
}

// Kamino 139: a rebuild that fails keeps the working deliverer, and the map
// it works with, instead of leaving the engine on a closed one.
func TestSetXsendSpecKeepsTheOldOneWhenTheRebuildFails(t *testing.T) {
	x := newXsendBuilds(t, "A=1")
	x.fail = true
	if err := x.b.SetXsendSpec("B=2"); err == nil {
		t.Fatal("SetXsendSpec succeeded with a failing build")
	}
	if got, want := x.events(), "build A=1; fail B=2"; got != want {
		t.Fatalf("events %q, want %q: the working deliverer was closed", got, want)
	}
	x.b.mu.Lock()
	d, _ := x.b.made[deliverXsend].(*namedDeliverer)
	x.b.mu.Unlock()
	if d == nil || d.name != "A=1" {
		t.Fatalf("xsend deliverer after the failure is %v, want the old one (A=1)", d)
	}
	if x.b.Current() != deliverXsend || x.b.XsendSpec() != "A=1" {
		t.Fatalf("current %q spec %q, want xsend still on A=1", x.b.Current(), x.b.XsendSpec())
	}
}

// Not in use, the built one is simply dropped; the next Use builds afresh.
func TestSetXsendSpecDropsAnIdleOne(t *testing.T) {
	x := newXsendBuilds(t, "A=1")
	x.b.mu.Lock()
	x.b.cur = deliverAgent // as if switched away
	x.b.mu.Unlock()
	if err := x.b.SetXsendSpec("B=2"); err != nil {
		t.Fatalf("SetXsendSpec: %v", err)
	}
	if got, want := x.events(), "build A=1; close A=1"; got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
	if err := x.b.Use(deliverXsend); err != nil {
		t.Fatalf("Use: %v", err)
	}
	if got, want := x.events(), "build A=1; close A=1; build B=2"; got != want {
		t.Fatalf("events %q, want %q", got, want)
	}
}
