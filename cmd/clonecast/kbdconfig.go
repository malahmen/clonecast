package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Keyboard selection, and the only setting clonecast persists.
//
// Discovery is a blunt test — EV_KEY with KEY_A and KEY_ENTER — and on a real
// desk it matches more than the keyboard you type on: both event nodes of one
// keyboard, a gaming mouse's macro interfaces, and (with keyd or
// input-remapper) both the physical keyboard the remapper grabbed and the
// virtual one it publishes. Grabbing several nodes of one device makes every
// press arrive twice, which reads as keys doing nothing at all.
//
// So when the answer is ambiguous, ask once and remember it, rather than
// guessing differently on every run.

const kbdKey = "KEYBOARD"

// candidate is one device discovery found, with whether it can be grabbed.
// listCandidates is platform-specific; everything else here is not.
type candidate struct {
	Path  string
	State string // "free", or "BUSY (...)" with the reason
	Name  string
	// ID names the device the same way across reboots and replugs, and is
	// what gets saved: the /dev/input/by-id link when it has one, otherwise
	// "name:" and its name. A /dev/input/eventN path is not an identity —
	// the numbers are handed out in probe order — and saving one meant a
	// later run grabbing whichever device had that number by then.
	ID string
}

// legacyEventPath reports whether a saved value is a bare /dev/input/eventN,
// which clonecast saved before IDs and which cannot be trusted to still be
// the same device.
func legacyEventPath(s string) bool { return strings.HasPrefix(s, "/dev/input/event") }

// resolveIDs turns saved IDs into device paths. ok=false says why the saved
// choice cannot be used, so the caller asks again instead of grabbing the
// wrong device.
func resolveIDs(found []candidate, ids []string) (paths []string, why string) {
	for _, id := range ids {
		if legacyEventPath(id) {
			return nil, fmt.Sprintf("saved keyboard %q is a device number, which can change between boots", id)
		}
		var match []candidate
		for _, c := range found {
			if c.ID == id {
				match = append(match, c)
			}
		}
		switch len(match) {
		case 0:
			return nil, fmt.Sprintf("saved keyboard %q is gone", id)
		case 1:
			paths = append(paths, match[0].Path)
		default:
			return nil, fmt.Sprintf("saved keyboard %q now matches %d devices", id, len(match))
		}
	}
	return paths, ""
}

// idsFor is the IDs to save for these paths. A path discovery does not know
// is saved as given, which is no worse than before.
func idsFor(found []candidate, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		id := p
		for _, c := range found {
			if c.Path == p && c.ID != "" {
				id = c.ID
			}
		}
		out = append(out, id)
	}
	return out
}

// saveKeyboards remembers a choice by ID.
func saveKeyboards(found []candidate, paths []string) error {
	return configSet(kbdKey, strings.Join(idsFor(found, paths), ","))
}

func configPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "clonecast", "clonecast.conf")
}

// configGet reads one key. A missing or unreadable file is simply no value:
// the config is a convenience, never a requirement.
func configGet(key string) string {
	p := configPath()
	if p == "" {
		return ""
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	var val string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			val = strings.Trim(strings.TrimSpace(v), `"`) // last wins
		}
	}
	return val
}

// configSet rewrites one key, preserving every other line and any comments.
func configSet(key, val string) error {
	p := configPath()
	if p == "" {
		return fmt.Errorf("no config directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var out []string
	replaced := false
	if b, err := os.ReadFile(p); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && strings.TrimSpace(k) == key {
				if replaced {
					continue // drop earlier duplicates
				}
				out = append(out, key+"="+val)
				replaced = true
				continue
			}
			out = append(out, line)
		}
	}
	if !replaced {
		out = append(out, key+"="+val)
	}
	body := strings.Join(out, "\n")
	body = strings.TrimRight(body, "\n") + "\n"
	return os.WriteFile(p, []byte(body), 0o644)
}

