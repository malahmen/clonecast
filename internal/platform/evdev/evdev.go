//go:build linux

// Package evdev implements the broadcast.Source and broadcast.Injector on
// Linux using the kernel's evdev and uinput interfaces.
//
// Capture: every device under /dev/input that looks like a keyboard is opened
// and grabbed with EVIOCGRAB, so no other process (including the compositor)
// sees its events. This is the only capture method that works on Wayland.
//
// Injection: a uinput virtual keyboard is created with the capabilities and
// vendor/product IDs of the first physical keyboard, plus the keys of every
// other candidate keyboard, so to the compositor and to applications it is
// indistinguishable from real hardware.
//
// Requirements: read access to /dev/input/event* (the "input" group) and write
// access to /dev/uinput (see scripts/setup-bazzite.sh).
package evdev

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/charmbracelet/log"
	"golang.org/x/sys/unix"

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
	devs    []captured
	paths   []string
	skipped []string
	ch      chan keys.Event
	wg      sync.WaitGroup
	once    sync.Once

	// The readers poll their device and wake, the read end of a pipe whose
	// write end Close closes: that is what stops them. Closing a device's fd
	// does not wake a reader blocked on it (Kamino 186).
	wake, wakeW int
	done        chan struct{}
	// lost is told when a device goes away under its reader. lostN counts
	// them, so a Switch to the same paths knows the capture is not intact.
	lost  func(path string, err error, left int)
	lostN atomic.Int32

	dupMu   sync.Mutex
	lastSrc map[dupKey]dupSeen
	dupN    uint64
	dupLog  sync.Once
}

// captured is one grabbed device: its path, the raw non-blocking fd its
// reader polls, and the grab and key-state ioctls on that fd.
type captured struct {
	path string
	fd   int
	dev  device
}

// device is what a Source does to a grabbed device besides reading it.
type device interface {
	grabber
	Close() error
}

type dupKey struct {
	code  ev.EvCode
	state keys.State
}

type dupSeen struct {
	dev int
	at  time.Time
}

// duplicate reports whether this event just arrived from a different device,
// and records it either way. Only a different device can suppress: with one
// device grabbed this can never drop anything, so the common case is
// behaviour-identical to not having it. Devices are told apart by their fd.
func (s *Source) duplicate(d int, k dupKey) bool {
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
	if err := grabKeysUp(d, keysUpTimeout); err != nil {
		return err
	}
	_ = d.Ungrab()
	return nil
}

// keysUpTimeout is how long a grab waits for the keys held on a device to be
// released before giving up on it.
const keysUpTimeout = 3 * time.Second

// grabber is the part of *ev.InputDevice grabKeysUp needs.
type grabber interface {
	State(ev.EvType) (ev.StateMap, error)
	Grab() error
	Ungrab() error
}

