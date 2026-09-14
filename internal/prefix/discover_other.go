//go:build !linux

package prefix

// Stubs for the macOS development build (REFERENCE.md 5.3: build tags, not
// runtime checks). Prefix discovery and the "is this prefix booted?" check both
// read /proc, which only Linux has; everything else in this package — the
// system.reg editor, the exe copy, the wine-command route — is portable and
// unit-tested here.

// Discover is Linux-only.
func Discover() ([]Proc, error) { return nil, ErrNoProc }

// Busy is Linux-only. Callers must either supply a wine command (so the route
// does not need the answer) or pass --assume-idle to say the prefix is down.
func Busy(path string) (bool, []Running, error) { return false, nil, ErrNoProc }

// PrefixOfPID is Linux-only: there is no /proc to read an environment out of.
// Callers pair by the agent's announced window title instead (REFERENCE.md
// 4.17), which is also the fallback on Linux when a pid is unreadable.
func PrefixOfPID(pid int) (string, error) { return "", ErrNoProc }
