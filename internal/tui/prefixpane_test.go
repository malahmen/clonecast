package tui

// Pure reducer tests for PrefixPane: messages are fabricated directly rather
// than produced by prefix.Discover/Install/Uninstall/Describe (which touch
// /proc and a real Wine prefix), so these run anywhere and exercise the
// state machine the same way tui.go's Update does.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/malahmen/clonecast/internal/prefix"
)

func keyRune(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func TestPrefixPaneTarget(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})
	if got := p.Target(); got != "" {
		t.Fatalf("empty pane Target() = %q, want \"\"", got)
	}

	p, _ = p.Update(prefixListMsg{procs: []prefix.Proc{
		{Prefix: "/bottles/a"},
		{Prefix: "/bottles/b"},
	}})
	if got := p.Target(); got != "/bottles/a" {
		t.Fatalf("Target() after list = %q, want /bottles/a (cursor 0)", got)
	}

	// A manual path takes precedence over the list selection.
	p.manual = "/manual/prefix"
	if got := p.Target(); got != "/manual/prefix" {
		t.Fatalf("Target() with manual set = %q, want /manual/prefix", got)
	}
}

func TestPrefixPaneCursorMoveRefreshesTarget(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})
	p, _ = p.Update(prefixListMsg{procs: []prefix.Proc{
		{Prefix: "/bottles/a"},
		{Prefix: "/bottles/b"},
	}})
	if p.target != "/bottles/a" {
		t.Fatalf("target after list = %q, want /bottles/a", p.target)
	}

	// Moving to a different row must re-describe (a non-nil cmd).
	p, cmd := p.Update(keyRune('j'))
	if cmd == nil {
		t.Fatal("moving cursor to a new prefix returned a nil cmd, want a describe command")
	}
	if p.target != "/bottles/b" {
		t.Fatalf("target after j = %q, want /bottles/b", p.target)
	}

	// Moving again with no row change (already at the bottom) must not
	// re-describe: no wasted work, and no risk of clobbering a fresher result.
	p, cmd = p.Update(keyRune('j'))
	if cmd != nil {
		t.Fatal("moving past the last row returned a non-nil cmd, want nil (target unchanged)")
	}
}

func TestPrefixPaneStaleStatusIgnored(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})
	p, _ = p.Update(prefixListMsg{procs: []prefix.Proc{{Prefix: "/bottles/a"}, {Prefix: "/bottles/b"}}})
	p, _ = p.Update(keyRune('j')) // target is now /bottles/b

	// A slow describe() for the prefix we've since moved off must not land.
	p, _ = p.Update(prefixStatusMsg{prefix: "/bottles/a", st: &prefix.Status{Prefix: "/bottles/a"}})
	if p.descOK {
		t.Fatal("a status result for a stale target was applied")
	}

	p, _ = p.Update(prefixStatusMsg{prefix: "/bottles/b", st: &prefix.Status{Prefix: "/bottles/b", Exe: "agent.exe"}})
	if !p.descOK || p.desc == nil || p.desc.Exe != "agent.exe" {
		t.Fatalf("status result for the current target was not applied: descOK=%v desc=%+v", p.descOK, p.desc)
	}
}

func TestPrefixPaneManualPathEditing(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})

	p, cmd := p.Update(keyRune('p'))
	if !p.Editing() {
		t.Fatal("'p' did not enter editing mode")
	}
	if cmd == nil {
		t.Fatal("entering path edit mode should return the cursor-blink cmd")
	}

	for _, r := range "/home/me/bottle" {
		p, _ = p.Update(keyRune(r))
	}
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if p.Editing() {
		t.Fatal("enter did not leave editing mode")
	}
	if p.manual != "/home/me/bottle" {
		t.Fatalf("manual path = %q, want /home/me/bottle", p.manual)
	}

	// Esc cancels without touching the previously committed manual path.
	p, _ = p.Update(keyRune('p'))
	for _, r := range "garbage" {
		p, _ = p.Update(keyRune(r))
	}
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if p.Editing() {
		t.Fatal("esc did not leave editing mode")
	}
	if p.manual != "/home/me/bottle" {
		t.Fatalf("manual path after esc = %q, want unchanged /home/me/bottle", p.manual)
	}
}

func TestPrefixPaneEditingCapturesGlobalLookingKeys(t *testing.T) {
	// A path can legitimately contain 'q', 'j', 'k' etc.; while editing, those
	// must go to the text input, not be reinterpreted as pane shortcuts. This
	// is enforced by tui.go checking Editing() before treating a key as
	// global, but the pane's own Update must still accept them as text.
	p := NewPrefixPane(PrefixPaneConfig{})
	p, _ = p.Update(keyRune('p'))
	p, _ = p.Update(keyRune('q'))
	if !p.Editing() {
		t.Fatal("'q' while editing exited edit mode instead of being typed")
	}
}

