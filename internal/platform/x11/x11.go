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
	"github.com/jezek/xgb/res"
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

// New connects to the X server named by $DISPLAY (the desktop Xwayland). It
// needs the X-Resource extension, which is how Find tells whose window is
// whose (Focus needs it for the same reason).
func New() (*Sender, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("x11 connect: %w", err)
	}
	if err := res.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("x11 X-Resource extension: %w", err)
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

// SendKey delivers one key transition (down or up) to win. state is the X
// modifier mask. The deliverer passes 0: the engine tracks no modifier state
// (REFERENCE.md 7.1, 7.9), and a modifier in the key set is broadcast as its
// own press and release. It does not move focus. Caller holds no lock;
// SendKey serialises internally.
//
// The request is checked, which is the round trip the old GetInputFocus flush
// made anyway: a window that has gone (the game closed or recreated it) comes
// back as BadWindow, so the deliverer drops it from its cache and resolves
// again (Kamino 202). Unchecked, the error was lost and the dead window kept.
func (s *Sender) SendKey(win xproto.Window, code keys.Code, down bool, state uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("x11 sender closed")
	}
	detail := xproto.Keycode(uint16(code) + evdevKeycodeOffset)
	var ck xproto.SendEventCookie
	if down {
		ev := xproto.KeyPressEvent{
			Detail: detail, Time: s.nextStamp(), Root: s.root, Event: win,
			Child: 0, State: state, SameScreen: true,
		}
		ck = xproto.SendEventChecked(s.conn, false, win, uint32(xproto.EventMaskKeyPress), string(ev.Bytes()))
	} else {
		ev := xproto.KeyReleaseEvent{
			Detail: detail, Time: s.nextStamp(), Root: s.root, Event: win,
			Child: 0, State: state, SameScreen: true,
		}
		ck = xproto.SendEventChecked(s.conn, false, win, uint32(xproto.EventMaskKeyRelease), string(ev.Bytes()))
	}
	return ck.Check()
}

// Find returns the X window to aim at for a KWin window owned by host pid,
// looking for title, and how many windows matched.
//
// The search is scoped to that one client (Kamino 202). It used to walk the
// whole X tree for the first window with the title, so every target aimed at
// "World of Warcraft" got the same window - the first client's - and the
// others got nothing. Now only windows owned by pid, and the windows below
// them, are candidates: a plain Wine client's own window or, in a Wine
// virtual desktop, the game window inside that client's desktop. The desktop
// belongs to the prefix's explorer.exe, which is the pid KWin reports; the
// game window inside it belongs to the game process, so ownership is checked
// on the way down, not on the match itself.
//
// Pids come from X-Resource, the source KWin uses for a window's pid (see
// Focus), so the two agree even for a Flatpak-sandboxed Wine. matches > 1
// means one desktop holds several clients, and the caller must not pick one.
func (s *Sender) Find(pid int, title string) (win xproto.Window, matches int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return 0, 0, fmt.Errorf("x11 sender closed")
	}
	clients, err := res.QueryClients(s.conn).Reply()
	if err != nil {
		return 0, 0, fmt.Errorf("x11 QueryClients: %w", err)
	}
	t := &liveTree{conn: s.conn}
	if len(clients.Clients) > 0 {
		specs := make([]res.ClientIdSpec, len(clients.Clients))
		for i, c := range clients.Clients {
			specs[i] = res.ClientIdSpec{Client: c.ResourceBase, Mask: res.ClientIdMaskLocalClientPID}
		}
		ids, err := res.QueryClientIds(s.conn, uint32(len(specs)), specs).Reply()
		if err != nil {
			return 0, 0, fmt.Errorf("x11 QueryClientIds: %w", err)
		}
		pids := make(map[uint32]int, len(ids.Ids))
		for _, id := range ids.Ids {
			if len(id.Value) > 0 {
				pids[id.Spec.Client] = int(id.Value[0])
			}
		}
		for _, c := range clients.Clients {
			if p, ok := pids[c.ResourceBase]; ok {
				t.clients = append(t.clients, clientPID{base: c.ResourceBase, mask: c.ResourceMask, pid: p})
			}
		}
	}
	win, matches = findUnder(t, s.root, pid, title)
	return win, matches, nil
}

