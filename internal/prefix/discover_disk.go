package prefix

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// wineInfra are the processes every booted Wine prefix runs. None of them is
// what you want to see in a prefix list, so Game() looks past them.
var wineInfra = map[string]bool{
	"wineserver": true, "wineboot.exe": true, "services.exe": true,
	"winedevice.exe": true, "plugplay.exe": true, "explorer.exe": true,
	"rpcss.exe": true, "svchost.exe": true, "start.exe": true,
	"conhost.exe": true, "tabtip.exe": true, "winemenubuilder.exe": true,
	"wine": true, "wine64": true, "wine-preloader": true, "wineserver-preloader": true,
}

// Game names the process worth showing for a prefix — the game or launcher
// rather than the half-dozen Wine services beside it. Empty when the prefix has
// no live process, or only infrastructure.
//
// A bare count ("10 proc") is accurate and useless: ten is what a booted prefix
// always looks like, so it says nothing about which prefix this is.
func (p Proc) Game() string {
	for _, r := range p.Processes {
		base := filepath.Base(strings.ReplaceAll(r.Command, `\`, "/"))
		if base == "" || base == "." || wineInfra[strings.ToLower(base)] {
			continue
		}
		return base
	}
	return ""
}

// Running reports whether this prefix has any live process.
func (p Proc) Running() bool { return len(p.Processes) > 0 }

// diskRoots are the places a Wine prefix is normally found: a prefix named by
// the environment, the default one, and the per-bottle directories of both the
// Flatpak and native installs of Bottles.
func diskRoots() (exact []string, globs []string) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, nil
	}
	if wp := os.Getenv("WINEPREFIX"); wp != "" {
		exact = append(exact, wp)
	}
	exact = append(exact, filepath.Join(home, ".wine"))
	data := os.Getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	globs = append(globs,
		filepath.Join(home, ".var", "app", "com.usebottles.bottles", "data", "bottles", "bottles", "*"),
		filepath.Join(data, "bottles", "bottles", "*"),
	)
	return exact, globs
}

// canonical resolves a prefix path to one spelling so the same directory
// reached two ways compares equal.
//
// filepath.Abs is not enough: on an ostree system /home is a symlink to
// /var/home, so a prefix found on disk under /home and the same prefix read
// out of a process environment as /var/home are different strings for one
// directory — and the merge listed every running prefix twice.
func canonical(dir string) string {
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if res, err := filepath.EvalSymlinks(dir); err == nil {
		return res
	}
	return dir // a path that cannot be resolved is still worth listing
}

// IsPrefix reports whether a directory looks like a Wine prefix. system.reg is
// the test rather than drive_c: an empty drive_c can exist without a prefix
// having been created, while system.reg only appears once wineboot has run.
func IsPrefix(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "system.reg"))
	return err == nil && st.Mode().IsRegular()
}

// DiscoverOnDisk lists Wine prefixes found on the filesystem, whether or not
// anything is running in them.
//
// Process-based discovery can only see a prefix while the game is up, which
// makes the one thing you need the list for — installing the agent before
// starting the game — impossible without typing a path. This is the cold list.
func DiscoverOnDisk() ([]Proc, error) {
	exact, globs := diskRoots()
	seen := map[string]bool{}
	var out []Proc
	add := func(dir string) {
		c := canonical(dir)
		if seen[c] || !IsPrefix(c) {
			return
		}
		seen[c] = true
		out = append(out, Proc{Prefix: c})
	}
	for _, d := range exact {
		add(d)
	}
	for _, g := range globs {
		matches, err := filepath.Glob(g)
		if err != nil {
			continue
		}
		for _, m := range matches {
			add(m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Prefix < out[j].Prefix })
	return out, nil
}

// DiscoverAll merges the running prefixes with the ones found on disk. A
// running prefix wins, so it keeps its process list (and therefore its Game
// name and the Busy route install.go picks).
func DiscoverAll() ([]Proc, error) {
	disk, _ := DiscoverOnDisk()
	// On a platform without /proc, Discover reports ErrNoProc. That is not a
	// failure here: the disk list is the whole answer.
	live, err := Discover()
	if err != nil && !errors.Is(err, ErrNoProc) {
		return disk, err
	}
	byPath := map[string]int{}
	var out []Proc
	for _, p := range live {
		p.Prefix = canonical(p.Prefix)
		if _, dup := byPath[p.Prefix]; dup {
			continue // two processes of one prefix already grouped by Discover
		}
		byPath[p.Prefix] = len(out)
		out = append(out, p)
	}
	for _, p := range disk {
		if _, ok := byPath[p.Prefix]; ok {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Running() != out[j].Running() {
			return out[i].Running() // running first: those are the ones you act on
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out, nil
}

// WineCmdFromProcesses derives the wine binary that belongs to a running
// prefix, from the prefix's own wineserver process. Empty when it cannot be
// worked out.
//
// This is what makes installing into a RUNNING prefix possible without asking
// anyone to supply a wine command. Editing system.reg while a wineserver holds
// it would be discarded on shutdown, so the only safe route on a live prefix is
// `wine reg add` — and the wine that matches the prefix is sitting in its own
// process list. Bottles launches wineserver by absolute path, e.g.
//
//	.../runners/soda-11.0-10/lib/wine/../../bin/wineserver
//
// which cleans to .../runners/soda-11.0-10/bin/wineserver; its sibling `wine`
// is the binary wanted. Guessing a runner from the prefix name would break the
// moment a prefix used a different one; reading it from the process cannot.
func WineCmdFromProcesses(p Proc) string {
	for _, r := range p.Processes {
		cmd := strings.TrimSpace(r.Command)
		if cmd == "" || !strings.HasPrefix(cmd, "/") {
			continue // a Windows-side path (C:\...) names nothing on this side
		}
		clean := filepath.Clean(cmd)
		if filepath.Base(clean) != "wineserver" {
			continue
		}
		wine := filepath.Join(filepath.Dir(clean), "wine")
		st, err := os.Stat(wine)
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue // present but not runnable: not a usable answer
		}
		return wine
	}
	return ""
}
