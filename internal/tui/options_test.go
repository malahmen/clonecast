package tui

import (
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/platform/mock"
	"github.com/malahmen/clonecast/internal/prefix"
)

// Kamino item 45: every option reachable from the TUI. These were flags only:
// --master-by-agent and --xsend (Settings), and --agent-title, --agent-wine,
// --agent-exe with agent install's --route, --name, --assume-idle and
// --keep-exe (Prefixes).

type fakeXsend struct {
	spec string
}

func (f *fakeXsend) Names() []string   { return []string{"dance"} }
func (f *fakeXsend) Current() string   { return "dance" }
func (f *fakeXsend) Use(string) error  { return nil }
func (f *fakeXsend) XsendSpec() string { return f.spec }
func (f *fakeXsend) SetXsendSpec(s string) error {
	if s == "bad" {
		return errors.New("bad map")
	}
	f.spec = s
	return nil
}

func settingsModel(b Backends) Model {
	wm := mock.NewWM()
	engine := broadcast.New(mock.NewSource(time.Hour), &mock.Injector{}, wm, broadcast.DefaultConfig())
	m := New(engine, wm, b, nil, PrefixPaneConfig{}, nil)
	m.pane = paneSettings
	return m
}

func TestSettingsSwitchMasterByAgent(t *testing.T) {
	m := settingsModel(nil)
	m.setCursor = setMasterAgent
	m.changeSetting()
	if !m.engine.Config().MasterByAgent {
		t.Fatal("space on the row did not turn master by agent on")
	}
	m.changeSetting()
	if m.engine.Config().MasterByAgent {
		t.Fatal("a second space did not turn it off")
	}
}

func TestSettingsEditTheXsendMap(t *testing.T) {
	b := &fakeXsend{}
	m := settingsModel(b)
	m.setCursor = setXsend
	m.changeSetting()
	if m.editing != editXsend {
		t.Fatalf("editing = %v, want the xsend map", m.editing)
	}
	m.input.SetValue("Almerinda=World of Warcraft")
	if err := m.applyEdit(); err != nil {
		t.Fatal(err)
	}
	if b.spec != "Almerinda=World of Warcraft" {
		t.Errorf("map = %q", b.spec)
	}
	m.editing = editXsend
	m.input.SetValue("bad")
	if err := m.applyEdit(); err == nil {
		t.Error("a bad map was accepted")
	}
}

func typeInto(p PrefixPane, s string) PrefixPane {
	for _, r := range s {
		p, _ = p.Update(keyRune(r))
	}
	p, _ = p.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return p
}

func TestPrefixPaneInstallOptions(t *testing.T) {
	p := NewPrefixPane(PrefixPaneConfig{Token: "s3cret"})
	p, _ = p.Update(keyRune('t'))
	p = typeInto(p, "Game")
	p, _ = p.Update(keyRune('w'))
	p = typeInto(p, "umu-run")
	p, _ = p.Update(keyRune('x'))
	p = typeInto(p, "/tmp/agent.exe")
	p, _ = p.Update(keyRune('n'))
	p = typeInto(p, "my-agent")
	p, _ = p.Update(keyRune('o')) // auto -> file
	p, _ = p.Update(keyRune('a'))
	p, _ = p.Update(keyRune('K'))

	got := p.config("/pfx", p.cfg.WineCmd)
	want := prefix.Config{Prefix: "/pfx", ExePath: "/tmp/agent.exe", Title: "Game", WineCmd: "umu-run",
		Token: "s3cret", Route: prefix.RouteFile, ValueName: "my-agent", AssumeIdle: true, KeepExe: true}
	if got != want {
		t.Errorf("config =\n %+v\nwant\n %+v", got, want)
	}
	if p.Editing() {
		t.Error("still editing after enter")
	}
}

func TestPrefixPaneRouteCycles(t *testing.T) {
	r := ""
	var seen []string
	for i := 0; i < 3; i++ {
		r = nextRoute(r)
		seen = append(seen, r)
	}
	if seen[0] != prefix.RouteFile || seen[1] != prefix.RouteReg || seen[2] != prefix.RouteAuto {
		t.Errorf("routes = %v", seen)
	}
}

// The Prefixes pane's options line has dimmed parts, and truncate cut it by
// runes, counting the colour codes: the line came out a column short and the
// pane's border with it (2026-10-20).
func TestTruncateKeepsTheWidthOfAStyledLine(t *testing.T) {
	// Raw codes: a test has no terminal, so lipgloss would render no colour.
	dim := func(s string) string { return "\x1b[38;5;241m" + s + "\x1b[0m" }
	line := "route=auto  title=" + dim("game window") + "  name=" + dim("clonecast-agent")
	for _, w := range []int{10, 20, 25, 30, 40} {
		got := truncate(line, w)
		if lipgloss.Width(got) != w {
			t.Errorf("truncate to %d: width %d (%q)", w, lipgloss.Width(got), got)
		}
	}
}