// xtree is the part of the X server findUnder reads, so the search is
// testable against a fake tree.
type xtree interface {
	children(w xproto.Window) []xproto.Window
	name(w xproto.Window) string
	owner(w xproto.Window) int // host pid of the client that created w, 0 if unknown
}

// findUnder walks the tree under root for windows titled title that are owned
// by pid or lie below a window owned by pid. It returns the first match and
// the number of matches. A match's own subtree is not searched: a window's
// children are part of it, not further clients.
func findUnder(t xtree, root xproto.Window, pid int, title string) (xproto.Window, int) {
	var first xproto.Window
	n := 0
	var walk func(w xproto.Window, under bool)
	walk = func(w xproto.Window, under bool) {
		if !under && t.owner(w) == pid {
			under = true
		}
		if under && t.name(w) == title {
			if n == 0 {
				first = w
			}
			n++
			return
		}
		for _, c := range t.children(w) {
			walk(c, under)
		}
	}
	if pid > 0 {
		walk(root, false)
	}
	return first, n
}

// clientPID is one X client's resource id range and its host pid.
type clientPID struct {
	base, mask uint32
	pid        int
}

// liveTree reads the tree from the server. Used with Sender.mu held.
type liveTree struct {
	conn    *xgb.Conn
	clients []clientPID
}

func (t *liveTree) children(w xproto.Window) []xproto.Window {
	tree, err := xproto.QueryTree(t.conn, w).Reply()
	if err != nil {
		return nil
	}
	return tree.Children
}

func (t *liveTree) name(w xproto.Window) string {
	name, _ := windowName(t.conn, w)
	return name
}

