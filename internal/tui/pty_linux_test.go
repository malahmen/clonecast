//go:build linux

package tui

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Linux unlocks the slave with TIOCSPTLCK and names it by number (TIOCGPTN ->
// /dev/pts/N). macOS does the same job with three other ioctls; see
// pty_darwin_test.go.
func openPTY() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, err
	}
	unlock := 0
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("unlockpt: %w", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, m.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		m.Close()
		return nil, nil, fmt.Errorf("ptsname: %w", e)
	}
	s, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}
