package tui

import (
	"sync"

	"github.com/malahmen/clonecast/internal/broadcast"
)

// The fakes here carry no build tag: options_test.go, which runs on every OS,
// uses them too, and with fakeAgents in the darwin||linux pty file
// `GOOS=windows go vet` failed on it (Kamino 278).

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