// resolveKeyboards decides which devices to capture, in this order:
//
//  1. --keyboard, which always wins and is never saved
//  2. the saved KEYBOARD, if it still names exactly the devices it did
//  3. exactly one discovered keyboard — no question to ask
//  4. otherwise ask — by keypress first, then from a numbered list — and
//     save the answer by ID
//
// Step 4 needs a terminal. Without one it returns the list and the error
// instead, because silently grabbing everything is what produced doubled keys.
func resolveKeyboards(flagSpec string) ([]string, error) {
	spec := splitPaths(flagSpec)
	var found []candidate
	if len(spec) > 0 {
		// A path is used as given, a by-id link resolved to the node it
		// points at (the Keyboards pane compares by node); anything else is
		// an ID, as saved.
		if !containsID(spec) {
			return canonicalPaths(spec), nil
		}
		var err error
		if found, err = listCandidates(); err != nil {
			return nil, err
		}
		var paths []string
		for _, s := range spec {
			if strings.HasPrefix(s, "/") {
				paths = append(paths, canonicalPaths([]string{s})...)
				continue
			}
			p, why := resolveIDs(found, []string{s})
			if why != "" {
				return nil, fmt.Errorf("--keyboard: %s", why)
			}
			paths = append(paths, p...)
		}
		return paths, nil
	}

	found, err := listCandidates()
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no keyboard found under /dev/input (are you in the input group?)")
	}

	// A saved choice is honoured only while it still names exactly the
	// devices it did. Unplugged, renumbered or ambiguous, it is asked again:
	// grabbing a different device is worse than one question.
	if saved := splitPaths(configGet(kbdKey)); len(saved) > 0 {
		paths, why := resolveIDs(found, saved)
		if why == "" {
			return paths, nil
		}
		fmt.Fprintf(os.Stderr, "%s; asking again\n", why)
	}

	if len(found) == 1 {
		return []string{found[0].Path}, nil
	}

	if !isTerminal(os.Stdin) {
		var b strings.Builder
		for _, c := range found {
			fmt.Fprintf(&b, "\n  %s  %-28s %s", c.ID, c.State, c.Name)
		}
		return nil, fmt.Errorf("several keyboards found and no terminal to ask on; pass --keyboard with one of:%s", b.String())
	}

	if c, ok := askByKeypress(found); ok {
		chosen := []string{c.Path}
		if err := saveKeyboards(found, chosen); err != nil {
			fmt.Fprintf(os.Stderr, "could not save your choice (%v); pass --keyboard next time\n", err)
		} else {
			fmt.Printf("saved to %s as %s — change it with --keyboard, or edit that file\n", configPath(), c.ID)
		}
		return chosen, nil
	}

	fmt.Println("Which keyboard should clonecast capture?")
	fmt.Println("(the one you type on. With keyd or input-remapper, pick the VIRTUAL device it publishes,")
	fmt.Println(" not the physical one it has grabbed — that one shows as BUSY.)")
	fmt.Println()
	for i, c := range found {
		fmt.Printf("  %d) %-20s %-28s %s\n", i+1, c.Path, c.State, c.Name)
	}
	fmt.Println()
	fmt.Print("number (or several, comma-separated): ")

	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("reading your choice: %w", err)
	}
	var chosen []string
	for _, f := range strings.Split(strings.TrimSpace(line), ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		n, cerr := strconv.Atoi(f)
		if cerr != nil || n < 1 || n > len(found) {
			return nil, fmt.Errorf("%q is not one of the numbers listed", f)
		}
		chosen = append(chosen, found[n-1].Path)
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("nothing chosen")
	}

	if err := saveKeyboards(found, chosen); err != nil {
		// Not fatal: this run can still proceed, it just will not be
		// remembered, and saying so is better than failing the start.
		fmt.Fprintf(os.Stderr, "could not save your choice (%v); pass --keyboard next time\n", err)
	} else {
		fmt.Printf("saved to %s — change it with --keyboard, or edit that file\n", configPath())
	}
	return chosen, nil
}

// askByKeypress asks for one keypress and returns the device it came from.
// ok=false means fall back to the list: nothing free to listen on, no key in
// time, or a platform without it.
func askByKeypress(found []candidate) (candidate, bool) {
	var free []candidate
	for _, c := range found {
		if c.State == "free" {
			free = append(free, c)
		}
	}
	if len(free) == 0 {
		return candidate{}, false
	}
	fmt.Println("Which keyboard should clonecast capture? Press Shift on the keyboard you type on.")
	fmt.Printf("(listening on %d devices for %s; with no key in that time you get a list to choose from)\n", len(free), keypressTimeout)
	i, err := waitForKeyPress(free)
	if err != nil {
		fmt.Printf("%v — choosing from a list instead.\n\n", err)
		return candidate{}, false
	}
	fmt.Printf("got it: %s (%s)\n", free[i].Name, free[i].Path)
	return free[i], true
}

// canonicalPaths resolves symlinks (a /dev/input/by-id link) to the device
// node, leaving anything unresolvable as given for Open to report.
func canonicalPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = p
		if r, err := filepath.EvalSymlinks(p); err == nil {
			out[i] = r
		}
	}
	return out
}

// containsID reports whether a --keyboard value names any device by ID
// rather than by path.
func containsID(spec []string) bool {
	for _, s := range spec {
		if !strings.HasPrefix(s, "/") {
			return true
		}
	}
	return false
}
