package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
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

// queueLen is how many frames may wait for one agent. Each agent has its own
// queue and writer, so one that stops reading delays only itself (Kamino item
// 44); a full queue means it has not read for a long while, and it is dropped.
const queueLen = 256

// Agent is one connected in-bottle agent.
type Agent struct {
	ID     int64  // registry-local identity, stable for the connection
	Remote string // its address, for the log
	Since  time.Time

	// What it announced, through an atomic pointer rather than a plain field.
	// An agent RE-announces whenever WoW recreates its top-level window
	// (REFERENCE.md 4.14), i.e. at every login and loading screen, and that
	// write was taken under r.mu while Agents(), Label() and the pairing
	// passes all read it without r.mu — a real data race, reported by -race.
	// An atomic pointer is used instead of widening r.mu because the readers
	// are on the delivery path, where taking the registry lock to read one
	// struct would put the pairing pass in front of every key.
	hello atomic.Pointer[agentwire.Hello]

	conn net.Conn
	mu   sync.Mutex // serialises writes; the read loop runs concurrently

	out      chan agentwire.Frame // frames waiting for this agent's writer
	done     chan struct{}        // closed when the agent is removed
	doneOnce sync.Once
	// unsent counts frames queued and not yet written (or failed), so Close
	// can let the writers finish: the Ups the engine sends at quit are the
	// last frames queued, and closing the socket under them lost them.
	unsent atomic.Int32
}

func newAgent(id int64, c net.Conn) *Agent {
	return &Agent{ID: id, Remote: c.RemoteAddr().String(), Since: time.Now(), conn: c,
		out: make(chan agentwire.Frame, queueLen), done: make(chan struct{})}
}

// enqueue hands a frame to the agent's writer without waiting for the socket,
// and reports false if its queue is full or it is gone.
func (a *Agent) enqueue(f agentwire.Frame) bool {
	select {
	case <-a.done:
		return false
	default:
	}
	a.unsent.Add(1)
	select {
	case a.out <- f:
		return true
	default:
		a.unsent.Add(-1)
		return false
	}
}

// run writes queued frames in order until the agent is removed, or a write
// fails, which it reports to fail and stops.
func (a *Agent) run(fail func(error)) {
	for {
		select {
		case <-a.done:
			return
		case f := <-a.out:
			err := a.send(f)
			a.unsent.Add(-1)
			if err != nil {
				fail(err)
				return
			}
		}
	}
}

func (a *Agent) stop() { a.doneOnce.Do(func() { close(a.done) }) }

// Hello is what this agent announced. Always read it once and work from the
// copy: a re-announce can land between two reads, and a comparison made from
// two different Hellos is a pairing decision about an agent that never
// existed.
func (a *Agent) Hello() agentwire.Hello {
	if h := a.hello.Load(); h != nil {
		return *h
	}
	return agentwire.Hello{}
}

// setHello replaces it. The value is copied, so the caller may reuse its own.
func (a *Agent) setHello(h agentwire.Hello) { a.hello.Store(&h) }

// Label is a short human identity for logs: the prefix's last path element if
// it announced one (bottle names are the useful part), else its window title,
// else its address.
func (a *Agent) Label() string {
	h := a.Hello()
	switch {
	case h.Prefix != "":
		return filepath.Base(h.Prefix)
	case h.Title != "":
		return h.Title
	}
	return a.Remote
}

// send writes one frame, from the agent's own writer (run). The deadline is
// short and absolute: a wedged agent (a suspended prefix, a full socket
// buffer) is dropped rather than waited on, which is recoverable — it
// reconnects.
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

