//go:build linux

package x11

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/res"
	"github.com/jezek/xgb/xproto"
)

// guardEvery is how often FocusGuard checks the X input focus. A steal lasts
// until the next check, so this bounds how many keys can go to the wrong game.
const guardEvery = 100 * time.Millisecond

// guardQuiet is how long an identical correction line is kept out of the log,
// so a client that keeps stealing does not flood it.
const guardQuiet = 5 * time.Second

// clickGrace is how long the focus may sit on the game under the mouse before
// KWin names it active. A click moves the X focus first and KWin's
// _NET_ACTIVE_WINDOW follows within a second on the box (2026-10-20);
// correcting in between gave the keyboard back to the client clicked away
// from. Longer would let a game that steals while under the mouse keep the
// keys for longer.
const clickGrace = time.Second

// clickSettle is how long after a mouse click the guard looks at what it hit:
// long enough for the click to move the X focus.
const clickSettle = 200 * time.Millisecond

// askEvery is how often the guard asks KWin again to activate the game clicked,
// while KWin names another.
const askEvery = 500 * time.Millisecond

// FocusGuard keeps the X input focus, which is where XWayland sends the keys,
// on the window KWin says is active (REFERENCE.md 4.24). Between Wine clients
// the two drift apart: after a click the focus can stay on the old client, and
// a client not clicked can take it, a second or many seconds later, while KWin
// still shows the clicked one. FocusGuard gives the focus back to KWin's window.
//
// A click moves the X focus before KWin publishes the new active window. So a
// focus on the game under the mouse is given clickGrace to be confirmed by
// KWin; a focus anywhere else is a steal and is corrected at once.
//
// With the mouse watched (Clicked), the game you click wins over KWin: KWin
// does not always activate the game clicked (2026-10-20), and then the guard
// handed the keyboard back to the game clicked away from. The guard asks KWin
// to activate the clicked game and keeps the X focus on it, until you click
// another game, or something that is not a game.
//
// Without a click it never picks a window itself: it enforces KWin's choice, read from
// _NET_ACTIVE_WINDOW on the X root. KWin does not clear that property when a
// native Wayland window (the terminal) becomes active; the X focus then sits
// on a window of KWin's own, so FocusGuard leaves the focus alone whenever KWin
// owns it, or nobody does.
type FocusGuard struct {
	conn   *xgb.Conn
	root   xproto.Window
	active xproto.Atom
	check  xproto.Atom
	name   xproto.Atom
	utf8   xproto.Atom
	state  xproto.Atom

	mu      sync.Mutex
	notify  func(string)
	clickAt time.Time // the last mouse click

	clickSeen time.Time     // the last click looked at
	chosen    xproto.Window // the game clicked, 0 for KWin's choice
	chosenPID int
	askedAt   time.Time // when KWin was last asked to activate chosen

	lastLine string
	lastAt   time.Time
	held     int // repeats of lastLine not said

	// The focus owner seen diverging from KWin under the mouse, and since when.
	pendingPID   int
	pendingSince time.Time

	stop chan struct{}
	done chan struct{}
}

// NewFocusGuard connects to $DISPLAY and starts guarding.
func NewFocusGuard() (*FocusGuard, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("x11 connect: %w", err)
	}
	if err := res.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("x11 X-Resource extension: %w", err)
	}
	g := &FocusGuard{conn: conn, root: xproto.Setup(conn).DefaultScreen(conn).Root,
		stop: make(chan struct{}), done: make(chan struct{})}
	for name, dst := range map[string]*xproto.Atom{"_NET_ACTIVE_WINDOW": &g.active,
		"_NET_SUPPORTING_WM_CHECK": &g.check, "_NET_WM_NAME": &g.name, "UTF8_STRING": &g.utf8, "WM_STATE": &g.state} {
		a, err := xproto.InternAtom(conn, false, uint16(len(name)), name).Reply()
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("x11 atom %s: %w", name, err)
		}
		*dst = a.Atom
	}
	go g.run()
	return g, nil
}

// SetNotify sets where a line goes each time the focus is corrected.
func (g *FocusGuard) SetNotify(f func(string)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.notify = f
}

func (g *FocusGuard) say(line string) {
	g.mu.Lock()
	f := g.notify
	g.mu.Unlock()
	if f != nil {
		f(line)
	}
}

// Clicked tells the guard a mouse button was pressed.
func (g *FocusGuard) Clicked() {
	g.mu.Lock()
	g.clickAt = time.Now()
	g.mu.Unlock()
}

// Close stops guarding.
func (g *FocusGuard) Close() error {
	close(g.stop)
	<-g.done
	g.conn.Close()
	return nil
}

func (g *FocusGuard) run() {
	defer close(g.done)
	t := time.NewTicker(guardEvery)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
			g.tick()
		}
	}
}

