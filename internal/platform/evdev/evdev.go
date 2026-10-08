//go:build linux

// Package evdev implements the broadcast.Source and broadcast.Injector on
// Linux using the kernel's evdev and uinput interfaces.
//
// Capture: every device under /dev/input that looks like a keyboard is opened
// and grabbed with EVIOCGRAB, so no other process (including the compositor)
// sees its events. This is the only capture method that works on Wayland.
//
// Injection: a uinput virtual keyboard is created by cloning the capabilities
// and vendor/product IDs of the first physical keyboard, so to the compositor
// and to applications it is indistinguishable from real hardware.
//
// Requirements: read access to /dev/input/event* (the "input" group) and write
// access to /dev/uinput (see scripts/setup-bazzite.sh).
package evdev

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"

	ev "github.com/holoplot/go-evdev"

	"github.com/malahmen/clonecast/internal/keys"
)

// VirtualName is the device name of the injected keyboard. Devices carrying
// this name are skipped during discovery so clonecast never grabs itself.
const VirtualName = "clonecast virtual keyboard"

// Source grabs physical keyboards and merges their key events.
// dupWindow is how close together the same key-down may arrive from two
// DIFFERENT devices before the second is treated as a duplicate rather than a
// second press.
//
// One physical keyboard commonly exposes several event nodes that all report
// the same keys — the ASUS Strix presents input0 and input1, a Razer mouse
// presents three — so grabbing every discovered keyboard can see each press
// twice. Twice is not harmless: a tick toggles on and straight back off, so
// Enter appears to do nothing and the first target flickers.
//
// Deduplicating EVENTS rather than devices avoids having to guess which node
// of a physical device carries typing; guessing wrong captures nothing, which
// is worse than doubling. No human produces the same key-down twice in 5ms,
// and kernel autorepeat is both slower and comes from the same device, which
// this never touches.
const dupWindow = 5 * time.Millisecond

type Source struct {
	devs    []*ev.InputDevice
	paths   []string
	skipped []string
	ch      chan keys.Event
	wg      sync.WaitGroup
	once    sync.Once

	dupMu   sync.Mutex
	lastSrc map[dupKey]dupSeen
	dupN    uint64
	dupLog  sync.Once
}

type dupKey struct {
	code  ev.EvCode
	state keys.State
}

type dupSeen struct {
	dev *ev.InputDevice
	at  time.Time
}

// duplicate reports whether this event just arrived from a different device,
// and records it either way. Only a different device can suppress: with one
// device grabbed this can never drop anything, so the common case is
// behaviour-identical to not having it.
func (s *Source) duplicate(d *ev.InputDevice, k dupKey) bool {
	now := time.Now()
	s.dupMu.Lock()
	defer s.dupMu.Unlock()
	if prev, ok := s.lastSrc[k]; ok && prev.dev != d && now.Sub(prev.at) < dupWindow {
		s.dupN++
		s.dupLog.Do(func() {
			log.Infof("evdev: ignoring duplicate key events — more than one grabbed device reports the same keys; --keyboard picks one")
		})
		// Not recorded: the surviving event stays the reference, so a third
		// node reporting the same press is also suppressed.
		return true
	}
	s.lastSrc[k] = dupSeen{dev: d, at: now}
	return false
}

// Coalesced is how many duplicate events have been dropped.
func (s *Source) Coalesced() uint64 {
	s.dupMu.Lock()
	defer s.dupMu.Unlock()
	return s.dupN
}

// Skipped returns one line per device that could not be opened or grabbed, so
// the caller can say which keyboards are NOT being captured and why. Empty
// when everything asked for was grabbed.
func (s *Source) Skipped() []string { return s.skipped }

// Discover returns the paths of devices that look like keyboards. The
// heuristic is: reports EV_KEY, and has both KEY_A and KEY_ENTER. That
// excludes mice, gamepads and power buttons while keeping laptop and USB
// keyboards.
func Discover() ([]ev.InputPath, error) {
	all, err := ev.ListDevicePaths()
	if err != nil {
		return nil, err
	}
	var out []ev.InputPath
	for _, p := range all {
		if strings.Contains(p.Name, VirtualName) {
			continue
		}
		d, err := ev.OpenWithFlags(p.Path, syscall.O_RDONLY|syscall.O_NONBLOCK)
		if err != nil {
			continue
		}
		isKbd := hasKey(d, ev.KEY_A) && hasKey(d, ev.KEY_ENTER)
		_ = d.Close()
		if isKbd {
			out = append(out, p)
		}
	}
	return out, nil
}

func hasKey(d *ev.InputDevice, code ev.EvCode) bool {
	for _, c := range d.CapableEvents(ev.EV_KEY) {
		if c == code {
			return true
		}
	}
	return false
}

