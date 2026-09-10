//go:build linux

// Package x11 implements broadcast.Deliverer via X11 XSendEvent (Technique A;
// see REFERENCE.md 4.11/4.12/7.9). It sends synthetic KeyPress/KeyRelease
// events straight to a Wine game's X window: Wine's winex11.drv ignores the
// send_event flag, maps the X window to an HWND, and wineserver routes the key
// to that window's thread focus even when the window is not focused — so keys
// reach an unfocused client with no focus change and no cursor movement, and
// (unlike PostMessage) key state / Raw Input / low-level hooks are synthesized
// too, reaching DirectInput/Raw-Input games.
//
// EXPERIMENTAL. Two constraints from real testing (4.12): the target client
// must run in a Wine virtual desktop (plain windowed clients don't receive
// background XSendEvent here), and there is an unsolved stuck-key issue —
// a release delivered to a background window doesn't clear the synthesized
// hardware key state WoW's movement polls, so a key can stay engaged. Ship
// this behind an explicit opt-in, never as the default, until that is solved.
package x11

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
)

// evdevKeycodeOffset is the fixed offset between a Linux evdev key code and the
// X keycode Xwayland assigns (Xwayland uses evdev codes + 8). Because Wine's
// keyc2scan is built from the X server's keymap, sending X keycode = evdev+8 is
// layout-safe: no keysym lookup is needed. See research.md §5.2.
const evdevKeycodeOffset = 8

// Sender holds one X connection and turns key events into XSendEvent requests
// aimed at a specific X window. It is safe for concurrent use.
type Sender struct {
	mu   sync.Mutex
	conn *xgb.Conn
	root xproto.Window

	// stamp is a monotonically increasing X timestamp so each event's Time is
	// strictly greater than the last; Wine's server compares event times, and
	// CurrentTime(0) for both press and release was observed to misbehave.
	stamp uint32
}

// New connects to the X server named by $DISPLAY (the desktop Xwayland).
func New() (*Sender, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("x11 connect: %w", err)
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	return &Sender{conn: conn, root: root, stamp: uint32(time.Now().UnixMilli() & 0x7fffffff)}, nil
}

// Close releases the X connection.
func (s *Sender) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	return nil
}

func (s *Sender) nextStamp() xproto.Timestamp {
	s.stamp += 10
	return xproto.Timestamp(s.stamp)
}

// flush forces queued requests out to the server (a round-trip).
func (s *Sender) flush() { _, _ = xproto.GetInputFocus(s.conn).Reply() }

// SendKey delivers one key transition (down or up) to win. state carries the
// X modifier mask (Shift/Control/...) the caller is tracking. It does not move
// focus. Caller holds no lock; SendKey serialises internally.
func (s *Sender) SendKey(win xproto.Window, code keys.Code, down bool, state uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("x11 sender closed")
	}
	detail := xproto.Keycode(uint16(code) + evdevKeycodeOffset)
	if down {
		ev := xproto.KeyPressEvent{
			Detail: detail, Time: s.nextStamp(), Root: s.root, Event: win,
			Child: 0, State: state, SameScreen: true,
		}
		xproto.SendEvent(s.conn, false, win, uint32(xproto.EventMaskKeyPress), string(ev.Bytes()))
	} else {
		ev := xproto.KeyReleaseEvent{
			Detail: detail, Time: s.nextStamp(), Root: s.root, Event: win,
			Child: 0, State: state, SameScreen: true,
		}
		xproto.SendEvent(s.conn, false, win, uint32(xproto.EventMaskKeyRelease), string(ev.Bytes()))
	}
	s.flush()
	return nil
}

// FindByTitle returns the X window whose _NET_WM_NAME / WM_NAME exactly equals
// title. For a client in a Wine virtual desktop the game window ("World of
// Warcraft") is a child of the VD window, so the whole tree under root is
// searched, not just direct children. Returns ok=false if none matches.
func (s *Sender) FindByTitle(title string) (win xproto.Window, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return 0, false, fmt.Errorf("x11 sender closed")
	}
	want := title
	var walk func(w xproto.Window) (xproto.Window, bool)
	walk = func(w xproto.Window) (xproto.Window, bool) {
		if name, e := windowName(s.conn, w); e == nil && name == want {
			return w, true
		}
		tree, e := xproto.QueryTree(s.conn, w).Reply()
		if e != nil {
			return 0, false
		}
		for _, c := range tree.Children {
			if got, found := walk(c); found {
				return got, true
			}
		}
		return 0, false
	}
	if got, found := walk(s.root); found {
		return got, true, nil
	}
	return 0, false, nil
}

// windowName reads _NET_WM_NAME (UTF-8) then falls back to WM_NAME.
func windowName(conn *xgb.Conn, w xproto.Window) (string, error) {
	for _, atomName := range []string{"_NET_WM_NAME", "WM_NAME"} {
		atom, err := xproto.InternAtom(conn, true, uint16(len(atomName)), atomName).Reply()
		if err != nil || atom == nil || atom.Atom == 0 {
			continue
		}
		r, err := xproto.GetProperty(conn, false, w, atom.Atom, xproto.GetPropertyTypeAny, 0, 1024).Reply()
		if err != nil || r == nil || len(r.Value) == 0 {
			continue
		}
		return strings.TrimRight(string(r.Value), "\x00"), nil
	}
	return "", fmt.Errorf("no name")
}

// deliverer implements broadcast.Deliverer over a Sender. Each target's KWin
// caption is resolved to an X window by exact title match and cached; the
// event (down/up as carried in keys.Event) is XSendEvent'd there. It never
// moves focus, so the engine runs it without the focus lock.
type deliverer struct {
	s      *Sender
	titles func(broadcast.WindowID) string // maps a target id to its X window title

	mu    sync.Mutex
	cache map[broadcast.WindowID]xproto.Window
}

// NewDeliverer builds an xsend Deliverer. titles maps a target WindowID to the
// exact X window title to aim at (e.g. "World of Warcraft" for a VD client);
// resolution is cached per target.
func NewDeliverer(s *Sender, titles func(broadcast.WindowID) string) broadcast.Deliverer {
	return &deliverer{s: s, titles: titles, cache: map[broadcast.WindowID]xproto.Window{}}
}

func (d *deliverer) MovesFocus() bool { return false }
func (d *deliverer) Close() error     { return d.s.Close() }

func (d *deliverer) resolve(id broadcast.WindowID) (xproto.Window, bool) {
	d.mu.Lock()
	if w, ok := d.cache[id]; ok {
		d.mu.Unlock()
		return w, true
	}
	d.mu.Unlock()

	title := d.titles(id)
	if title == "" {
		return 0, false
	}
	w, ok, err := d.s.FindByTitle(title)
	if err != nil || !ok {
		return 0, false
	}
	d.mu.Lock()
	d.cache[id] = w
	d.mu.Unlock()
	return w, true
}

func (d *deliverer) Deliver(_ context.Context, ev keys.Event, targets []broadcast.WindowID, notify func(string, bool)) (int, error) {
	down := ev.State != keys.Up
	delivered := 0
	for _, t := range targets {
		w, ok := d.resolve(t)
		if !ok {
			notify("xsend: no X window for "+string(t), true)
			continue
		}
		if err := d.s.SendKey(w, ev.Code, down, 0); err != nil {
			notify("xsend "+ev.String()+" to "+string(t)+": "+err.Error(), true)
			// a stale cached window (game closed) — drop it so the next try re-resolves
			d.mu.Lock()
			delete(d.cache, t)
			d.mu.Unlock()
			continue
		}
		delivered++
	}
	return delivered, nil
}