// tick runs one check, and corrects the focus if it strayed.
func (g *FocusGuard) tick() {
	if g.followClick() {
		return
	}
	w := g.windowProp(g.root, g.active)
	if w == 0 {
		g.pendingPID = 0
		return
	}
	focus, err := xproto.GetInputFocus(g.conn).Reply()
	if err != nil || focus.Focus == xproto.Window(w) {
		g.pendingPID = 0
		return
	}
	havePID := 0
	if focus.Focus != xproto.InputFocusNone && focus.Focus != xproto.InputFocusPointerRoot {
		havePID = g.ownerPID(uint32(focus.Focus))
	}
	wantPID := g.ownerPID(uint32(w))
	wm := g.wmPID()
	if !shouldCorrect(wantPID, havePID, wm) {
		g.pendingPID = 0
		return
	}
	now := time.Now()
	if havePID != g.pendingPID {
		g.pendingPID, g.pendingSince = havePID, now
	}
	underMouse := isUnderMouse(havePID, g.pointerPID(), wm)
	if !correctNow(underMouse, now.Sub(g.pendingSince)) {
		return
	}
	g.pendingPID = 0
	if err := xproto.SetInputFocusChecked(g.conn, xproto.InputFocusParent, w, xproto.TimeCurrentTime).Check(); err != nil {
		g.log(fmt.Sprintf("focus guard: could not give the keyboard back to %s: %s", g.title(w, wantPID), errText(err)))
		return
	}
	how := "took the keyboard"
	if underMouse {
		how = fmt.Sprintf("had the keyboard under the mouse for %v without KWin activating it", clickGrace)
	}
	g.log(fmt.Sprintf("focus guard: %s %s while %s was active; gave it back",
		g.title(focus.Focus, havePID), how, g.title(w, wantPID)))
}

// followClick keeps the keyboard on the game clicked last, and reports whether
// it did; false leaves the focus to KWin's choice.
func (g *FocusGuard) followClick() bool {
	focus, err := xproto.GetInputFocus(g.conn).Reply()
	if err != nil {
		return false
	}
	havePID := 0
	if focus.Focus != xproto.InputFocusNone && focus.Focus != xproto.InputFocusPointerRoot {
		havePID = g.ownerPID(uint32(focus.Focus))
	}
	wm := g.wmPID()

	g.mu.Lock()
	at := g.clickAt
	g.mu.Unlock()
	if at.After(g.clickSeen) && time.Since(at) >= clickSettle {
		g.clickSeen = at
		win, pid := g.pointerClient()
		if !clickedGame(havePID, pid, wm) {
			g.chosen = 0
		} else if win != g.chosen {
			g.chosen, g.chosenPID, g.askedAt = win, pid, time.Time{}
		}
	}
	if g.chosen == 0 {
		return false
	}
	if havePID == wm && wm != 0 {
		g.chosen = 0 // a Wayland window took over, the terminal say
		return false
	}

	if active := g.windowProp(g.root, g.active); g.ownerPID(uint32(active)) != g.chosenPID && time.Since(g.askedAt) >= askEvery {
		g.askedAt = time.Now()
		if err := g.activate(g.chosen); err != nil {
			g.log(fmt.Sprintf("focus guard: could not ask KWin to activate %s: %s", g.title(g.chosen, g.chosenPID), errText(err)))
		} else {
			g.log(fmt.Sprintf("focus guard: asked KWin to activate %s, which you clicked", g.title(g.chosen, g.chosenPID)))
		}
	}
	if havePID == 0 || havePID == g.chosenPID {
		return true
	}
	if err := xproto.SetInputFocusChecked(g.conn, xproto.InputFocusParent, g.chosen, xproto.TimeCurrentTime).Check(); err != nil {
		g.log(fmt.Sprintf("focus guard: could not give the keyboard back to %s: %s", g.title(g.chosen, g.chosenPID), errText(err)))
		g.chosen = 0 // gone, or no longer viewable
		return false
	}
	g.log(fmt.Sprintf("focus guard: %s took the keyboard from %s, which you clicked; gave it back",
		g.title(focus.Focus, havePID), g.title(g.chosen, g.chosenPID)))
	return true
}

// clickedGame says whether a click hit a game: the pointer is on another
// client's window, not KWin's (a title bar, the desktop), and the X focus is
// not KWin's, which it is when a Wayland window such as the terminal is
// active. XWayland does not track the pointer over Wayland windows, so the
// pointer alone cannot tell.
func clickedGame(havePID, pointerPID, wmPID int) bool {
	return pointerPID != 0 && havePID != 0 && pointerPID != wmPID && havePID != wmPID
}

// activate asks the window manager to activate w, as a pager would (source 2):
// KWin's focus stealing prevention does not hold that back.
func (g *FocusGuard) activate(w xproto.Window) error {
	ev := xproto.ClientMessageEvent{Format: 32, Window: w, Type: g.active,
		Data: xproto.ClientMessageDataUnionData32New([]uint32{2, uint32(xproto.TimeCurrentTime), 0, 0, 0})}
	return xproto.SendEventChecked(g.conn, false, g.root,
		xproto.EventMaskSubstructureRedirect|xproto.EventMaskSubstructureNotify, string(ev.Bytes())).Check()
}

