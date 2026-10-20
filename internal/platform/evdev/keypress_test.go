//go:build linux

package evdev

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// FIFOs stand in for event nodes: WaitForKeyPress only opens, polls and
// reads 24-byte input_event records, so no uinput device or input group is
// needed.

func event(typ uint16, code uint16, val int32) []byte {
	b := make([]byte, inputEventSize)
	binary.LittleEndian.PutUint16(b[16:], typ)
	binary.LittleEndian.PutUint16(b[18:], code)
	binary.LittleEndian.PutUint32(b[20:], uint32(val))
	return b
}

func fifos(t *testing.T, n int) []string {
	t.Helper()
	dir := t.TempDir()
	var out []string
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, "event"+string(rune('0'+i)))
		if err := unix.Mkfifo(p, 0o600); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// writeAfter opens a FIFO for writing (which waits for the reader
// WaitForKeyPress opens), waits, writes the records and keeps it open until
// the test ends.
func writeAfter(t *testing.T, path string, wait time.Duration, recs ...[]byte) {
	t.Helper()
	go func() {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		time.Sleep(wait)
		for _, r := range recs {
			_, _ = f.Write(r)
		}
		time.Sleep(2 * time.Second)
		_ = f.Close()
	}()
}

func TestWaitForKeyPressReportsTheDeviceTheKeyWentDownOn(t *testing.T) {
	p := fifos(t, 3)
	// The silent node of a two-node keyboard: something, but never a key down.
	writeAfter(t, p[0], 50*time.Millisecond, event(4, 4, 7), event(0, 0, 0))
	writeAfter(t, p[1], 150*time.Millisecond, event(1, 42, 1), event(0, 0, 0)) // LEFTSHIFT down
	writeAfter(t, p[2], 0)
	i, err := WaitForKeyPress(p, 3*time.Second)
	if err != nil || i != 1 {
		t.Fatalf("WaitForKeyPress = %d, %v; want 1 (the node the key went down on)", i, err)
	}
}

func TestWaitForKeyPressIgnoresReleasesAndRepeats(t *testing.T) {
	p := fifos(t, 1)
	writeAfter(t, p[0], 100*time.Millisecond, event(1, 28, 0), event(1, 28, 2))
	if _, err := WaitForKeyPress(p, 600*time.Millisecond); !errors.Is(err, ErrNoKeypress) {
		t.Fatalf("err = %v, want ErrNoKeypress: a release or a repeat is not a choice", err)
	}
}

func TestWaitForKeyPressStopsWhenEveryDeviceGoesAway(t *testing.T) {
	p := fifos(t, 1)
	go func() { // a writer that opens and closes at once: the reader sees a hang-up
		f, err := os.OpenFile(p[0], os.O_WRONLY, 0)
		if err == nil {
			time.Sleep(100 * time.Millisecond)
			_ = f.Close()
		}
	}()
	start := time.Now()
	_, err := WaitForKeyPress(p, 5*time.Second)
	if err == nil || errors.Is(err, ErrNoKeypress) {
		t.Fatalf("err = %v, want the devices-gone error", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("waited %v: a hang-up should end the wait, not spin until the timeout", time.Since(start))
	}
}
