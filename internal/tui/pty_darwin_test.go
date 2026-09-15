//go:build darwin

package tui

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// macOS clones a pty from /dev/ptmx and hands the slave's name back through
// TIOCPTYGNAME, after granting and unlocking it. (Linux's equivalents live in
// pty_linux_test.go; the two are the reason this is build-tagged rather than a
// runtime switch.)
const (
	tiocptygrant = 0x20007454
	tiocptyunlk  = 0x20007452
	tiocptygname = 0x40807453
)

func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, req := range []uintptr{tiocptygrant, tiocptyunlk} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), req, 0); e != 0 {
			m.Close()
			return nil, nil, fmt.Errorf("ioctl 0x%x: %w", req, e)
		}
	}
	var buf [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), tiocptygname, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", e)
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	s, err := os.OpenFile(string(buf[:n]), os.O_RDWR, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