// pointerClient is the client window under the mouse, the one carrying
// WM_STATE (else the top-level one), and its owner's pid; 0, 0 if none.
func (g *FocusGuard) pointerClient() (xproto.Window, int) {
	w, client := g.root, xproto.Window(0)
	for {
		r, err := xproto.QueryPointer(g.conn, w).Reply()
		if err != nil || !r.SameScreen || r.Child == 0 {
			break
		}
		w = r.Child
		if client == 0 {
			client = w // top-level, unless a child carries WM_STATE
		}
		if p, err := xproto.GetProperty(g.conn, false, w, g.state, xproto.GetPropertyTypeAny, 0, 0).Reply(); err == nil && p.Format != 0 {
			client = w
			break
		}
	}
	if client == 0 {
		return 0, 0
	}
	return client, g.ownerPID(uint32(client))
}

// errText is an X error without its sequence number, so a repeated failure
// is one line in the log rather than one per check.
func errText(err error) string {
	s, _, _ := strings.Cut(err.Error(), " {")
	return s
}

// log says line, unless it said the same line less than guardQuiet ago; the
// next line it says counts the repeats it held back, so a fight shows.
func (g *FocusGuard) log(line string) {
	now := time.Now()
	if line == g.lastLine && now.Sub(g.lastAt) < guardQuiet {
		g.held++
		return
	}
	out := line
	if g.held > 0 {
		out += fmt.Sprintf(" (and %d more since the last line)", g.held)
	}
	g.lastLine, g.lastAt, g.held = line, now, 0
	g.say(out)
}

// shouldCorrect is tick's decision, free of X so it can be tested. wantPID owns
// the window KWin says is active, havePID the X focus, wmPID is KWin. The focus
// moves only from one known client to another: not when KWin holds it (a
// Wayland window is active), nor when nobody does, nor when either owner is
// unknown.
func shouldCorrect(wantPID, havePID, wmPID int) bool {
	if wantPID == 0 || havePID == 0 || wantPID == wmPID || havePID == wmPID {
		return false
	}
	return havePID != wantPID
}

// correctNow decides when a focus that shouldCorrect objects to is moved: a
// focus on the game under the mouse is most likely a click KWin has not
// published yet, and waits clickGrace; any other is a steal, moved at once.
func correctNow(underMouse bool, diverged time.Duration) bool {
	return !underMouse || diverged >= clickGrace
}

// isUnderMouse says whether the focus owner may have just been clicked: the
// pointer is on one of its windows, or on KWin's, which is where a title bar
// is (the decoration is KWin's, the game sits below it).
func isUnderMouse(havePID, pointerPID, wmPID int) bool {
	return pointerPID != 0 && (pointerPID == havePID || pointerPID == wmPID)
}

// pointerPID is the owner of the deepest window under the mouse, 0 if none.
func (g *FocusGuard) pointerPID() int {
	w := g.root
	for {
		r, err := xproto.QueryPointer(g.conn, w).Reply()
		if err != nil || !r.SameScreen {
			return 0
		}
		if r.Child == 0 {
			break
		}
		w = r.Child
	}
	if w == g.root {
		return 0
	}
	return g.ownerPID(uint32(w))
}

// wmPID is the pid of the window manager, from the window it names on the
// root as _NET_SUPPORTING_WM_CHECK; 0 if unknown.
func (g *FocusGuard) wmPID() int {
	w := g.windowProp(g.root, g.check)
	if w == 0 {
		return 0
	}
	return g.ownerPID(uint32(w))
}

// windowProp reads a WINDOW property of w, 0 if unset.
func (g *FocusGuard) windowProp(w xproto.Window, prop xproto.Atom) xproto.Window {
	r, err := xproto.GetProperty(g.conn, false, w, prop, xproto.AtomWindow, 0, 1).Reply()
	if err != nil || r.Format != 32 || len(r.Value) < 4 {
		return 0
	}
	return xproto.Window(xgb.Get32(r.Value))
}

// ownerPID is the host pid of the X client owning a resource, 0 if unknown.
func (g *FocusGuard) ownerPID(id uint32) int {
	ids, err := res.QueryClientIds(g.conn, 1, []res.ClientIdSpec{{Client: id, Mask: res.ClientIdMaskLocalClientPID}}).Reply()
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

// title names a window for the log: its _NET_WM_NAME, else its owner's pid.
func (g *FocusGuard) title(w xproto.Window, pid int) string {
	r, err := xproto.GetProperty(g.conn, false, w, g.name, g.utf8, 0, 256).Reply()
	if err == nil && len(r.Value) > 0 {
		return string(r.Value)
	}
	return fmt.Sprintf("pid %d", pid)
}
