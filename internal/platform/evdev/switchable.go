//go:build linux

package evdev

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/charmbracelet/log"

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
	cur    *feed
	out    chan keys.Event
	closed bool

	// open grabs a set of devices; a seam, so tests need no input devices.
	open func(paths []string) (capture, error)
	// keysUp is how long a release waits for held keys (keysUpTimeout).
	keysUp time.Duration

	notifyMu sync.Mutex
	notify   func(text string, isErr bool)
}

// capture is what Switchable needs of a Source.
type capture interface {
	Events() <-chan keys.Event
	Paths() []string
	Skipped() []string
	KeysUp(timeout time.Duration) error
	Lost() int
	Close() error
}

// feed is one capture being pumped into the stable channel, with the keys it
// has pressed there and not yet released.
type feed struct {
	src     capture
	stop    chan struct{}
	mu      sync.Mutex
	retired bool
	down    map[ev.EvCode]bool
}

// OpenSwitchable grabs an initial set of devices, with Open's semantics: a
// device that cannot be grabbed is skipped, and it fails only if none could be.
func OpenSwitchable(paths ...string) (*Switchable, error) {
	s := newSwitchable(nil)
	s.open = func(p []string) (capture, error) { return openSource(s.deviceLost, p...) }
	if err := s.Switch(paths...); err != nil {
		return nil, err
	}
	return s, nil
}

func newSwitchable(open func([]string) (capture, error)) *Switchable {
	return &Switchable{out: make(chan keys.Event, 64), open: open, keysUp: keysUpTimeout}
}

// SetNotify routes the notices of a keyboard going away to the engine's Log
// pane. Until it is set they only go to the log file.
func (s *Switchable) SetNotify(n func(text string, isErr bool)) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.notify = n
}

// deviceLost is told by a reader that its device went away. It takes no s.mu:
// a Switch holding it waits for that reader to stop.
func (s *Switchable) deviceLost(path string, err error, left int) {
	text := fmt.Sprintf("keyboard capture: %s went away (%v), unplugged or asleep; ", path, err)
	if left == 0 {
		text += "nothing is captured now: reconnect it and choose it in the Keyboards pane"
	} else {
		text += fmt.Sprintf("still capturing %d other device(s)", left)
	}
	s.notifyMu.Lock()
	n := s.notify
	s.notifyMu.Unlock()
	if n != nil {
		n(text, true)
		return
	}
	log.Warn(text)
}

// Events is the stable channel the engine reads.
//
// It is never closed. A retired pump may still be draining its Source after
// Switch returns, so closing this channel would risk a send on a closed
// channel. The engine shuts down on its context rather than on this channel
// ending, so nothing needs the close.
func (s *Switchable) Events() <-chan keys.Event { return s.out }

// Switch re-grabs, replacing the captured devices.
//
// The old devices are released FIRST, because the sets usually overlap: going
// from one node of a keyboard to both of them, or re-selecting the same device,
// would otherwise fail against this process's own existing grab. The cost is a
// few milliseconds with nothing captured, and that on an action a human just
// took. If the new set cannot be grabbed, the previous one is restored and the
// error says whether that worked.
//
// They are released only once no key is down on them, the grab side's rule
// the other way round (see waitKeysUp): the Keyboards pane switches on the
// press of Enter or Space, whose release would otherwise go to the compositor
// and leave the virtual key down (Kamino 138). A key still held after the
// timeout refuses the switch, as a grab refuses the device.
func (s *Switchable) Switch(paths ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("source is closed")
	}
	// Already exactly this: re-grabbing would fight itself. Unless a device
	// went away, when choosing it again is how it comes back.
	if s.cur != nil && s.cur.src.Lost() == 0 && samePaths(s.cur.src.Paths(), paths) {
		return nil
	}

	var previous []string
	if s.cur != nil {
		if err := s.cur.src.KeysUp(s.keysUp); err != nil {
			return fmt.Errorf("the keyboard being released: %w", err)
		}
		previous = s.cur.src.Paths()
		_ = s.release(s.cur, time.Second)
		s.cur = nil
	}

	next, err := s.open(paths)
	if err != nil {
		if len(previous) == 0 {
			return err
		}
		back, rerr := s.open(previous)
		if rerr != nil {
			return fmt.Errorf("%w (and the previous keyboard could not be re-grabbed: %v)", err, rerr)
		}
		s.cur = s.start(back)
		return fmt.Errorf("%w (previous keyboard restored)", err)
	}

	s.cur = s.start(next)
	return nil
}

func (s *Switchable) start(src capture) *feed {
	f := &feed{src: src, stop: make(chan struct{}), down: map[ev.EvCode]bool{}}
	go s.pump(f)
	return f
}

// pump forwards one capture's events to the stable channel, until it is
// retired.
//
// It takes no s.mu, deliberately. An earlier version checked "am I still the
// current source?" under s.mu per event and deadlocked: Switch held s.mu while
// waiting for the old pump to drain, and the pump could not drain because it
// was blocked on that same mutex. The retired check is under the feed's own
// lock, which a send gives up when the feed is stopped.
func (s *Switchable) pump(f *feed) {
	for e := range f.src.Events() {
		f.mu.Lock()
		if !f.retired {
			select {
			case s.out <- e:
				switch e.State {
				case keys.Down:
					f.down[e.Code] = true
				case keys.Up:
					delete(f.down, e.Code)
				}
			case <-f.stop:
			}
		}
		f.mu.Unlock()
	}
}

// release retires a feed and releases its devices.
//
// Nothing the retired capture produces is forwarded any more (Kamino 186): an
// event from a device no longer grabbed is a press whose release goes to the
// compositor. And whatever it pressed in the stable channel without releasing,
// a release still in flight when it was retired, is released here, so the
// engine is never left holding a key of a device it no longer reads. wait is
// how long to try to hand those on before giving up on an engine that has gone.
func (s *Switchable) release(f *feed, wait time.Duration) error {
	close(f.stop)
	f.mu.Lock()
	f.retired = true
	ups := make([]keys.Event, 0, len(f.down))
	for code := range f.down {
		ups = append(ups, keys.Event{Code: code, State: keys.Up})
	}
	f.mu.Unlock()
	err := f.src.Close()
	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	for _, e := range ups {
		select {
		case s.out <- e:
			continue
		default:
		}
		select {
		case s.out <- e:
		case <-timeout.C:
			return err
		}
	}
	return err
}

// Close releases the captured devices, after waiting for held keys as Switch
// does (but releasing them anyway after the timeout: quitting is not refused).
// The event channel is left open (see Events) and Switch is refused afterwards.
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
	if err := s.cur.src.KeysUp(s.keysUp); err != nil {
		log.Warnf("keyboard capture: releasing anyway: %v", err)
	}
	f := s.cur
	s.cur = nil
	return s.release(f, 0)
}

// Paths is what is currently captured.
func (s *Switchable) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	return s.cur.src.Paths()
}

// Skipped is why anything asked for is not being captured.
func (s *Switchable) Skipped() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur == nil {
		return nil
	}
	return s.cur.src.Skipped()
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