// grabKeysUp grabs d only while no key on it is down. A key held across a
// grab is pressed for the compositor but released to clonecast, so the
// compositor thinks it is held for good and ignores every later press of it,
// even after clonecast exits: the Enter that started clonecast, or the Shift
// pressed for the keyboard picker, then stopped working until a reboot.
//
// The state is checked again after the grab, because a key can go down
// between the check and the grab. Ungrabbing then lets its release reach the
// compositor, and the wait starts over.
func grabKeysUp(d grabber, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		held, err := heldKeys(d)
		if err != nil {
			return err
		}
		if held == 0 {
			if err := d.Grab(); err != nil {
				return err
			}
			if held, err = heldKeys(d); err == nil && held == 0 {
				return nil
			}
			_ = d.Ungrab()
			if err != nil {
				return err
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d key(s) still held after %v; release them and try again", held, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func heldKeys(d grabber) (int, error) {
	st, err := d.State(ev.EV_KEY)
	if err != nil {
		return 0, fmt.Errorf("read key state: %w", err)
	}
	n := 0
	for _, down := range st {
		if down {
			n++
		}
	}
	return n, nil
}

// waitKeysUp waits until no key is down on any of devs. Releasing a grab
// while a key is down has grabKeysUp's problem the other way round: the key's
// press reached clonecast, which passed it to the virtual keyboard, and its
// release reaches only the compositor, so the virtual key stays down for good
// (Kamino 138). A device whose state cannot be read is gone, and holds nothing.
func waitKeysUp(devs []grabber, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		held := 0
		for _, d := range devs {
			if n, err := heldKeys(d); err == nil {
				held += n
			}
		}
		if held == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d key(s) still held after %v; release them and try again", held, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The evdev ioctls a Source needs, on its own fds. go-evdev's helpers go
// through os.File.Fd(), which puts the fd back in blocking mode (Kamino 186).
const (
	eviocgversion = 0x80044501 // EVIOCGVERSION: fails on anything but an event node
	eviocgrab     = 0x40044590 // EVIOCGRAB
	eviocgkey     = 0x80604518 // EVIOCGKEY(KEY_CNT/8): one bit per key, set while it is down
)

// rawDev is a grabbed event node, driven through its raw fd.
type rawDev struct{ fd int }

func (r rawDev) Grab() error   { return unix.IoctlSetInt(r.fd, eviocgrab, 1) }
func (r rawDev) Ungrab() error { return unix.IoctlSetInt(r.fd, eviocgrab, 0) }
func (r rawDev) Close() error  { return unix.Close(r.fd) }

// State reads the key state; it is only ever asked for EV_KEY.
func (r rawDev) State(ev.EvType) (ev.StateMap, error) {
	var bits [ev.KEY_CNT / 8]byte
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(r.fd), eviocgkey, uintptr(unsafe.Pointer(&bits[0]))); errno != 0 {
		return nil, errno
	}
	st := ev.StateMap{}
	for i, b := range bits {
		for j := 0; j < 8; j++ {
			if b&(1<<j) != 0 {
				st[ev.EvCode(i*8+j)] = true
			}
		}
	}
	return st, nil
}

func openRaw(path string) (captured, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return captured{}, err
	}
	if _, err := unix.IoctlGetUint32(fd, eviocgversion); err != nil {
		_ = unix.Close(fd)
		return captured{}, fmt.Errorf("not an input device: %w", err)
	}
	return captured{path: path, fd: fd, dev: rawDev{fd: fd}}, nil
}

// Open grabs the given devices (or every discovered keyboard when paths is
// empty) and starts reading.
func Open(paths ...string) (*Source, error) { return openSource(nil, paths...) }

// openSource is Open, telling lost when a device goes away under its reader:
// left is how many of this Source's devices are still there.
func openSource(lost func(path string, err error, left int), paths ...string) (*Source, error) {
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

	s := newSource(lost)
	// A device that cannot be opened or grabbed is SKIPPED, not fatal, and the
	// reason is collected for the caller to report. Another process holding an
	// exclusive grab is normal rather than exceptional: keyd and
	// input-remapper both work by grabbing a keyboard and re-emitting it, so
	// the physical device is busy by design and the virtual one it publishes
	// is the device to capture. Aborting on the first EBUSY meant clonecast
	// could not start at all on such a machine, even though a perfectly good
	// keyboard was available.
	for _, p := range paths {
		c, err := openRaw(p)
		if err != nil {
			s.skipped = append(s.skipped, fmt.Sprintf("%s: open: %v", p, err))
			continue
		}
		if err := grabKeysUp(c.dev, keysUpTimeout); err != nil {
			_ = c.dev.Close()
			s.skipped = append(s.skipped, fmt.Sprintf("%s: grab: %v", p, err))
			continue
		}
		s.devs = append(s.devs, c)
		s.paths = append(s.paths, p)
	}
	if len(s.devs) == 0 {
		return nil, fmt.Errorf("no keyboard could be grabbed (%s)", strings.Join(s.skipped, "; "))
	}
	if err := s.start(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func newSource(lost func(path string, err error, left int)) *Source {
	return &Source{
		ch:      make(chan keys.Event, 64),
		lastSrc: make(map[dupKey]dupSeen),
		wake:    -1,
		wakeW:   -1,
		done:    make(chan struct{}),
		lost:    lost,
	}
}

// start opens the wake pipe and starts one reader per device.
func (s *Source) start() error {
	var p [2]int
	if err := unix.Pipe2(p[:], unix.O_NONBLOCK|unix.O_CLOEXEC); err != nil {
		return fmt.Errorf("wake pipe: %w", err)
	}
	s.wake, s.wakeW = p[0], p[1]
	for _, c := range s.devs {
		s.wg.Add(1)
		go s.read(c)
	}
	go func() {
		s.wg.Wait()
		close(s.ch)
	}()
	return nil
}

// read forwards one device's key events until Close wakes it or the device
// goes away. poll(2) on the device and the wake pipe, as WaitForKeyPress does:
// a blocking read is not woken by closing its fd, so a reader of a released
// keyboard used to forward one more event, a press whose release then went to
// the compositor (Kamino 186).
func (s *Source) read(c captured) {
	defer s.wg.Done()
	down := map[ev.EvCode]bool{} // what this device has pressed and not released
	buf := make([]byte, inputEventSize*64)
	fds := []unix.PollFd{
		{Fd: int32(c.fd), Events: unix.POLLIN},
		{Fd: int32(s.wake), Events: unix.POLLIN},
	}
	for {
		fds[0].Revents, fds[1].Revents = 0, 0
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			s.gone(c, down, fmt.Errorf("poll: %w", err))
			return
		}
		if fds[1].Revents != 0 {
			return // Close: nothing more is read, even if it is already queued
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				s.gone(c, down, errors.New("hung up"))
				return
			}
			continue
		}
		n, err := unix.Read(c.fd, buf)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			s.gone(c, down, err)
			return
		}
		if n == 0 {
			s.gone(c, down, errors.New("end of file"))
			return
		}
		for off := 0; off+inputEventSize <= n; off += inputEventSize {
			if binary.LittleEndian.Uint16(buf[off+16:]) != uint16(ev.EV_KEY) {
				continue
			}
			e := keys.Event{
				Code:  ev.EvCode(binary.LittleEndian.Uint16(buf[off+18:])),
				State: keys.State(int32(binary.LittleEndian.Uint32(buf[off+20:]))),
			}
			if s.duplicate(c.fd, dupKey{code: e.Code, state: e.State}) {
				continue
			}
			switch e.State {
			case keys.Down:
				down[e.Code] = true
			case keys.Up:
				delete(down, e.Code)
			}
			if !s.send(e) {
				return
			}
		}
	}
}

