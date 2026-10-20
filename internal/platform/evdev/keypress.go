//go:build linux

package evdev

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

// ErrNoKeypress is WaitForKeyPress giving up: no key went down in time.
var ErrNoKeypress = errors.New("no key was pressed")

// inputEventSize is struct input_event on a 64-bit kernel: a 16-byte timeval,
// then type (u16), code (u16) and value (s32).
const inputEventSize = 24

// WaitForKeyPress reports which of paths a key next goes DOWN on, as an index
// into paths, without grabbing any of them. It is how the picker asks "which
// keyboard do you type on": one keyboard commonly exposes several nodes that
// all claim the same keys (the ASUS Strix has two), discovery cannot tell them
// apart, and only a press says which one carries the typing. Choosing from a
// list instead picked the silent node and captured nothing.
//
// Raw non-blocking fds and poll(2), not this package's readers: a blocking
// ReadOne is not reliably woken by closing its fd (see Switchable.Events), and
// this must stop listening the moment it has its answer. Whatever is already
// queued when it starts — the Enter that ran the command — is discarded first.
func WaitForKeyPress(paths []string, timeout time.Duration) (int, error) {
	fds := make([]unix.PollFd, 0, len(paths))
	idx := make([]int, 0, len(paths))
	defer func() {
		for _, p := range fds {
			if p.Fd >= 0 {
				_ = unix.Close(int(p.Fd))
			}
		}
	}()
	for i, p := range paths {
		fd, err := unix.Open(p, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // unreadable: it cannot be the answer
		}
		fds = append(fds, unix.PollFd{Fd: int32(fd), Events: unix.POLLIN})
		idx = append(idx, i)
	}
	if len(fds) == 0 {
		return -1, fmt.Errorf("none of the keyboards could be opened")
	}

	buf := make([]byte, inputEventSize*64)
	drain := func(fd int) (pressed bool) {
		for {
			n, err := unix.Read(fd, buf)
			if n <= 0 || err != nil {
				return pressed
			}
			for off := 0; off+inputEventSize <= n; off += inputEventSize {
				typ := binary.LittleEndian.Uint16(buf[off+16:])
				val := int32(binary.LittleEndian.Uint32(buf[off+20:]))
				if typ == 1 && val == 1 { // EV_KEY, key down (not up, not repeat)
					pressed = true
				}
			}
		}
	}
	for _, p := range fds {
		drain(int(p.Fd))
	}

	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return -1, ErrNoKeypress
		}
		n, err := unix.Poll(fds, int(left/time.Millisecond)+1)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return -1, fmt.Errorf("poll: %w", err)
		}
		if n == 0 {
			continue
		}
		live := 0
		for i := range fds {
			if fds[i].Revents&unix.POLLIN != 0 && drain(int(fds[i].Fd)) {
				return idx[i], nil
			}
			// Unplugged mid-wait: poll would report it on every call and the
			// loop would spin. A negative fd is one poll ignores; closed below.
			if fds[i].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 && fds[i].Fd >= 0 {
				_ = unix.Close(int(fds[i].Fd))
				fds[i].Fd = -1
			}
			fds[i].Revents = 0
			if fds[i].Fd >= 0 {
				live++
			}
		}
		if live == 0 {
			return -1, fmt.Errorf("every keyboard went away while waiting")
		}
	}
}
