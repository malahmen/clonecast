package tui

// Pure reducer tests for PrefixPane: messages are fabricated directly rather
// than produced by prefix.Discover/Install/Uninstall/Describe (which touch
// /proc and a real Wine prefix), so these run anywhere and exercise the
// state machine the same way tui.go's Update does.

import (
	"errors"
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
