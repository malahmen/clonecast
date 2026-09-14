package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/broadcast"
)

// DefaultListen is the one loopback address clonecast listens on for agents.
// One port for every prefix, instead of a hand-picked port per instance.
const DefaultListen = "127.0.0.1:48800"

// repairInterval is how often the window<->agent pairing is recomputed even
// when nothing registered or disconnected. It exists because a window can
// appear long after its agent: the agent autostarts at prefix boot
// (REFERENCE.md 4.16) and the game window shows up minutes later.
const repairInterval = 2 * time.Second

// Agent is one connected in-bottle agent.
type Agent struct {
	ID     int64           // registry-local identity, stable for the connection
	Hello  agentwire.Hello // what it announced (updated if it re-announces)
	Remote string          // its address, for the log
	Since  time.Time

	conn net.Conn
	mu   sync.Mutex // serialises writes; the read loop runs concurrently
}

// Label is a short human identity for logs: the prefix's last path element if
// it announced one (bottle names are the useful part), else its window title,
// else its address.
func (a *Agent) Label() string {
	switch {
	case a.Hello.Prefix != "":
		return filepath.Base(a.Hello.Prefix)
	case a.Hello.Title != "":
		return a.Hello.Title
	}
	return a.Remote
}

// send writes one frame. The deadline is short and absolute: a wedged agent
// (a suspended prefix, a full socket buffer) must never stall the engine's
// broadcast worker, and dropping it is recoverable — it reconnects.
func (a *Agent) send(f agentwire.Frame) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return err
	}
	return f.Write(a.conn)
}

// Info is a snapshot of a connected agent for the UI and the log.
type Info struct {
	ID     int64
	Prefix string
	Title  string
	PID    int
	HWND   uint64
	Remote string
	Since  time.Time
	Window broadcast.WindowID // the window it is paired with, "" if none
}

// Options configures a Registry.
type Options struct {
	// Windows lists the compositor's windows. The registry needs it to pair
	// an agent's prefix with a window; it is called on a slow ticker and
	// whenever the agent set changes, never per keystroke.
	Windows func(context.Context) ([]broadcast.Window, error)
	// PrefixOf maps a host pid to its WINEPREFIX. nil means
	// prefix.PrefixOfPID, i.e. /proc on Linux and "no /proc" elsewhere.
	PrefixOf func(pid int) (string, error)
	// Notify reports registrations, disconnections and pairing decisions to
	// the UI's log. nil discards them.
	Notify func(text string, isErr bool)
}

// Registry accepts agent connections and keeps the live set (REFERENCE.md
// 4.17). It is the inversion of the old design: clonecast listens once and
// every agent dials in and stays connected, announcing the prefix and window
// it serves, so there is no per-instance port and no --agent map. Key frames
// travel back down the same connection.
type Registry struct {
	opts Options
	pair *pairing

	mu     sync.Mutex
	ln     net.Listener
	gen    int64 // listener generation, so a rebound listener's old accept loop exits
	addr   string
	agents map[int64]*Agent
	nextID int64
	closed bool

	changed chan struct{} // nudges the repair loop when the agent set changes
}

// NewRegistry builds a registry. It does not listen yet — call Listen.
func NewRegistry(opts Options) *Registry {
	if opts.Notify == nil {
		opts.Notify = func(string, bool) {}
	}
	if opts.Windows == nil {
		opts.Windows = func(context.Context) ([]broadcast.Window, error) {
			return nil, errors.New("no window manager")
		}
	}
	return &Registry{
		opts:    opts,
		pair:    newPairing(opts.Windows, opts.PrefixOf, opts.Notify),
		agents:  map[int64]*Agent{},
		changed: make(chan struct{}, 1),
	}
}

// Listen binds addr and starts accepting agents. Called again with a different
// address it rebinds: the new listener is opened first, so a failure leaves
// the old one serving, and connections already established are kept (they are
// live agents; nothing about them depends on which port they dialled). That is
// what makes the listen address editable from the TUI rather than a flag you
// must restart to change.
func (r *Registry) Listen(addr string) error {
	if addr == "" {
		addr = DefaultListen
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errors.New("registry is closed")
	}
	if r.ln != nil && r.addr == addr {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen for agents on %s: %w (another clonecast running, or the port is taken)", addr, err)
	}

	r.mu.Lock()
	old := r.ln
	r.ln, r.addr = ln, ln.Addr().String()
	r.gen++
	gen := r.gen
	r.mu.Unlock()

	if old != nil {
		_ = old.Close()
		r.opts.Notify("agents now listening on "+r.Addr()+" (was "+old.Addr().String()+"; connected agents kept)", false)
	}
	go r.accept(ln, gen)
	return nil
}

// Addr is the address agents are expected to dial.
func (r *Registry) Addr() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.addr == "" {
		return "(not listening)"
	}
	return r.addr
}

// Count is how many agents are connected right now. This is what makes the
// agent backend "available": no map to configure, just agents that showed up.
func (r *Registry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.agents)
}

// Agents is a snapshot for the UI, ordered by registration.
func (r *Registry) Agents() []Info {
	r.mu.Lock()
	list := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		list = append(list, a)
	}
	r.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	out := make([]Info, len(list))
	for i, a := range list {
		w, _ := r.pair.windowFor(a.ID)
		out[i] = Info{
			ID: a.ID, Prefix: a.Hello.Prefix, Title: a.Hello.Title,
			PID: a.Hello.PID, HWND: a.Hello.HWND, Remote: a.Remote,
			Since: a.Since, Window: w,
		}
	}
	return out
}