func TestPrefixPaneInstallUninstallGuards(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})

	// No target selected: both must no-op, not panic.
	if _, cmd := p.Update(keyRune('i')); cmd != nil {
		t.Fatal("install with no target returned a non-nil cmd")
	}
	if _, cmd := p.Update(keyRune('u')); cmd != nil {
		t.Fatal("uninstall with no target returned a non-nil cmd")
	}

	p.manual = "/bottles/a"
	p, cmd := p.Update(keyRune('i'))
	if cmd == nil {
		t.Fatal("install with a target returned a nil cmd")
	}
	if !p.busy {
		t.Fatal("install did not mark the pane busy")
	}

	// Busy: a second install/uninstall must be ignored until the first
	// finishes (prefixInstallMsg/prefixUninstallMsg clears busy).
	if _, cmd := p.Update(keyRune('u')); cmd != nil {
		t.Fatal("uninstall while busy returned a non-nil cmd")
	}

	p, _ = p.Update(prefixInstallMsg{prefix: "/bottles/a", res: &prefix.Result{Route: prefix.RouteFile}})
	if p.busy {
		t.Fatal("busy was not cleared after prefixInstallMsg")
	}
	if p.err {
		t.Fatal("a successful install left the pane in error state")
	}
}

func TestPrefixPaneInstallErrorReported(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})
	p.manual = "/bottles/a"
	p, _ = p.Update(keyRune('i'))
	p, cmd := p.Update(prefixInstallMsg{prefix: "/bottles/a", err: errors.New("boom")})
	if !p.err {
		t.Fatal("a failed install did not set the error state")
	}
	if cmd != nil {
		t.Fatal("a failed install should not queue a describe (nothing to re-check)")
	}
}

// A booted prefix whose wine command cannot be derived must be refused before
// an install is attempted, with the remedy in the message — the error path only
// ever showed its first line, so the lines saying what to do were lost.
func TestPrefixPaneRefusesRunningPrefixWhenWineUnknown(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})
	p, _ = p.Update(prefixListMsg{procs: []prefix.Proc{
		{Prefix: "/bottles/live", Processes: []prefix.Running{{PID: 1, Command: "WoW.exe"}}},
		{Prefix: "/bottles/idle"},
	}})

	p, cmd := p.Update(keyRune('i'))
	if cmd != nil {
		t.Fatal("install on a running prefix returned a command, want a refusal with no work done")
	}
	if !p.err {
		t.Fatal("refusal was not marked as an error")
	}
	if !strings.Contains(strings.ToLower(p.status), "close the game") {
		t.Fatalf("status = %q, want it to say to close the game", p.status)
	}
	if !strings.Contains(p.status, "--agent-wine") {
		t.Fatalf("status = %q, want it to offer --agent-wine as the alternative", p.status)
	}
	if p.busy {
		t.Fatal("pane was left busy after a refusal")
	}
}

// The idle row must still install, or the guard would block everything.
func TestPrefixPaneInstallsIdlePrefix(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{})
	p, _ = p.Update(prefixListMsg{procs: []prefix.Proc{
		{Prefix: "/bottles/idle"},
		{Prefix: "/bottles/live", Processes: []prefix.Running{{PID: 1, Command: "WoW.exe"}}},
	}})
	p, cmd := p.Update(keyRune('i'))
	if cmd == nil {
		t.Fatal("install on an idle prefix returned no command")
	}
	if p.err {
		t.Fatalf("install on an idle prefix set an error: %q", p.status)
	}
}

// With a wine command the reg route works on a live prefix, so the guard must
// not stand in the way.
func TestPrefixPaneWineCmdAllowsRunningPrefix(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{WineCmd: "wine"})
	p, _ = p.Update(prefixListMsg{procs: []prefix.Proc{
		{Prefix: "/bottles/live", Processes: []prefix.Running{{PID: 1, Command: "WoW.exe"}}},
	}})
	p, cmd := p.Update(keyRune('i'))
	if cmd == nil {
		t.Fatal("install on a running prefix with a wine command was refused, want it attempted")
	}
	if p.err {
		t.Fatalf("unexpected error: %q", p.status)
	}
}

func TestWrapTo(t *testing.T) {
	if got := wrapTo("", 20); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty message = %q, want one empty line so the layout does not jump", got)
	}
	// Honours newlines already present, and wraps long paragraphs.
	got := wrapTo("one two three\nfour", 9)
	want := []string{"one two", "three", "four"}
	if len(got) != len(want) {
		t.Fatalf("wrapTo lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("wrapTo = %q, want %q", got, want)
		}
	}
	// No line may exceed the width when the words themselves fit.
	for _, l := range wrapTo("the prefix is running so its system reg is held open", 16) {
		if len(l) > 16 {
			t.Fatalf("line %q exceeds width 16", l)
		}
	}
}

// A booted prefix whose own wineserver names a usable wine must be installed
// into, not refused: editing system.reg is unsafe while it runs, but `reg add`
// through that wine is not, so closing the game is no longer required.
func TestPrefixPaneInstallsRunningPrefixViaDerivedWine(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "runner", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"wine", "wineserver"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	p := NewPrefixPane(PrefixPaneConfig{})
	p, _ = p.Update(prefixListMsg{procs: []prefix.Proc{{
		Prefix: "/bottles/live",
		// The lib/wine/../../bin form Bottles actually launches, so the test
		// covers the path cleaning too.
		Processes: []prefix.Running{
			{PID: 1, Command: `C:\windows\system32\services.exe`},
			{PID: 2, Command: filepath.Join(dir, "runner", "lib", "wine", "..", "..", "bin", "wineserver")},
		},
	}}})

	p, cmd := p.Update(keyRune('i'))
	if cmd == nil {
		t.Fatalf("install was refused, want it attempted through the derived wine: %q", p.status)
	}
	if p.err {
		t.Fatalf("unexpected error: %q", p.status)
	}
	if !p.installRunning {
		t.Fatal("installRunning not set, so the result would not mention restarting the game")
	}
}