// CanGrab reports whether a device can be grabbed right now, without keeping
// the grab. --list-keyboards uses it: a device another process holds
// exclusively (keyd, input-remapper) cannot be captured, and knowing which
// before starting is the difference between choosing a device and guessing.
func CanGrab(path string) error {
	d, err := ev.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Grab(); err != nil {
		return err
	}
	_ = d.Ungrab()
	return nil
}

// Open grabs the given devices (or every discovered keyboard when paths is
// empty) and starts reading.
func Open(paths ...string) (*Source, error) {
	if len(paths) == 0 {
		found, err := Discover()
		if err != nil {
			return nil, err
		}
		for _, p := range found {
			paths = append(paths, p.Path)
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("no keyboard found under /dev/input (are you in the input group?)")
	}

	s := &Source{ch: make(chan keys.Event, 64), lastSrc: make(map[dupKey]dupSeen)}
	// A device that cannot be opened or grabbed is SKIPPED, not fatal, and the
	// reason is collected for the caller to report. Another process holding an
	// exclusive grab is normal rather than exceptional: keyd and
	// input-remapper both work by grabbing a keyboard and re-emitting it, so
	// the physical device is busy by design and the virtual one it publishes
	// is the device to capture. Aborting on the first EBUSY meant clonecast
	// could not start at all on such a machine, even though a perfectly good
	// keyboard was available.
	for _, p := range paths {
		d, err := ev.Open(p)
		if err != nil {
			s.skipped = append(s.skipped, fmt.Sprintf("%s: open: %v", p, err))
			continue
		}
		if err := d.Grab(); err != nil {
			_ = d.Close()
			s.skipped = append(s.skipped, fmt.Sprintf("%s: grab: %v", p, err))
			continue
		}
		s.devs = append(s.devs, d)
		s.paths = append(s.paths, p)
		s.wg.Add(1)
		go s.read(d)
	}
	if len(s.devs) == 0 {
		_ = s.Close()
		return nil, fmt.Errorf("no keyboard could be grabbed (%s)", strings.Join(s.skipped, "; "))
	}
	go func() {
		s.wg.Wait()
		close(s.ch)
	}()
	return s, nil
}

func (s *Source) read(d *ev.InputDevice) {
	defer s.wg.Done()
	for {
		e, err := d.ReadOne()
		if err != nil {
			return // device closed or unplugged
		}
		if e.Type != ev.EV_KEY {
			continue
		}
		k := dupKey{code: e.Code, state: keys.State(e.Value)}
		if s.duplicate(d, k) {
			continue
		}
		s.ch <- keys.Event{Code: e.Code, State: keys.State(e.Value)}
	}
}

// Paths returns the devices actually grabbed, which is not necessarily what was
// asked for: one that was busy or missing is skipped (see Skipped).
func (s *Source) Paths() []string {
	out := make([]string, len(s.paths))
	copy(out, s.paths)
	return out
}

// Devices returns the grabbed devices, mainly so the Injector can clone one.
func (s *Source) Devices() []*ev.InputDevice { return s.devs }

func (s *Source) Events() <-chan keys.Event { return s.ch }

// Close ungrabs and closes every device, which also ends the reader goroutines.
func (s *Source) Close() error {
	var first error
	s.once.Do(func() {
		for _, d := range s.devs {
			_ = d.Ungrab()
			if err := d.Close(); err != nil && first == nil {
				first = err
			}
		}
	})
	return first
}

// Injector writes key events to a uinput virtual keyboard.
type Injector struct {
	dev *ev.InputDevice
}

// NewInjector creates the virtual keyboard by cloning template. Pass the first
// grabbed physical keyboard. The compositor needs a moment to pick up a new
// device; callers should wait ~500ms before the first Emit.
func NewInjector(template *ev.InputDevice) (*Injector, error) {
	d, err := ev.CloneDevice(VirtualName, template)
	if err != nil {
		return nil, fmt.Errorf("create uinput device (is /dev/uinput writable?): %w", err)
	}
	return &Injector{dev: d}, nil
}

// Emit writes the key transition followed by the SYN_REPORT that tells the
// kernel the event is complete.
func (i *Injector) Emit(e keys.Event) error {
	if err := i.dev.WriteOne(&ev.InputEvent{Type: ev.EV_KEY, Code: e.Code, Value: int32(e.State)}); err != nil {
		return err
	}
	return i.dev.WriteOne(&ev.InputEvent{Type: ev.EV_SYN, Code: ev.SYN_REPORT, Value: 0})
}

func (i *Injector) Close() error {
	return ev.DestroyDevice(i.dev)
}
