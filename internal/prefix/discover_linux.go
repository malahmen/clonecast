//go:build linux

package prefix

// Discover lists the Wine prefixes that currently have processes alive, so the
// CLI and the TUI can offer them instead of asking the user to type a path
// (research.md §C2, "Discovering prefixes to install into").
func Discover() ([]Proc, error) {
	rs, err := ScanProc("/proc")
	if err != nil {
		return nil, err
	}
	return GroupByPrefix(rs), nil
}

// Busy reports whether a prefix has booted — i.e. whether a wineserver is
// holding its registry. A busy prefix rewrites system.reg from memory when it
// shuts down, so an offline edit of that file would be silently thrown away;
// install.go uses this to pick the route.
//
// Evidence is any live process whose WINEPREFIX is this prefix (wineserver
// itself, the game, the launcher's wine wrapper — all carry it).
func Busy(path string) (bool, []Running, error) {
	rs, err := ScanProc("/proc")
	if err != nil {
		return false, nil, err
	}
	var hits []Running
	for _, r := range rs {
		if SamePrefix(r.Prefix, path) {
			hits = append(hits, r)
		}
	}
	return len(hits) > 0, hits, nil
}

// PrefixOfPID reports which Wine prefix a host pid runs in, by reading its
// environment. See PrefixOfPIDIn for why this is the right join for pairing a
// window to an agent.
func PrefixOfPID(pid int) (string, error) {
	return PrefixOfPIDIn("/proc", pid)
}
