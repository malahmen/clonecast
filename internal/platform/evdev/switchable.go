package evdev

import (
	"errors"
	"fmt"
	"sync"

	ev "github.com/holoplot/go-evdev"

	"github.com/malahmen/clonecast/internal/keys"
)

// Switchable is a Source whose captured devices can be changed while the engine
// is running.
//
// The engine selects on the channel Events() returns, so that channel has to
// outlive any one set of grabbed devices: this owns the real Source and pumps
// its events into a channel of its own. Swapping the devices underneath is then
// invisible upstream, and neither the engine nor the TUI has to know.
//
// Why it exists: which keyboard to capture cannot be decided reliably before
// the first keypress. Discovery matches any device reporting KEY_A and
// KEY_ENTER, which on a real desk includes both event nodes of one keyboard and
// a gaming mouse's macro interfaces, and only typing says which one you use.
// Choosing at startup and living with it meant restarting to correct a guess.
type Switchable struct {
	mu     sync.Mutex
	cur    *Source
	out    chan keys.Event
	closed bool
}

// OpenSwitchable grabs an initial set of devices, with Open's semantics: a
// device that cannot be grabbed is skipped, and it fails only if none could be.
func OpenSwitchable(paths ...string) (*Switchable, error) {
	s := &Switchable{out: make(chan keys.Event, 64)}
	if err := s.Switch(paths...); err != nil {
		return nil, err
	}
	return s, nil
}

// Events is the stable channel the engine reads.
//
// It is never closed. A reader goroutine blocked in a blocking ReadOne is not
// reliably woken by closing its fd, so a pump can outlive the Source it serves;
// closing this channel would then risk a send on a closed channel. The engine
// shuts down on its context rather than on this channel ending, so nothing
// needs the close.
func (s *Switchable) Events() <-chan keys.Event { return s.out }

// Switch re-grabs, replacing the captured devices.
//
// The old devices are released FIRST, because the sets usually overlap: going
// from one node of a keyboard to both of them, or re-selecting the same device,
// would otherwise fail against this process's own existing grab. The cost is a
// few milliseconds with nothing captured, and that on an action a human just
// took. If the new set cannot be grabbed, the previous one is restored and the
// error says whether that worked.
func (s *Switchable) Switch(paths ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("source is closed")
	}
	if s.cur != nil && samePaths(s.cur.Paths(), paths) {
		return nil // already exactly this: re-grabbing would fight itself
	}

	var previous []string
	if s.cur != nil {
		previous = s.cur.Paths()
		_ = s.cur.Close() // ungrabs synchronously; the reader may linger
		s.cur = nil
	}

	next, err := Open(paths...)
	if err != nil {
		if len(previous) == 0 {
			return err
		}
		back, rerr := Open(previous...)
		if rerr != nil {
			return fmt.Errorf("%w (and the previous keyboard could not be re-grabbed: %v)", err, rerr)
		}
		s.cur = back
		go s.pump(back)
		return fmt.Errorf("%w (previous keyboard restored)", err)
	}

	s.cur = next
	go s.pump(next)
	return nil
}

// pump forwards one Source's events to the stable channel.
//
// It takes no lock, deliberately. An earlier version checked "am I still the
// current source?" under s.mu per event and deadlocked: Switch held s.mu while
// waiting for the old pump to drain, and the pump could not drain because it
// was blocked on that same mutex. Nor does Switch wait for it any more, for the
// reason given on Events. A few events from a device just released are real
// keypresses, better forwarded than dropped.
func (s *Switchable) pump(src *Source) {
	for ev := range src.Events() {
		s.out <- ev
	}
}

// Close releases the captured devices. The event channel is left open (see
// Events) and Switch is refused afterwards.
func (s *Switchable) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cur == nil {
		return nil
	}
	err := s.cur.Close()
	s.cur = nil
	return err
}

// Paths is what is currently captured.
func (s *Switchable) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	return s.cur.Paths()
}

// Skipped is why anything asked for is not being captured.
func (s *Switchable) Skipped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	return s.cur.Skipped()
}

// Devices exposes the grabbed devices so an Injector can clone one.
func (s *Switchable) Devices() []*ev.InputDevice {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	return s.cur.Devices()
}

func samePaths(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, p := range a {
		seen[p]++
	}
	for _, p := range b {
		seen[p]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
