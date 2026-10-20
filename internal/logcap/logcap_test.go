package logcap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheFileNeverPassesTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	l, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := strings.Repeat("a", 29) + "\n" // 30 bytes
	for i := 0; i < 10; i++ {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	cur, _ := os.ReadFile(path)
	prev, _ := os.ReadFile(path + ".1")
	if len(cur) > 100 || len(prev) > 100 {
		t.Fatalf("sizes %d and %d, want both at most 100", len(cur), len(prev))
	}
	// 10 lines of 30: three per file, so the last one is in the current file
	// and the 3 before it in the previous one.
	if len(cur) != 30 || len(prev) != 90 {
		t.Fatalf("current %d bytes, previous %d; want 30 and 90", len(cur), len(prev))
	}
}

// A log already near the cap when opened (an earlier run's) rotates on the
// first write that would pass it.
func TestAnExistingFileCountsTowardsTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("o", 90)), 0o644); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := l.Write([]byte(strings.Repeat("n", 20))); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(path)
	prev, _ := os.ReadFile(path + ".1")
	if string(cur) != strings.Repeat("n", 20) || len(prev) != 90 {
		t.Fatalf("current %q, previous %d bytes; want the new line alone and the old 90", cur, len(prev))
	}
}

// One line longer than the cap is written whole rather than lost.
func TestALineLongerThanTheCapIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	l, err := Open(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if n, err := l.Write([]byte(strings.Repeat("z", 50))); err != nil || n != 50 {
		t.Fatalf("wrote %d, %v", n, err)
	}
}
