package tui

// PrefixPane is the TUI face of `clonecast agent install` (REFERENCE.md 7.11):
// it lists the Wine prefixes that currently have processes alive and installs
// the in-bottle agent into the highlighted one, so setting up a new game never
// requires quitting to a shell or typing a prefix path.
//
// It is written as a standalone Bubble Tea sub-model with no dependency on
// Model or on the shared styles in tui.go, so it can be dropped into the main
// layout without restructuring it. Wiring (a follow-up, three edits in tui.go):
//
//  1. a pane constant, e.g. panePrefixes, in the pane iota;
//  2. a field `prefixes PrefixPane` on Model, built in New with
//     NewPrefixPane(PrefixPaneConfig{ExePath: ..., Port: ...});
//  3. in Update, forward messages when m.pane == panePrefixes:
//     `pp, cmd := m.prefixes.Update(msg); m.prefixes = pp; return m, cmd`
//     (and m.prefixes.Init() batched into Model.Init), plus
//     `m.panel("Prefixes", m.prefixes.View(), w, h, m.pane == panePrefixes)`
//     in View.
//
// Everything the pane needs beyond that is its own.

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/malahmen/clonecast/internal/prefix"
)

// PrefixPaneConfig is what the pane needs to perform an install: the same
// values the `agent install` flags carry.
type PrefixPaneConfig struct {
	ExePath string // built clonecast-agent.exe ("" = look next to the binary)
	Port    int    // agent listen port inside the prefix (0 = agent default)
	Title   string // target window title ("" = agent default)
	WineCmd string // wine command for prefixes that are already booted
}

// PrefixPane lists running Wine prefixes and installs the agent into one.
type PrefixPane struct {
	cfg    PrefixPaneConfig
	procs  []prefix.Proc
	cursor int
	status string
	err    bool
	busy   bool
}

type (
	prefixListMsg struct {
		procs []prefix.Proc
		err   error
	}
	prefixInstallMsg struct {
		prefix string
		res    *prefix.Result
		err    error
	}
)

// NewPrefixPane builds the pane. Call Init (or Refresh) to populate it.
func NewPrefixPane(cfg PrefixPaneConfig) PrefixPane {
	return PrefixPane{cfg: cfg, status: "press r to scan for running Wine prefixes"}
}

func (p PrefixPane) Init() tea.Cmd { return p.Refresh() }

// Refresh rescans /proc for prefixes with live processes.
func (p PrefixPane) Refresh() tea.Cmd {
	return func() tea.Msg {
		procs, err := prefix.Discover()
		return prefixListMsg{procs: procs, err: err}
	}
}

// Selected is the highlighted prefix path, or "" when the list is empty.
func (p PrefixPane) Selected() string {
	if p.cursor < 0 || p.cursor >= len(p.procs) {
		return ""
	}
	return p.procs[p.cursor].Prefix
}

// install returns a command that installs the agent into path. The prefix is
// running (that is how it was discovered), so this only succeeds offline-style
// when a wine command was configured; otherwise the error explains what to do,
// exactly as the CLI does.
func (p PrefixPane) install(path string) tea.Cmd {
	cfg := prefix.Config{
		Prefix:  path,
		ExePath: p.cfg.ExePath,
		Port:    p.cfg.Port,
		Title:   p.cfg.Title,
		WineCmd: p.cfg.WineCmd,
	}
	return func() tea.Msg {
		res, err := prefix.Install(cfg)
		return prefixInstallMsg{prefix: path, res: res, err: err}
	}
}

// Update handles the pane's keys and its own async messages.
func (p PrefixPane) Update(msg tea.Msg) (PrefixPane, tea.Cmd) {
	switch msg := msg.(type) {
	case prefixListMsg:
		if msg.err != nil {
			p.status, p.err = "scan: "+msg.err.Error(), true
			return p, nil
		}
		p.procs, p.err = msg.procs, false
		if p.cursor >= len(p.procs) {
			p.cursor = max(0, len(p.procs)-1)
		}
		p.status = fmt.Sprintf("%d running prefix(es)", len(p.procs))
		return p, nil

	case prefixInstallMsg:
		p.busy = false
		if msg.err != nil {
			p.status, p.err = "install: "+firstLine(msg.err.Error()), true
			return p, nil
		}
		p.err = false
		p.status = fmt.Sprintf("installed (%s route) — starts on this prefix's next boot", msg.res.Route)
		return p, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "r":
			p.status, p.err = "scanning…", false
			return p, p.Refresh()
		case "j", "down":
			if p.cursor < len(p.procs)-1 {
				p.cursor++
			}
		case "k", "up":
			if p.cursor > 0 {
				p.cursor--
			}
		case "i", "enter":
			sel := p.Selected()
			if sel == "" || p.busy {
				return p, nil
			}
			p.busy = true
			p.status, p.err = "installing into "+sel+"…", false
			return p, p.install(sel)
		}
	}
	return p, nil
}

var (
	prefixPaneDim = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	prefixPaneErr = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	prefixPaneSel = lipgloss.NewStyle().Reverse(true)
)

// View renders the list. w and h are the inner panel size.
func (p PrefixPane) View(w, h int) string {
	var b strings.Builder
	if len(p.procs) == 0 {
		b.WriteString(prefixPaneDim.Render("no running Wine prefixes (start the game, then r)") + "\n")
	}
	rows := max(1, h-2)
	start := 0
	if p.cursor >= rows {
		start = p.cursor - rows + 1
	}
	for i := start; i < len(p.procs) && i < start+rows; i++ {
		pr := p.procs[i]
		line := fmt.Sprintf("%s  %s", shortPath(pr.Prefix, max(8, w-14)),
			prefixPaneDim.Render(fmt.Sprintf("%d proc", len(pr.Processes))))
		if i == p.cursor {
			line = prefixPaneSel.Render(line)
		}
		b.WriteString(line + "\n")
	}
	st := p.status
	if p.err {
		st = prefixPaneErr.Render(st)
	}
	b.WriteString("\n" + st + "\n")
	b.WriteString(prefixPaneDim.Render("i: install agent   r: rescan"))
	return b.String()
}

// shortPath keeps the tail of a long prefix path, which is the part that
// names the bottle.
func shortPath(s string, n int) string {
	if n <= 1 || len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
