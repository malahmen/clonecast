//go:build linux

package x11

import (
	"fmt"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/res"
	"github.com/jezek/xgb/xproto"
)

// Focus answers which X client XWayland is delivering keyboard input to, as a
// host pid, for kwin.WM.Active (REFERENCE.md 4.22).
//
// KWin's activeWindow and the X input focus can disagree for seconds at a time
// when switching between Wine clients with the mouse: a Wine client sets the
// X input focus itself, and XWayland delivers keys to that window whatever
// KWin reports. Measured on the box (2026-10-18): KWin said Malahmen while the
// X focus was on Almerinda for 2.5 s, so the client really receiving the keys
// got them twice and the one KWin called the master got none.
//
// Pids come from the X-Resource extension, which the X server derives from
// each client socket's peer credentials: the real HOST pid even for a
// Flatpak-sandboxed Wine, and the same pid KWin reports for the window (4.17).
// It is safe for concurrent use.
type Focus struct {
	conn *xgb.Conn
}

// NewFocus connects to the X server named by $DISPLAY and checks it has
// X-Resource.
func NewFocus() (*Focus, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("x11 connect: %w", err)
	}
	if err := res.Init(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("x11 X-Resource extension: %w", err)
	}
	return &Focus{conn: conn}, nil
}

// Close drops the X connection.
func (f *Focus) Close() error {
	f.conn.Close()
	return nil
}

// Snapshot returns the pid owning the X input focus window (0 when the focus
// is None or PointerRoot, or its owner reports no pid) and the pids of every X
// client — which tells the caller whether a window KWin reports belongs to an
// XWayland client at all.
func (f *Focus) Snapshot() (focusPID int, xPIDs map[int]bool, err error) {
	clients, err := res.QueryClients(f.conn).Reply()
	if err != nil {
		return 0, nil, fmt.Errorf("x11 QueryClients: %w", err)
	}
	specs := make([]res.ClientIdSpec, len(clients.Clients))
	for i, c := range clients.Clients {
		specs[i] = res.ClientIdSpec{Client: c.ResourceBase, Mask: res.ClientIdMaskLocalClientPID}
	}
	xPIDs = make(map[int]bool, len(specs))
	if len(specs) > 0 {
		ids, err := res.QueryClientIds(f.conn, uint32(len(specs)), specs).Reply()
		if err != nil {
			return 0, nil, fmt.Errorf("x11 QueryClientIds: %w", err)
		}
		for _, id := range ids.Ids {
			if len(id.Value) > 0 {
				xPIDs[int(id.Value[0])] = true
			}
		}
	}

	focus, err := xproto.GetInputFocus(f.conn).Reply()
	if err != nil {
		return 0, nil, fmt.Errorf("x11 GetInputFocus: %w", err)
	}
	if focus.Focus == xproto.InputFocusNone || focus.Focus == xproto.InputFocusPointerRoot {
		return 0, xPIDs, nil
	}
	ids, err := res.QueryClientIds(f.conn, 1, []res.ClientIdSpec{{Client: uint32(focus.Focus), Mask: res.ClientIdMaskLocalClientPID}}).Reply()
	if err != nil {
		return 0, nil, fmt.Errorf("x11 QueryClientIds(focus): %w", err)
	}
	for _, id := range ids.Ids {
		if len(id.Value) > 0 {
			return int(id.Value[0]), xPIDs, nil
		}
	}
	return 0, xPIDs, nil
}
