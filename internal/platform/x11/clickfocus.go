//go:build linux

package x11

import (
	"fmt"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/res"
	"github.com/jezek/xgb/xproto"
)

// clickChecks are when, after KWin activates an X window, ClickFocus checks
// that the X input focus followed. Measured on the box (2026-10-19): a click
// left the focus on the old client until a second click, or a third client
// took it for 1.3-1.5 s, so the checks run past that.
var clickChecks = []time.Duration{
	150 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1500 * time.Millisecond,
}

// ClickFocus makes the X input focus follow the window KWin activates
// (REFERENCE.md 4.24). KWin switches at once on a click, but between Wine
// clients the X input focus, which is where XWayland sends the keys, does
// not always follow: it stays on the client clicked away from, or another
// client takes it. When that happens just after an activation, ClickFocus
// gives the focus to the window KWin activated.
//
// It only acts in the moments after KWin activates an X window, and only on
// the window KWin activated: it never picks a window itself. KWin publishes
// the active X window as _NET_ACTIVE_WINDOW on the root window. It does NOT
// clear it when a native Wayland window (the terminal clonecast runs in)
// becomes active: the property keeps naming the last X window. A check that
// fires after such a switch can therefore still move the X focus, but only
// within XWayland, which then does not have the Wayland keyboard focus, so
// the keys keep going to the terminal. The flip side: returning from the
// terminal to the same X window may not change the property, and then there
// is nothing to follow.
type ClickFocus struct {
	conn   *xgb.Conn
	root   xproto.Window
	active xproto.Atom
	name   xproto.Atom
	utf8   xproto.Atom
	mu     sync.Mutex
	gen    int // bumped on every activation; stale checks see it changed and stop
	notify func(string)

	done chan struct{}
}

// NewClickFocus connects to $DISPLAY and starts following activations.
func NewClickFocus() (*ClickFocus, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("x11 connect: %w", err)
	}
	if err := res.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("x11 X-Resource extension: %w", err)
	}
	c := &ClickFocus{conn: conn, root: xproto.Setup(conn).DefaultScreen(conn).Root, done: make(chan struct{})}
	for name, dst := range map[string]*xproto.Atom{"_NET_ACTIVE_WINDOW": &c.active, "_NET_WM_NAME": &c.name, "UTF8_STRING": &c.utf8} {
		a, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("x11 atom %s: %w", name, err)
		}
		*dst = a.Atom
	}
	if err := xproto.ChangeWindowAttributesChecked(conn, c.root, xproto.CwEventMask,
		[]uint32{xproto.EventMaskPropertyChange}).Check(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("x11 watch root window: %w", err)
	}
	go c.run()
	return c, nil
}

// SetNotify sets where a line goes each time the focus is corrected.
func (c *ClickFocus) SetNotify(f func(string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.notify = f
}

func (c *ClickFocus) say(line string) {
	c.mu.Lock()
	f := c.notify
	c.mu.Unlock()
	if f != nil {
		f(line)
	}
}

// Close stops following activations.
func (c *ClickFocus) Close() error {
	c.conn.Close()
	<-c.done
	return nil
}

func (c *ClickFocus) run() {
	defer close(c.done)
	for {
		ev, err := c.conn.WaitForEvent()
		if ev == nil && err == nil {
			return // connection closed
		}
		p, ok := ev.(xproto.PropertyNotifyEvent)
		if !ok || p.Atom != c.active {
			continue
		}
		w, aerr := c.activeWindow()
		// Every change cancels the checks still running for the previous
		// activation, including a change to None (which KWin may never
		// send; see the type doc): only the latest click is followed.
		c.mu.Lock()
		c.gen++
		gen := c.gen
		c.mu.Unlock()
		if aerr != nil || w == 0 {
			continue
		}
		go c.follow(w, gen)
	}
}

// follow runs the checks for one activation of w.
func (c *ClickFocus) follow(w xproto.Window, gen int) {
	start := time.Now()
	for _, at := range clickChecks {
		time.Sleep(time.Until(start.Add(at)))
		c.mu.Lock()
		stale := c.gen != gen
		c.mu.Unlock()
		if stale {
			return // a newer activation owns the focus now
		}
		now, err := c.activeWindow()
		if err != nil {
			return
		}
		focus, err := xproto.GetInputFocus(c.conn).Reply()
		if err != nil {
			return
		}
		wantPID, havePID := c.ownerPID(uint32(w)), 0
		if focus.Focus != xproto.InputFocusNone && focus.Focus != xproto.InputFocusPointerRoot {
			havePID = c.ownerPID(uint32(focus.Focus))
		}
		if !shouldCorrect(w, now, wantPID, havePID) {
			continue
		}
		if err := xproto.SetInputFocusChecked(c.conn, xproto.InputFocusParent, w, xproto.TimeCurrentTime).Check(); err != nil {
			c.say(fmt.Sprintf("click focus: could not give the keyboard to %s: %v", c.title(w), err))
			return
		}
		held := "nobody"
		if havePID != 0 {
			held = c.title(xproto.Window(focus.Focus))
		}
		c.say(fmt.Sprintf("click focus: %s was clicked but %s had the keyboard after %v; gave it to %s",
			c.title(w), held, at, c.title(w)))
	}
}

// shouldCorrect is follow's decision, free of X so it can be tested. clicked
// is the window KWin activated, activeNow what KWin says is active at the
// check. The focus is corrected only while KWin still names the clicked
// window, only when that window's owner is known, and only when the focus is
// somewhere else: on another process, or nowhere.
func shouldCorrect(clicked, activeNow xproto.Window, clickedPID, focusPID int) bool {
	if activeNow != clicked || clickedPID == 0 {
		return false
	}
	return focusPID != clickedPID
}

func (c *ClickFocus) activeWindow() (xproto.Window, error) {
	r, err := xproto.GetProperty(c.conn, false, c.root, c.active, xproto.AtomWindow, 0, 1).Reply()
	if err != nil {
		return 0, err
	}
	if r.Format != 32 || len(r.Value) < 4 {
		return 0, nil
	}
	return xproto.Window(xgb.Get32(r.Value)), nil
}

// ownerPID is the host pid of the X client owning a resource, 0 if unknown.
func (c *ClickFocus) ownerPID(id uint32) int {
	ids, err := res.QueryClientIds(c.conn, 1, []res.ClientIdSpec{{Client: id, Mask: res.ClientIdMaskLocalClientPID}}).Reply()
	if err != nil {
		return 0
	}
	for _, v := range ids.Ids {
		if len(v.Value) > 0 {
			return int(v.Value[0])
		}
	}
	return 0
}

// title names a window for the log: its _NET_WM_NAME, else its id.
func (c *ClickFocus) title(w xproto.Window) string {
	r, err := xproto.GetProperty(c.conn, false, w, c.name, c.utf8, 0, 256).Reply()
	if err == nil && len(r.Value) > 0 {
		return string(r.Value)
	}
	return fmt.Sprintf("window 0x%x", uint32(w))
}