// Paired reports whether a live agent serves this window — what the TUI marks
// each Targets row with, so ticking a row is the only action needed.
func (r *Registry) Paired(id broadcast.WindowID) bool {
	return r.agentFor(id) != nil
}

// agentFor is Paired's internal form: the agent to send a window's keys to.
func (r *Registry) agentFor(id broadcast.WindowID) *Agent {
	aid, ok := r.pair.agentFor(id)
	if !ok {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.agents[aid]
}

// Run keeps the pairing fresh until ctx ends. It is cheap (one window list
// every few seconds, one /proc read per new window ever) and it is what lets a
// game launched after clonecast pick up its already-connected agent.
func (r *Registry) Run(ctx context.Context) {
	t := time.NewTicker(repairInterval)
	defer t.Stop()
	r.pair.recompute(ctx, r.snapshot())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.changed:
		}
		r.pair.recompute(ctx, r.snapshot())
	}
}

// Repair recomputes the pairing once, synchronously. Run does this on a timer;
// this is for callers that want it now (and for tests).
func (r *Registry) Repair(ctx context.Context) {
	r.pair.recompute(ctx, r.snapshot())
}

// Close stops listening and drops every agent.
func (r *Registry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	ln := r.ln
	r.ln = nil
	conns := make([]net.Conn, 0, len(r.agents))
	for _, a := range r.agents {
		conns = append(conns, a.conn)
	}
	r.agents = map[int64]*Agent{}
	r.mu.Unlock()

	if ln != nil {
		_ = ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	return nil
}

func (r *Registry) snapshot() []*Agent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Agent, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) accept(ln net.Listener, gen int64) {
	for {
		c, err := ln.Accept()
		if err != nil {
			r.mu.Lock()
			stale := r.closed || r.gen != gen
			r.mu.Unlock()
			if stale {
				return // this listener was replaced or the registry closed
			}
			r.opts.Notify("agent listener: "+err.Error(), true)
			time.Sleep(200 * time.Millisecond)
			continue
		}
		go r.serve(c)
	}
}

// serve reads one agent connection for its lifetime: a hello, then keepalives
// until it goes away. Frames travel the other way, written by the deliverer.
func (r *Registry) serve(c net.Conn) {
	defer c.Close()
	sc := bufio.NewScanner(c)

	_ = c.SetReadDeadline(time.Now().Add(agentwire.HelloTimeout))
	if !sc.Scan() {
		r.opts.Notify(fmt.Sprintf("agent %s: no hello within %s, closing", c.RemoteAddr(), agentwire.HelloTimeout), true)
		return
	}
	msg, err := agentwire.Decode(sc.Text())
	if err != nil {
		r.opts.Notify(fmt.Sprintf("agent %s: %v (rebuild the agent with `make agent` and reinstall it)", c.RemoteAddr(), err), true)
		return
	}
	if msg.Kind != agentwire.KindHello {
		r.opts.Notify(fmt.Sprintf("agent %s: first line was a %s, not a hello; closing", c.RemoteAddr(), msg.Kind), true)
		return
	}

	a := r.add(c, msg.Hello)
	defer r.remove(a)

	for {
		// Any line refreshes liveness; the agent sends a keepalive every
		// KeepaliveInterval, so silence for DeadAfter means it is gone even
		// if the socket has not noticed (a suspended or killed prefix).
		_ = c.SetReadDeadline(time.Now().Add(agentwire.DeadAfter))
		if !sc.Scan() {
			return
		}
		m, err := agentwire.Decode(sc.Text())
		if err != nil {
			continue // one bad line is not worth the connection
		}
		if m.Kind == agentwire.KindHello {
			// The agent re-resolved its window (WoW recreates its top-level
			// window across login/loading, REFERENCE.md 4.14).
			r.mu.Lock()
			a.Hello = m.Hello
			r.mu.Unlock()
			r.nudge()
		}
	}
}

func (r *Registry) add(c net.Conn, h agentwire.Hello) *Agent {
	r.mu.Lock()
	r.nextID++
	a := &Agent{ID: r.nextID, Hello: h, Remote: c.RemoteAddr().String(), Since: time.Now(), conn: c}
	r.agents[a.ID] = a
	n := len(r.agents)
	r.mu.Unlock()
	r.opts.Notify(fmt.Sprintf("agent registered: %s (%s, win pid %d, hwnd 0x%x) — %d connected", a.Label(), h.Describe(), h.PID, h.HWND, n), false)
	r.nudge()
	return a
}

func (r *Registry) remove(a *Agent) {
	r.mu.Lock()
	_, present := r.agents[a.ID]
	delete(r.agents, a.ID)
	n := len(r.agents)
	r.mu.Unlock()
	_ = a.conn.Close()
	if present {
		r.opts.Notify(fmt.Sprintf("agent gone: %s — %d connected", a.Label(), n), n == 0)
		r.nudge()
	}
}

// drop forces an agent out after a failed send; its own read loop then exits.
func (r *Registry) drop(a *Agent) { _ = a.conn.Close() }

func (r *Registry) nudge() {
	select {
	case r.changed <- struct{}{}:
	default:
	}
}