// gone handles a device that went away under its reader: unplugged, or a
// Bluetooth keyboard gone to sleep. Its held keys are released, since their
// releases will never come, and the loss is reported: silence looked exactly
// like clonecast ignoring the keyboard (Kamino 202).
func (s *Source) gone(c captured, down map[ev.EvCode]bool, err error) {
	for code := range down {
		if !s.send(keys.Event{Code: code, State: keys.Up}) {
			break
		}
	}
	left := len(s.devs) - int(s.lostN.Add(1))
	if s.lost != nil {
		s.lost(c.path, err, left)
	} else {
		log.Warnf("keyboard capture: %s went away (%v)", c.path, err)
	}
}

// send hands an event on, unless the Source is closing and nobody reads.
func (s *Source) send(e keys.Event) bool {
	select {
	case s.ch <- e:
		return true
	case <-s.done:
		return false
	}
}

// KeysUp waits until no key is down on any grabbed device (see waitKeysUp).
func (s *Source) KeysUp(timeout time.Duration) error {
	gs := make([]grabber, 0, len(s.devs))
	for _, c := range s.devs {
		gs = append(gs, c.dev)
	}
	return waitKeysUp(gs, timeout)
}

// Lost is how many grabbed devices have gone away since the grab.
func (s *Source) Lost() int { return int(s.lostN.Load()) }

// Paths returns the devices actually grabbed, which is not necessarily what was
// asked for: one that was busy or missing is skipped (see Skipped).
func (s *Source) Paths() []string {
	out := make([]string, len(s.paths))
	copy(out, s.paths)
	return out
}

func (s *Source) Events() <-chan keys.Event { return s.ch }

