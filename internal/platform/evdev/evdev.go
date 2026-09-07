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

	ev "github.com/holoplot/go-evdev"

	"github.com/malahmen/clonecast/internal/keys"
)

// VirtualName is the device name of the injected keyboard. Devices carrying
// this name are skipped during discovery so clonecast never grabs itself.
const VirtualName = "clonecast virtual keyboard"

// Source grabs physical keyboards and merges their key events.
type Source struct {
	devs []*ev.InputDevice
	ch   chan keys.Event
	wg   sync.WaitGroup
	once sync.Once
}

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

	s := &Source{ch: make(chan keys.Event, 64)}
	for _, p := range paths {
		d, err := ev.Open(p)
		if err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("open %s: %w", p, err)
		}
		if err := d.Grab(); err != nil {
			_ = d.Close()
			_ = s.Close()
			return nil, fmt.Errorf("grab %s: %w", p, err)
		}
		s.devs = append(s.devs, d)
		s.wg.Add(1)
		go s.read(d)
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
		s.ch <- keys.Event{Code: e.Code, State: keys.State(e.Value)}
	}
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
