//go:build linux

package keys

import (
	"testing"

	evdev "github.com/holoplot/go-evdev"
)

// The generated table must be go-evdev's, entry for entry, so -toggle, -keys
// and the TUI accept and print exactly the names they did when keys used
// go-evdev's maps directly, and so a go-evdev bump without `go generate` is
// caught here (Kamino 278).
func TestTableMatchesGoEvdev(t *testing.T) {
	if len(keyNames) != len(evdev.KEYNames) {
		t.Errorf("keyNames has %d entries, go-evdev KEYNames %d", len(keyNames), len(evdev.KEYNames))
	}
	for c, n := range evdev.KEYNames {
		if got, ok := keyNames[Code(c)]; !ok || got != n {
			t.Errorf("keyNames[%#x] = %q (present %v), go-evdev has %q", uint16(c), got, ok, n)
		}
	}
	if len(keyFromString) != len(evdev.KEYFromString) {
		t.Errorf("keyFromString has %d entries, go-evdev KEYFromString %d", len(keyFromString), len(evdev.KEYFromString))
	}
	for n, c := range evdev.KEYFromString {
		if got, ok := keyFromString[n]; !ok || got != Code(c) {
			t.Errorf("keyFromString[%q] = %#x (present %v), go-evdev has %#x", n, uint16(got), ok, uint16(c))
		}
	}
}
