package prefix

import (
	"fmt"
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
// WINEPREFIX out of /proc/<pid>/environ. A Wine process with no WINEPREFIX is
// listed under the default prefix, $HOME/.wine from its own environment.
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
			// Missing these made Busy call a live ~/.wine idle, and the
			// offline registry edit was overwritten when its wineserver
			// exited (Kamino 202).
			if p, ok = defaultPrefix(root, e.Name(), string(blob)); !ok {
				continue
			}
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

// PrefixOfPIDIn reads one process's WINEPREFIX out of a procfs root, or
// places a Wine process with none in the default $HOME/.wine as ScanProc does. It is the
// join that lets clonecast pair a KWin window with a registered agent
// (REFERENCE.md 4.17): KWin reports a window's *host* pid (it asks the X
// server through XRes, which derives it from the socket peer credentials, so
// it is the real host pid even for a Flatpak-sandboxed Wine client, unlike
// _NET_WM_PID), and the prefix that pid runs in is exactly what the agent
// announced in its hello.
//
// Not-a-Wine-process and cannot-read are both reported as errors rather than
// an empty string, because the caller has a fallback (matching the announced
// window title) and needs to know it must use it.
//
// The root is a parameter for the same reason ScanProc's is: the function is
// portable and testable against a fake procfs, and only the decision to pass
// "/proc" is Linux-only (see discover_linux.go, discover_other.go).
func PrefixOfPIDIn(root string, pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("pid %d is not a process", pid)
	}
	name := strconv.Itoa(pid)
	blob, err := os.ReadFile(filepath.Join(root, name, "environ"))
	if err != nil {
		return "", err
	}
	p, ok := envValue(string(blob), "WINEPREFIX")
	if !ok || strings.TrimSpace(p) == "" {
		// A plain-`wine` game sets no WINEPREFIX and runs in $HOME/.wine,
		// which is also what its agent announces (it derives the prefix
		// from its own path). Without the default here such a window fell
		// to the title match only (Kamino 276).
		if p, ok = defaultPrefix(root, name, string(blob)); !ok {
			return "", fmt.Errorf("pid %d has no WINEPREFIX", pid)
		}
	}
	return CleanPrefix(p), nil
}

// defaultPrefix is the prefix of a process with no WINEPREFIX: plain `wine`
// runs in $HOME/.wine, and so does everything it starts, wineserver and the
// game included. Only a Wine process (isWineProcess) with HOME set is placed
// there; anything else has no prefix. blob is the process's environ.
func defaultPrefix(root, pid, blob string) (string, bool) {
	home, _ := envValue(blob, "HOME")
	if home == "" || !isWineProcess(root, pid) {
		return "", false
	}
	return filepath.Join(home, ".wine"), true
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

// wineNames are the host programs of a Wine prefix, by base name.
var wineNames = map[string]bool{
	"wine": true, "wine64": true, "wineserver": true,
	"wine-preloader": true, "wine64-preloader": true,
	"wine64-preloade": true, // comm is cut at 15 bytes
}

// isWineProcess reports whether a process is Wine's: a wine/wineserver binary,
// or a Windows program, whose argv[0] Wine sets to its Windows path
// (`C:\windows\system32\explorer.exe`). It is asked only of processes
// with no WINEPREFIX, to place them in the default prefix.
func isWineProcess(root, pid string) bool {
	var names []string
	if b, err := os.ReadFile(filepath.Join(root, pid, "cmdline")); err == nil {
		argv0, _, _ := strings.Cut(string(b), "\x00")
		names = append(names, argv0)
	}
	if b, err := os.ReadFile(filepath.Join(root, pid, "comm")); err == nil {
		names = append(names, strings.TrimSpace(string(b)))
	}
	for _, n := range names {
		base := n[strings.LastIndexAny(n, `/\`)+1:]
		if wineNames[base] || (len(base) > 4 && strings.EqualFold(base[len(base)-4:], ".exe")) {
			return true
		}
	}
	return false
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