// maxHeldRefused and holdRefusedFor are refuse's bounds. A handful of
// prefixes is the real case; the cap is for whatever else dials 127.0.0.1.
// An old agent closed after holdRefusedFor redials once and is held again,
// one reconnect per hold rather than 500 a second.
const (
	maxHeldRefused = 16
	holdRefusedFor = 10 * time.Minute
)

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
	// Token is the secret every agent must prove, the one `clonecast agent
	// install` leaves beside it (Kamino item 2). Without it any local process
	// could register and be sent the keystrokes. Protocol 3 proves it by
	// challenge-response and cannot run without it: every v3 agent is refused
	// when it is "". In protocol 2 the agent sends it in the hello, and ""
	// checks nothing, for tests.
	Token string
	// Protocol is the agent protocol accepted, 3 or 2; 0 means
	// agentwire.DefaultProtocol. Only that one is accepted, with no fallback
	// (Kamino 275); SetProtocol changes it for new connections.
	Protocol int
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
	// held are refused connections kept open, and warned the refusals
	// already logged; see refuse.
	held   map[net.Conn]struct{}
	warned map[string]bool
	// maxHeld and holdFor bound the refused connections held open: past
	// maxHeld a refusal closes at once, and each one held is closed after
	// holdFor (Kamino 139). Fields so tests can shrink them.
	maxHeld int
	holdFor time.Duration

	protocol atomic.Int32 // the agent protocol new connections must speak

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
	r := &Registry{
		opts:    opts,
		pair:    newPairing(opts.Windows, opts.PrefixOf, opts.Notify),
		agents:  map[int64]*Agent{},
		held:    map[net.Conn]struct{}{},
		warned:  map[string]bool{},
		maxHeld: maxHeldRefused,
		holdFor: holdRefusedFor,
		changed: make(chan struct{}, 1),
	}
	p := opts.Protocol
	if p == 0 {
		p = agentwire.DefaultProtocol
	}
	r.protocol.Store(int32(p))
	return r
}

// Protocol is the agent protocol new connections must speak.
func (r *Registry) Protocol() int { return int(r.protocol.Load()) }

