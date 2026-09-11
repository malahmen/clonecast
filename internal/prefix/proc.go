package prefix

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Running is one live process that belongs to a Wine prefix, as found by
// reading its environment block.
type Running struct {
	PID     int
	Prefix  string // WINEPREFIX, cleaned
	Command string // first argv element, or the comm name if argv is unreadable
}

// Proc is a Wine prefix that currently has at least one process alive. The
// prefix owns its agent's lifetime, so "running" here means "this prefix has
// booted": its system.reg is held in memory by a wineserver and must not be
// edited from the host.
type Proc struct {
	Prefix    string
	Processes []Running
}

// ScanProc lists the Wine processes visible under a procfs root by reading
// WINEPREFIX out of /proc/<pid>/environ.
//
// Why this works across launchers: environ is readable for the user's own
// processes, and a Flatpak app's children (Bottles' wine, wineserver and the
// game) are ordinary host processes in the host PID namespace, so the host
// sees them. Processes we may not read (other users, or gone mid-scan) are
// skipped silently; only a failure to read the root is an error.
//
// The root is a parameter so this is testable against a fake procfs, and so
// the function itself stays portable — the Linux-only part is merely the
// decision to pass "/proc" (see discover_linux.go).
func ScanProc(root string) ([]Running, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Running
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // not a process directory
		}
		blob, err := os.ReadFile(filepath.Join(root, e.Name(), "environ"))
		if err != nil {
			continue // not ours to read, or the process just exited
		}
		p, ok := envValue(string(blob), "WINEPREFIX")
		if !ok || p == "" {
			continue
		}
		out = append(out, Running{
			PID:     pid,
			Prefix:  CleanPrefix(p),
			Command: procCommand(root, e.Name()),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Prefix != out[j].Prefix {
			return out[i].Prefix < out[j].Prefix
		}
		return out[i].PID < out[j].PID
	})
	return out, nil
}

// GroupByPrefix collapses a process list into one entry per prefix.
func GroupByPrefix(rs []Running) []Proc {
	var out []Proc
	byPrefix := map[string]int{}
	for _, r := range rs {
		i, ok := byPrefix[r.Prefix]
		if !ok {
			byPrefix[r.Prefix] = len(out)
			out = append(out, Proc{Prefix: r.Prefix, Processes: []Running{r}})
			continue
		}
		out[i].Processes = append(out[i].Processes, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out
}

// envValue pulls one variable out of a NUL-separated /proc environ blob.
// Matching is exact on the name, so WINEPREFIXES or MY_WINEPREFIX do not hit.
func envValue(blob, name string) (string, bool) {
	for _, kv := range strings.Split(blob, "\x00") {
		if eq := strings.IndexByte(kv, '='); eq >= 0 && kv[:eq] == name {
			return kv[eq+1:], true
		}
	}
	return "", false
}

// procCommand is a human-readable label for a process: argv[0] if cmdline can
// be read, else the comm name, else "?".
func procCommand(root, pid string) string {
	if b, err := os.ReadFile(filepath.Join(root, pid, "cmdline")); err == nil {
		if argv0, _, _ := strings.Cut(string(b), "\x00"); argv0 != "" {
			return argv0
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, pid, "comm")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return "?"
}

// CleanPrefix normalises a prefix path for comparison: absolute where
// possible, no trailing slash, symlinks resolved when they resolve.
func CleanPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// SamePrefix reports whether two prefix paths name the same directory.
func SamePrefix(a, b string) bool {
	return CleanPrefix(a) == CleanPrefix(b)
}