// Close stops the readers, then ungrabs and closes every device. The readers
// are stopped first, so none of them is still reading a device after its
// release, nor polling an fd that has been closed and reused.
func (s *Source) Close() error {
	var first error
	s.once.Do(func() {
		close(s.done)
		if s.wakeW >= 0 {
			_ = unix.Close(s.wakeW)
		}
		s.wg.Wait()
		for _, c := range s.devs {
			_ = c.dev.Ungrab()
			if err := c.dev.Close(); err != nil && first == nil {
				first = err
			}
		}
		if s.wake >= 0 {
			_ = unix.Close(s.wake)
		}
	})
	return first
}

// Injector writes key events to a uinput virtual keyboard.
type Injector struct {
	dev *ev.InputDevice
}

// NewInjector creates the virtual keyboard. It takes the identity and the
// event types of paths[0], the first captured keyboard, and the keys of every
// keyboard in paths and every one discovery finds. A clone of the first
// device alone could not type a key only another captured keyboard has, nor
// one on a keyboard switched to later: passthrough of it went nowhere. The
// device is not rebuilt on a switch (see deviceController.Capture), which is
// why it covers every candidate from the start (Kamino 202).
//
// The compositor needs a moment to pick up a new device; callers should wait
// ~500ms before the first Emit.
func NewInjector(paths []string) (*Injector, error) {
	if len(paths) == 0 {
		return nil, errors.New("no keyboard to model the virtual keyboard on")
	}
	tmpl, err := ev.OpenWithFlags(paths[0], syscall.O_RDONLY|syscall.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	id, err := tmpl.InputID()
	if err != nil {
		_ = tmpl.Close()
		return nil, fmt.Errorf("read %s's id: %w", paths[0], err)
	}
	caps := map[ev.EvType][]ev.EvCode{}
	for _, t := range tmpl.CapableTypes() {
		caps[t] = tmpl.CapableEvents(t)
	}
	_ = tmpl.Close()

	others := append([]string(nil), paths[1:]...)
	if found, err := Discover(); err == nil {
		for _, p := range found {
			others = append(others, p.Path)
		}
	}
	var extra [][]ev.EvCode
	for _, p := range others {
		d, err := ev.OpenWithFlags(p, syscall.O_RDONLY|syscall.O_NONBLOCK)
		if err != nil {
			continue
		}
		extra = append(extra, d.CapableEvents(ev.EV_KEY))
		_ = d.Close()
	}

	d, err := ev.CreateDevice(VirtualName, id, withKeys(caps, extra))
	if err != nil {
		return nil, fmt.Errorf("create uinput device (is /dev/uinput writable?): %w", err)
	}
	return &Injector{dev: d}, nil
}

// withKeys is caps with the keyboard keys of extra added to its EV_KEY codes.
// Only keyboard keys: the mouse buttons of a gaming mouse's macro interface
// (which discovery can select) would make the virtual keyboard look like a
// pointer to the compositor.
func withKeys(caps map[ev.EvType][]ev.EvCode, extra [][]ev.EvCode) map[ev.EvType][]ev.EvCode {
	out := make(map[ev.EvType][]ev.EvCode, len(caps)+1)
	for t, codes := range caps {
		out[t] = codes
	}
	codes := append([]ev.EvCode(nil), caps[ev.EV_KEY]...)
	have := make(map[ev.EvCode]bool, len(codes))
	for _, c := range codes {
		have[c] = true
	}
	for _, set := range extra {
		for _, c := range set {
			if !have[c] && keyboardKey(c) {
				have[c] = true
				codes = append(codes, c)
			}
		}
	}
	if len(codes) > 0 {
		out[ev.EV_KEY] = codes
	}
	return out
}

// keyboardKey is a key code outside the button ranges (BTN_MISC to KEY_OK, and
// the BTN_TRIGGER_HAPPY block).
func keyboardKey(c ev.EvCode) bool {
	switch {
	case c == 0:
		return false
	case c >= ev.BTN_MISC && c < ev.KEY_OK:
		return false
	case c >= ev.BTN_TRIGGER_HAPPY && c < ev.BTN_TRIGGER_HAPPY+0x28:
		return false
	}
	return c <= ev.KEY_MAX
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
