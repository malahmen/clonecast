// Package agent implements broadcast.Deliverer for the "agent" backend: it
// sends key transitions over loopback TCP to a clonecast-agent running inside
// each target's Wine prefix, which PostMessages them to the game window (see
// REFERENCE.md 4.12/7.9). It never moves focus, so the engine runs it without
// the focus lock. This is the reliable default backend for Wine/Proton
// targets.
package agent

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/malahmen/clonecast/internal/agentwire"
	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
)

// Deliverer fans key events out to per-target in-bottle agents over TCP.
type Deliverer struct {
	// endpoints maps a target to the "host:port" of its agent; "" means the
	// target has no agent and is skipped.
	endpoints func(broadcast.WindowID) string
	dialTO    time.Duration

	mu     sync.Mutex
	conns  map[string]net.Conn         // keyed by endpoint
	warned map[broadcast.WindowID]bool // targets already warned about (no endpoint)
}

// New builds an agent Deliverer. endpoints resolves a target WindowID to its
// agent's "host:port" (typically 127.0.0.1:<per-instance-port>).
func New(endpoints func(broadcast.WindowID) string) *Deliverer {
	return &Deliverer{
		endpoints: endpoints,
		dialTO:    2 * time.Second,
		conns:     map[string]net.Conn{},
		warned:    map[broadcast.WindowID]bool{},
	}
}

func (d *Deliverer) MovesFocus() bool { return false }

func (d *Deliverer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for ep, c := range d.conns {
		_ = c.Close()
		delete(d.conns, ep)
	}
	return nil
}

// conn returns a live connection to ep, dialing (and caching) if needed.
func (d *Deliverer) conn(ep string) (net.Conn, error) {
	d.mu.Lock()
	c := d.conns[ep]
	d.mu.Unlock()
	if c != nil {
		return c, nil
	}
	nc, err := net.DialTimeout("tcp", ep, d.dialTO)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conns[ep] = nc
	d.mu.Unlock()
	return nc, nil
}

// drop closes and forgets ep's connection so the next send re-dials.
func (d *Deliverer) drop(ep string) {
	d.mu.Lock()
	if c := d.conns[ep]; c != nil {
		_ = c.Close()
		delete(d.conns, ep)
	}
	d.mu.Unlock()
}

func (d *Deliverer) Deliver(_ context.Context, ev keys.Event, targets []broadcast.WindowID, notify func(string, bool)) (int, error) {
	frame := agentwire.Frame{Code: uint16(ev.Code), Down: ev.State != keys.Up}
	delivered := 0
	for _, t := range targets {
		ep := d.endpoints(t)
		if ep == "" {
			d.warnOnce(t, notify) // a ticked target with no --agent entry: warn once, not per key
			continue
		}
		if d.send(ep, frame, notify) {
			delivered++
		}
	}
	return delivered, nil
}

// send writes frame to ep, transparently re-dialing once if the cached
// connection is stale (a "connection reset"/broken pipe from an agent that
// restarted or an idle-closed socket). Returns whether the frame was delivered.
func (d *Deliverer) send(ep string, frame agentwire.Frame, notify func(string, bool)) bool {
	for attempt := 0; attempt < 2; attempt++ {
		c, err := d.conn(ep)
		if err != nil {
			notify("agent dial "+ep+": "+err.Error(), true)
			return false
		}
		if err := frame.Write(c); err != nil {
			d.drop(ep) // stale connection — force a fresh dial on the retry
			if attempt == 1 {
				notify("agent send to "+ep+": "+err.Error(), true)
				return false
			}
			continue
		}
		return true
	}
	return false
}

// warnOnce reports a missing endpoint for a ticked target a single time, so a
// deliberately-unmapped target (or a title typo) doesn't spam the log per key.
func (d *Deliverer) warnOnce(t broadcast.WindowID, notify func(string, bool)) {
	d.mu.Lock()
	first := !d.warned[t]
	d.warned[t] = true
	d.mu.Unlock()
	if first {
		notify("agent: no endpoint mapped for target "+string(t)+" (ticked but not in --agent); ignoring it", true)
	}
}