// owner maps a window to its client by resource id: every id a client
// allocates is its resource base with bits under its mask.
func (t *liveTree) owner(w xproto.Window) int {
	for _, c := range t.clients {
		if uint32(w)&^c.mask == c.base {
			return c.pid
		}
	}
	return 0
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

// Target is what the deliverer needs to know about a KWin window to find its
// X window: the X window title to look for (its KWin caption, or the --xsend
// override for it) and the host pid KWin reports. Title "" means the window
// is gone.
type Target struct {
	Title string
	PID   int
}

// xserver is the part of Sender the deliverer uses; tests fake it.
type xserver interface {
	Find(pid int, title string) (xproto.Window, int, error)
	SendKey(win xproto.Window, code keys.Code, down bool, state uint16) error
	Close() error
}

// deliverer implements broadcast.Deliverer over a Sender. Each target is
// resolved to an X window under its own KWin window (Sender.Find) and cached;
// the event (down/up as carried in keys.Event) is XSendEvent'd there. It never
// moves focus, so the engine runs it without the focus lock.
type deliverer struct {
	x      xserver
	lookup func(broadcast.WindowID) Target

	mu    sync.Mutex
	cache map[broadcast.WindowID]resolved
	// list is the target list the cache was built for. A different list
	// empties the cache (Kamino 202): before, only a failed send did, and a
	// failed send never came (SendKey was unchecked), so a window resolved
	// once was kept for the whole run.
	list string
}

// resolved is one cached resolution, with the Target it was made from: a
// changed title or pid (the game restarted) re-resolves.
type resolved struct {
	from Target
	win  xproto.Window
}

// NewDeliverer builds an xsend Deliverer. lookup maps a target WindowID to
// its X window title and host pid (see Target).
func NewDeliverer(s *Sender, lookup func(broadcast.WindowID) Target) broadcast.Deliverer {
	return newDeliverer(s, lookup)
}

func newDeliverer(x xserver, lookup func(broadcast.WindowID) Target) *deliverer {
	return &deliverer{x: x, lookup: lookup, cache: map[broadcast.WindowID]resolved{}}
}

func (d *deliverer) MovesFocus() bool { return false }
func (d *deliverer) Close() error     { return d.x.Close() }

// resolveLocked returns id's X window, or why there is none. d.mu held.
// Failures are not cached: a game still loading has no named window yet.
func (d *deliverer) resolveLocked(id broadcast.WindowID) (xproto.Window, string) {
	from := d.lookup(id)
	if c, ok := d.cache[id]; ok && c.from == from {
		return c.win, ""
	}
	delete(d.cache, id)
	switch {
	case from.Title == "":
		return 0, "the window is gone"
	case from.PID <= 0:
		return 0, "KWin reports no pid for it"
	}
	w, n, err := d.x.Find(from.PID, from.Title)
	switch {
	case err != nil:
		return 0, err.Error()
	case n == 0:
		return 0, fmt.Sprintf("no X window titled %q in pid %d", from.Title, from.PID)
	case n > 1:
		// Several games in one Wine virtual desktop: any pick would be a
		// guess, and the keys of one would go to another.
		return 0, fmt.Sprintf("%d X windows titled %q in pid %d (one Wine virtual desktop holding several clients?)", n, from.Title, from.PID)
	}
	d.cache[id] = resolved{from: from, win: w}
	return w, ""
}

// send is one XSendEvent the deliverer will make.
type send struct {
	id  broadcast.WindowID
	win xproto.Window
}

// plan resolves the targets of req to the X windows to send to, and reports
// what it refuses through req.Notify.
//
// The origin is skipped by X window, not only by KWin id (Kamino 202): a
// target that resolves to the master's own X window is the master. And two
// targets that resolve to one X window are refused, both of them: that is one
// Wine virtual desktop seen as two KWin windows, and sending there twice
// would double every key for one client and give the other none.
func (d *deliverer) plan(req broadcast.Delivery) []send {
	var notes []string
	d.mu.Lock()
	ids := make([]string, len(req.Targets))
	for i, t := range req.Targets {
		ids[i] = string(t)
	}
	if list := strings.Join(ids, "\x00"); list != d.list {
		d.cache = map[broadcast.WindowID]resolved{}
		d.list = list
	}
	var origin xproto.Window
	haveOrigin := false
	if req.Origin != "" {
		// Not reported when it fails: the master need not be an xsend
		// target at all (a terminal, a native game).
		w, why := d.resolveLocked(req.Origin)
		origin, haveOrigin = w, why == ""
	}
	var cand []send
	byWin := map[xproto.Window][]broadcast.WindowID{}
	for _, t := range req.Targets {
		if t == req.Origin {
			continue // the master: passthrough already delivered the key there
		}
		w, why := d.resolveLocked(t)
		if why != "" {
			notes = append(notes, "xsend: no X window for "+string(t)+": "+why)
			continue
		}
		if haveOrigin && w == origin {
			continue // the master's own X window under another KWin id
		}
		if len(byWin[w]) == 0 {
			cand = append(cand, send{id: t, win: w})
		}
		byWin[w] = append(byWin[w], t)
	}
	d.mu.Unlock()

	out := cand[:0]
	for _, c := range cand {
		if shared := byWin[c.win]; len(shared) > 1 {
			names := make([]string, len(shared))
			for i, t := range shared {
				names[i] = string(t)
			}
			notes = append(notes, "xsend: "+strings.Join(names, ", ")+" are one X window (a Wine virtual desktop?): not sent")
			continue
		}
		out = append(out, c)
	}
	for _, n := range notes {
		req.Notify(n, true)
	}
	return out
}

// Deliver XSendEvents ev to every ticked target except origin — the focused
// window already received it through passthrough (REFERENCE.md 7.10). An
// empty origin matches no target, so every target is served.
func (d *deliverer) Deliver(_ context.Context, req broadcast.Delivery) ([]broadcast.WindowID, error) {
	down := req.Event.State != keys.Up
	var delivered []broadcast.WindowID
	for _, s := range d.plan(req) {
		t := s.id
		if err := d.x.SendKey(s.win, req.Event.Code, down, 0); err != nil {
			// "a key", never which: notices reach the log file, and REFERENCE.md
			// 8 promises no record of what was typed (Kamino 202).
			req.Notify("xsend a key to "+string(t)+": "+err.Error(), true)
			// a stale cached window (game closed) — drop it so the next try re-resolves
			d.mu.Lock()
			delete(d.cache, t)
			d.mu.Unlock()
			continue
		}
		delivered = append(delivered, t)
	}
	return delivered, nil
}
