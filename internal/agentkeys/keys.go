// Package agentkeys is the agent's keyboard state: it turns the key frames
// clonecast sends into the WM_KEYDOWN/WM_KEYUP messages the agent posts, and
// remembers which keys it has posted down. It is plain Go with no build tag so
// it is tested on Linux; cmd/clonecast-agent (Windows only) just posts what it
// returns.
//
// Two things need that memory (Kamino 136):
//
//   - A repeat. clonecast sends a held key's auto-repeats as more D frames for
//     the same code (the wire has no repeat kind). Windows marks a repeat with
//     lParam bit 30, the previous key state; without it every repeat looked to
//     the game like a fresh press.
//   - The end of a session. When the connection to clonecast ends (clonecast
//     quit or crashed, or dropped this agent for a full queue or a write
//     deadline) no U frame is coming for the keys still down, and the game
//     would hold them until the player pressed them again. Release returns an
//     Up for each one.
package agentkeys

import (
	"sort"
	"sync"

	"github.com/malahmen/clonecast/internal/agentwire"
)

// The two window messages the agent posts.
const (
	WMKeyDown = 0x0100
	WMKeyUp   = 0x0101
)

// Msg is one message to post: PostMessage(hwnd, Msg, VK, LParam).
type Msg struct {
	Msg    uint32
	VK     uint16
	LParam uint32
	Code   uint16 // the evdev code it came from
}

// LParam builds a keystroke message's lParam: repeat count 1, the scancode,
// the extended bit, bit 30 (the key was already down) and bit 31 (the key is
// being released). Windows sets bit 30 on every key-up, since a released key
// was down.
func LParam(scan uint16, extended, wasDown, up bool) uint32 {
	lp := uint32(1) | uint32(scan)<<16
	if extended {
		lp |= 1 << 24
	}
	if wasDown || up {
		lp |= 1 << 30
	}
	if up {
		lp |= 1 << 31
	}
	return lp
}

// State is the set of keys one session has posted down. The zero value is
// ready to use; it is safe for concurrent use.
type State struct {
	mu   sync.Mutex
	down map[uint16]bool
}

// Frame returns the message for f and records the key's new state. ok is
// false for a code the agent cannot map (agentwire.Mappable), which changes
// nothing.
func (s *State) Frame(f agentwire.Frame) (m Msg, ok bool) {
	vk, scan, ext, ok := agentwire.WinKey(f.Code)
	if !ok {
		return Msg{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down == nil {
		s.down = map[uint16]bool{}
	}
	if !f.Down {
		delete(s.down, f.Code)
		return Msg{Msg: WMKeyUp, VK: vk, LParam: LParam(scan, ext, true, true), Code: f.Code}, true
	}
	was := s.down[f.Code]
	s.down[f.Code] = true
	return Msg{Msg: WMKeyDown, VK: vk, LParam: LParam(scan, ext, was, false), Code: f.Code}, true
}

// Release forgets every key still down and returns a key-up for each, in code
// order, for the agent to post when its session ends.
func (s *State) Release() []Msg {
	s.mu.Lock()
	codes := make([]int, 0, len(s.down))
	for c := range s.down {
		codes = append(codes, int(c))
	}
	s.down = nil
	s.mu.Unlock()
	sort.Ints(codes)
	out := make([]Msg, 0, len(codes))
	for _, c := range codes {
		vk, scan, ext, _ := agentwire.WinKey(uint16(c))
		out = append(out, Msg{Msg: WMKeyUp, VK: vk, LParam: LParam(scan, ext, true, true), Code: uint16(c)})
	}
	return out
}

// Held reports whether the session has code down.
func (s *State) Held(code uint16) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down[code]
}
