package prefix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProc builds a procfs-shaped tree: /<pid>/environ (NUL separated) and
// /<pid>/cmdline. A nil env means "no environ file", which is what a process
// owned by another user looks like to us (EACCES) — it must be skipped, not
// fail the scan.
func fakeProc(t *testing.T, procs map[string][]string, cmdlines map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, env := range procs {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if env != nil {
			blob := strings.Join(env, "\x00") + "\x00"
			if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(blob), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if c, ok := cmdlines[pid]; ok {
			if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(c), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Non-process entries that a real /proc is full of.
	for _, name := range []string{"self", "sys", "cpuinfo", "1234abc"} {
		_ = os.MkdirAll(filepath.Join(root, name), 0o755)
	}
	return root
}

func TestScanProcFindsWinePrefixes(t *testing.T) {
	bottleA := t.TempDir()
	bottleB := t.TempDir()
	root := fakeProc(t, map[string][]string{
		// wineserver for bottle A: WINEPREFIX in the middle of the block
		"101": {"LANG=en_US.UTF-8", "WINEPREFIX=" + bottleA, "DISPLAY=:0"},
		// the game in the same bottle: WINEPREFIX first
		"102": {"WINEPREFIX=" + bottleA, "STEAM_COMPAT_DATA_PATH=/x"},
		// another bottle: WINEPREFIX last, with a trailing separator
		"103": {"HOME=/home/u", "WINEPREFIX=" + bottleB},
		// not a Wine process
		"104": {"HOME=/home/u", "PATH=/usr/bin"},
		// near misses that must not match
		"105": {"MY_WINEPREFIX=/nope", "WINEPREFIXES=/nope"},
		// unreadable environ (another user's process)
		"106": nil,
		// empty value: a set-but-empty WINEPREFIX is not a prefix
		"107": {"WINEPREFIX="},
	}, map[string]string{
		"101": "/app/bin/wineserver\x00-p\x00",
		"102": "Z:\\home\\u\\WoW.exe\x00",
	})

	got, err := ScanProc(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("found %d wine processes, want 3: %+v", len(got), got)
	}
	byPID := map[int]Running{}
	for _, r := range got {
		byPID[r.PID] = r
	}
	if r := byPID[101]; r.Prefix != CleanPrefix(bottleA) || r.Command != "/app/bin/wineserver" {
		t.Errorf("pid 101 = %+v, want prefix %s and the wineserver command", r, bottleA)
	}
	if r := byPID[103]; r.Prefix != CleanPrefix(bottleB) {
		t.Errorf("pid 103 prefix = %q, want %q", r.Prefix, bottleB)
	}
	// cmdline missing falls back to something printable, not an empty column.
	if r := byPID[103]; r.Command == "" {
		t.Error("pid 103 has an empty command label")
	}

	procs := GroupByPrefix(got)
	if len(procs) != 2 {
		t.Fatalf("grouped into %d prefixes, want 2: %+v", len(procs), procs)
	}
	for _, p := range procs {
		switch p.Prefix {
		case CleanPrefix(bottleA):
			if len(p.Processes) != 2 {
				t.Errorf("bottle A has %d processes, want 2", len(p.Processes))
			}
		case CleanPrefix(bottleB):
			if len(p.Processes) != 1 {
				t.Errorf("bottle B has %d processes, want 1", len(p.Processes))
			}
		default:
			t.Errorf("unexpected prefix %q", p.Prefix)
		}
	}
}

func TestScanProcEmptyAndMissingRoot(t *testing.T) {
	got, err := ScanProc(fakeProc(t, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("found %d processes in an empty procfs", len(got))
	}
	if _, err := ScanProc(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("scanning a missing root should fail")
	}
}

func TestEnvValueExactName(t *testing.T) {
	blob := "A=1\x00WINEPREFIX=/p\x00B=2\x00"
	if v, ok := envValue(blob, "WINEPREFIX"); !ok || v != "/p" {
		t.Errorf("got %q %v", v, ok)
	}
	if _, ok := envValue(blob, "WINE"); ok {
		t.Error("prefix of a name matched")
	}
	if _, ok := envValue("", "WINEPREFIX"); ok {
		t.Error("empty environ matched")
	}
}

func TestSamePrefix(t *testing.T) {
	dir := t.TempDir()
	if !SamePrefix(dir, dir+"/") {
		t.Error("trailing slash should not matter")
	}
	if !SamePrefix(dir, filepath.Join(dir, "x", "..")) {
		t.Error("unclean paths should compare equal")
	}
	if SamePrefix(dir, filepath.Join(dir, "other")) {
		t.Error("different directories compared equal")
	}
}
