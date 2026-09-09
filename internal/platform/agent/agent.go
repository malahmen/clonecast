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

	mu    sync.Mutex
	conns map[string]net.Conn // keyed by endpoint
}

// New builds an agent Deliverer. endpoints resolves a target WindowID to its
// agent's "host:port" (typically 127.0.0.1:<per-instance-port>).
func New(endpoints func(broadcast.WindowID) string) *Deliverer {
	return &Deliverer{
		endpoints: endpoints,
		dialTO:    2 * time.Second,
		conns:     map[string]net.Conn{},
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
			notify("agent: no endpoint for "+string(t), true)
			continue
		}
		c, err := d.conn(ep)
		if err != nil {
			notify("agent dial "+ep+": "+err.Error(), true)
			continue
		}
		if err := frame.Write(c); err != nil {
			// stale/closed connection — drop it and report; next key re-dials.
			d.drop(ep)
			notify("agent send to "+ep+": "+err.Error(), true)
			continue
		}
		delivered++
	}
	return delivered, nil
}