// SetProtocol changes the agent protocol for new connections; agents already
// registered keep their session. It is the TUI's "agent proto" row and
// --agent-protocol (Kamino 275).
func (r *Registry) SetProtocol(p int) error {
	if !agentwire.ValidProtocol(p) {
		return fmt.Errorf("agent protocol %d: want %d or %d", p, agentwire.Version, agentwire.MinVersion)
	}
	if p >= 3 && r.opts.Token == "" {
		return fmt.Errorf("agent protocol %d: %w", p, agentwire.ErrNoToken)
	}
	r.protocol.Store(int32(p))
	return nil
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
	if err := loopbackOnly(addr); err != nil {
		return err
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

// loopbackOnly refuses an address other machines could reach. Agents run in
// Wine prefixes on this machine, and the keys they are sent are everything
// broadcast (Kamino item 2).
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing to listen for agents on %s: agents run on this machine, so the address must be loopback (for example %s)", addr, DefaultListen)
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
		h := a.Hello()
		out[i] = Info{
			ID: a.ID, Prefix: h.Prefix, Title: h.Title,
			PID: h.PID, HWND: h.HWND, Remote: a.Remote,
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

// flushWait bounds how long Close waits for the agents' writers to send what
// is queued. A writer's own deadline is 2 s; an agent that slow is dropped
// anyway.
const flushWait = time.Second

// Close stops listening and drops every agent, once their writers have sent
// what is queued, or flushWait has passed: the engine releases held keys at
// quit just before this (Kamino 202).
func (r *Registry) Close() error {
	deadline := time.Now().Add(flushWait)
	for _, a := range r.snapshot() {
	wait:
		for a.unsent.Load() > 0 && time.Now().Before(deadline) {
			select {
			case <-a.done:
				break wait // its writer is gone; nothing more will be sent
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
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
	for c := range r.held {
		conns = append(conns, c)
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
	hello, ok := r.admit(c, sc, msg.Hello)
	if !ok {
		return
	}
	proto := hello.Version

	a := r.add(c, hello)
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
		if m.Kind == agentwire.KindHello && m.Hello.Version == proto {
			// The agent re-resolved its window (WoW recreates its top-level
			// window across login/loading, REFERENCE.md 4.14). In v3 it
			// carries no secret: the session was proven by the handshake.
			h := m.Hello
			h.Token, h.Nonce = "", ""
			a.setHello(h)
			r.nudge()
		}
	}
}

// admit decides on a connection's hello under the protocol set now, and runs
// the v3 handshake. It reports the hello to register, without its secrets,
// and false when the connection was refused or failed; refusals go through
// refuse. The handshake reads under the hello's deadline, HelloTimeout.
func (r *Registry) admit(c net.Conn, sc *bufio.Scanner, h agentwire.Hello) (agentwire.Hello, bool) {
	proto := r.Protocol()
	if err := agentwire.CheckVersion(proto, h); err != nil {
		// Said to the agent too: one at the other protocol logs it. An
		// agent built before protocol 3 ignores the line.
		_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = agentwire.WriteVersionRefusal(c, proto)
		r.refuse(c, h, err.Error())
		return h, false
	}
	if proto == 2 {
		if r.opts.Token != "" && subtle.ConstantTimeCompare([]byte(h.Token), []byte(r.opts.Token)) != 1 {
			why := "the wrong token"
			if h.Token == "" {
				why = "no token"
			}
			r.refuse(c, h, "it sent "+why+"; "+reinstall)
			return h, false
		}
		h.Token = ""
		return h, true
	}
	auth, err := agentwire.NewClonecastAuth(r.opts.Token, h, rand.Reader)
	switch {
	case errors.Is(err, agentwire.ErrBadNonce):
		r.refuse(c, h, "its protocol 3 hello carries no valid nonce; "+reinstall)
		return h, false
	case err != nil:
		r.opts.Notify(fmt.Sprintf("agent %s (%s): cannot authenticate it: %v", c.RemoteAddr(), h.Describe(), err), true)
		return h, false
	}
	_ = c.SetWriteDeadline(time.Now().Add(agentwire.HelloTimeout))
	err = auth.Run(sc, c)
	_ = c.SetWriteDeadline(time.Time{})
	switch {
	case errors.Is(err, agentwire.ErrAgentUnproven):
		r.refuse(c, h, "it does not prove the install's token; "+reinstall)
		return h, false
	case err != nil:
		// A real agent hangs up on a challenge that does not prove its
		// token, so this is most often an agent installed with another
		// token. Through refuse, so it is logged once, not per redial.
		// The reason leaves out the error itself, which names the ports and
		// would make every redial a new refusal to log.
		r.refuse(c, h, "it did not answer the challenge: it holds another token, or is not a clonecast agent; "+reinstall)
		return h, false
	}
	h.Token, h.Nonce = "", ""
	return h, true
}

// reinstall is the advice a refusal for the token ends with.
const reinstall = "install it again (`clonecast agent install`, or i in the Prefixes pane) and restart the game"

// refuse turns away an agent without the token, or at the other protocol, and keeps its connection open
// rather than closing it. Agents built before 2026-10-09 redial with no sleep
// when a connection ends at once (redial.Next), so closing it spun them about
// 500 times a second: 50,000 refusals in 90 seconds, in both logs and on the
// wineserver the game shares. Held open, they sit idle until the game exits.
// The refusal is logged once per prefix.
//
// Holding is bounded (Kamino 139): at most maxHeld at once, a refusal past
// that is closed at once, and each held one is closed after holdFor however
// much it sends.
//
// why is the whole reason, with what to do about it.
func (r *Registry) refuse(c net.Conn, h agentwire.Hello, why string) {
	key := h.Describe() + "\x00" + why
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	first := !r.warned[key]
	r.warned[key] = true
	full := len(r.held) >= r.maxHeld
	if !full {
		r.held[c] = struct{}{}
	}
	r.mu.Unlock()
	if first {
		r.opts.Notify(fmt.Sprintf("agent %s (%s): refused, %s", c.RemoteAddr(), h.Describe(), why), true)
	}
	if full {
		return // serve's deferred Close
	}
	_ = c.SetReadDeadline(time.Now().Add(r.holdFor))
	_, _ = io.Copy(io.Discard, c)
	r.mu.Lock()
	delete(r.held, c)
	r.mu.Unlock()
}

func (r *Registry) add(c net.Conn, h agentwire.Hello) *Agent {
	r.mu.Lock()
	r.nextID++
	a := newAgent(r.nextID, c)
	a.setHello(h)
	r.agents[a.ID] = a
	n := len(r.agents)
	r.mu.Unlock()
	r.opts.Notify(fmt.Sprintf("agent registered: %s (%s, win pid %d, hwnd 0x%x) — %d connected", a.Label(), h.Describe(), h.PID, h.HWND, n), false)
	go a.run(func(err error) {
		r.opts.Notify(fmt.Sprintf("agent %s: %v (dropped; it will re-register)", a.Label(), err), true)
		r.drop(a)
	})
	r.nudge()
	return a
}

func (r *Registry) remove(a *Agent) {
	r.mu.Lock()
	_, present := r.agents[a.ID]
	delete(r.agents, a.ID)
	n := len(r.agents)
	r.mu.Unlock()
	a.stop()
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
