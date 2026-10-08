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
//  2. the saved KEYBOARD, if those devices still exist
//  3. exactly one discovered keyboard — no question to ask
//  4. otherwise ask, and save the answer
//
// Step 4 needs a terminal. Without one it returns the list and the error
// instead, because silently grabbing everything is what produced doubled keys.
func resolveKeyboards(flagSpec string) ([]string, error) {
	if paths := splitPaths(flagSpec); len(paths) > 0 {
		return paths, nil
	}

	found, err := listCandidates()
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return nil, fmt.Errorf("no keyboard found under /dev/input (are you in the input group?)")
	}

	// A saved choice is only honoured while it still exists: unplug the
	// keyboard and the stale path would otherwise fail every start with a
	// message about the wrong thing.
	if saved := splitPaths(configGet(kbdKey)); len(saved) > 0 {
		ok := true
		for _, s := range saved {
			if !candidateExists(found, s) {
				ok = false
				break
			}
		}
		if ok {
			return saved, nil
		}
		fmt.Fprintf(os.Stderr, "saved keyboard %q is gone; asking again\n", strings.Join(saved, ","))
	}

	if len(found) == 1 {
		return []string{found[0].Path}, nil
	}

	if !isTerminal(os.Stdin) {
		var b strings.Builder
		for _, c := range found {
			fmt.Fprintf(&b, "\n  %s  %-28s %s", c.Path, c.State, c.Name)
		}
		return nil, fmt.Errorf("several keyboards found and no terminal to ask on; pass --keyboard with one of:%s", b.String())
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

	if err := configSet(kbdKey, strings.Join(chosen, ",")); err != nil {
		// Not fatal: this run can still proceed, it just will not be
		// remembered, and saying so is better than failing the start.
		fmt.Fprintf(os.Stderr, "could not save your choice (%v); pass --keyboard next time\n", err)
	} else {
		fmt.Printf("saved to %s — change it with --keyboard, or edit that file\n", configPath())
	}
	return chosen, nil
}

func candidateExists(found []candidate, path string) bool {
	for _, c := range found {
		if c.Path == path {
			return true
		}
	}
	return false
}
