package tui

// PrefixPane is the TUI face of `clonecast agent install/uninstall/status`
// (REFERENCE.md 7.11/4.16): it lists the Wine prefixes that currently have
// processes alive, lets you point at any other prefix by path, and installs,
// removes or describes the in-bottle agent in the selected one — so setting
// up (or tearing down) a game's agent never requires quitting to a shell.
//
// It is written as a standalone Bubble Tea sub-model with no dependency on
// Model or on the shared styles in tui.go, so it drops into the main layout
// without restructuring it (see tui.go's panePrefixes wiring).
//
// Keys: r rescan, p set/clear a manual prefix path, i install, u uninstall,
// j/k move. Status for the current target (list selection or manual path) is
// fetched automatically whenever it changes, so switching prefixes always
// shows what's actually installed there without a separate keypress.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/malahmen/clonecast/internal/prefix"
)

// PrefixPaneConfig is what the pane needs to perform an install: the same
// values the `agent install` flags carry.
type PrefixPaneConfig struct {
	ExePath string // built clonecast-agent.exe ("" = look next to the binary)
	Port    int    // clonecast port the installed agent dials (0 = agent default)
	Title   string // target window title ("" = agent default)
	WineCmd string // wine command for prefixes that are already booted
}

// PrefixPane lists running Wine prefixes and installs/removes/describes the
// agent in one — the list selection or a manually entered path, whichever
// was set last.
type PrefixPane struct {
	cfg   PrefixPaneConfig
	procs []prefix.Proc
	// installRunning records whether the install in flight targeted a booted
	// prefix, so the result can say that a restart is what starts the agent.
	installRunning bool
	cursor         int
	status         string
	err            bool
	busy           bool

	manual      string // manually entered prefix path; "" defers to the list
	editingPath bool
	pathInput   textinput.Model

	target  string         // last resolved Target(), to detect a selection change
	desc    *prefix.Status // last status fetch for target
	descOK  bool           // whether desc/descErr came back for the current target
	descErr string
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
	prefixUninstallMsg struct {
		prefix string
		res    *prefix.Result
		err    error
	}
	prefixStatusMsg struct {
		prefix string
		st     *prefix.Status
		err    error
	}
)

// NewPrefixPane builds the pane. Call Init (or Refresh) to populate it.
func NewPrefixPane(cfg PrefixPaneConfig) PrefixPane {
	ti := textinput.New()
	ti.CharLimit = 400
	ti.Placeholder = "/path/to/prefix (empty clears the manual path)"
	return PrefixPane{cfg: cfg, status: "press r to scan for running Wine prefixes", pathInput: ti}
}

func (p PrefixPane) Init() tea.Cmd { return p.Refresh() }

// Editing reports whether the pane is capturing keystrokes for its own path
// input, so the parent model knows not to steal them for global shortcuts.
func (p PrefixPane) Editing() bool { return p.editingPath }

// Refresh relists prefixes: the ones with live processes, plus the ones found
// on disk. Process-only discovery could not see a prefix until its game was
// running, which made installing the agent BEFORE starting the game — the
// normal order — impossible without typing a path.
func (p PrefixPane) Refresh() tea.Cmd {
	return func() tea.Msg {
		procs, err := prefix.DiscoverAll()
		return prefixListMsg{procs: procs, err: err}
	}
}

// listSelected is the highlighted prefix path from the running-process list,
// or "" when the list is empty.
func (p PrefixPane) listSelected() string {
	if p.cursor < 0 || p.cursor >= len(p.procs) {
		return ""
	}
	return p.procs[p.cursor].Prefix
}

// Target is the prefix install/uninstall/status act on: a manually entered
// path takes precedence, so pointing at an idle prefix (nothing in the
// running list) still works.
func (p PrefixPane) Target() string {
	if p.manual != "" {
		return p.manual
	}
	return p.listSelected()
}

// install runs the install for one prefix. wineCmd overrides the configured
// one so a RUNNING prefix can be installed into through its own wine, derived
// from its processes, instead of being refused.
func (p PrefixPane) install(path, wineCmd string) tea.Cmd {
	if wineCmd == "" {
		wineCmd = p.cfg.WineCmd
	}
	cfg := prefix.Config{
		Prefix:  path,
		ExePath: p.cfg.ExePath,
		Port:    p.cfg.Port,
		Title:   p.cfg.Title,
		WineCmd: wineCmd,
	}
	return func() tea.Msg {
		res, err := prefix.Install(cfg)
		return prefixInstallMsg{prefix: path, res: res, err: err}
	}
}

func (p PrefixPane) uninstall(path string) tea.Cmd {
	cfg := prefix.Config{Prefix: path, WineCmd: p.cfg.WineCmd}
	return func() tea.Msg {
		res, err := prefix.Uninstall(cfg)
		return prefixUninstallMsg{prefix: path, res: res, err: err}
	}
}

func (p PrefixPane) describe(path string) tea.Cmd {
	return func() tea.Msg {
		st, err := prefix.Describe(prefix.Config{Prefix: path})
		return prefixStatusMsg{prefix: path, st: st, err: err}
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
		live := 0
		for _, pr := range p.procs {
			if pr.Running() {
				live++
			}
		}
		p.status = fmt.Sprintf("%d prefix(es), %d running", len(p.procs), live)
		return p, p.refreshTargetIfChanged()

	case prefixInstallMsg:
		p.busy = false
		if msg.err != nil {
			p.status, p.err = "install: "+msg.err.Error(), true
			return p, nil
		}
		p.err = false
		if p.installRunning {
			p.status = fmt.Sprintf("installed (%s route) through the prefix's own wine — "+
				"RESTART THE GAME to start the agent; it does not attach to an already-running prefix",
				msg.res.Route)
		} else {
			p.status = fmt.Sprintf("installed (%s route) — starts on this prefix's next boot", msg.res.Route)
		}
		return p, p.describe(msg.prefix)

	case prefixUninstallMsg:
		p.busy = false
		if msg.err != nil {
			p.status, p.err = "uninstall: "+msg.err.Error(), true
			return p, nil
		}
		p.err = false
		p.status = "uninstalled"
		return p, p.describe(msg.prefix)

	case prefixStatusMsg:
		// A slow describe() for a target the user has since moved off must
		// not clobber a fresher one — compare against the current target.
		if msg.prefix != p.Target() {
			return p, nil
		}
		p.descOK = true
		if msg.err != nil {
			p.desc, p.descErr = nil, msg.err.Error()
		} else {
			p.desc, p.descErr = msg.st, ""
		}
		return p, nil

	case tea.KeyMsg:
		if p.editingPath {
			switch msg.Type {
			case tea.KeyEsc:
				p.editingPath = false
				return p, nil
			case tea.KeyEnter:
				p.manual = strings.TrimSpace(p.pathInput.Value())
				p.editingPath = false
				return p, p.refreshTargetIfChanged()
			}
			var cmd tea.Cmd
			p.pathInput, cmd = p.pathInput.Update(msg)
			return p, cmd
		}

		switch msg.String() {
		case "r":
			p.status, p.err = "scanning…", false
			return p, p.Refresh()
		case "j", "down":
			if p.manual == "" && p.cursor < len(p.procs)-1 {
				p.cursor++
			}
			return p, p.refreshTargetIfChanged()
		case "k", "up":
			if p.manual == "" && p.cursor > 0 {
				p.cursor--
			}
			return p, p.refreshTargetIfChanged()
		case "p":
			p.editingPath = true
			p.pathInput.SetValue(p.manual)
			p.pathInput.CursorEnd()
			p.pathInput.Focus()
			return p, textinput.Blink
		case "i", "enter":
			t := p.Target()
			if t == "" || p.busy {
				return p, nil
			}
			// A running prefix cannot have its system.reg edited — a live
			// wineserver holds it and rewrites it on shutdown, so the change
			// would be silently discarded — but it CAN be registered through
			// its own wine. That wine is derivable from the prefix's own
			// processes, so a running prefix no longer has to be closed first.
			wineCmd := ""
			running := p.runningTarget(t)
			if running && p.cfg.WineCmd == "" {
				wineCmd = prefix.WineCmdFromProcesses(p.procFor(t))
				if wineCmd == "" {
					p.status, p.err = "this prefix is running and its wine command could not be worked out "+
						"from its processes, so the registry cannot be written safely. Close the game and "+
						"press i again, or start clonecast with --agent-wine.", true
					return p, nil
				}
			}
			p.busy = true
			p.installRunning = running
			p.status, p.err = "installing into "+t+"…", false
			return p, p.install(t, wineCmd)
		case "u":
			t := p.Target()
			if t == "" || p.busy {
				return p, nil
			}
			p.busy = true
			p.status, p.err = "uninstalling from "+t+"…", false
			return p, p.uninstall(t)
		}
	}
	return p, nil
}

// procFor is the listed entry for a prefix, so its processes can be inspected.
// A zero Proc for an unlisted path: WineCmdFromProcesses then finds nothing,
// which is the right answer for a prefix we know nothing about.
func (p PrefixPane) procFor(path string) prefix.Proc {
	for _, pr := range p.procs {
		if pr.Prefix == path {
			return pr
		}
	}
	return prefix.Proc{}
}

// runningTarget reports whether the given prefix is one of the listed ones
// that currently has live processes. A manually typed path that is not in the
// list is not assumed to be running: prefix.Install still checks, so the worst
// case is the error we used to show.
func (p PrefixPane) runningTarget(path string) bool {
	for _, pr := range p.procs {
		if pr.Prefix == path {
			return pr.Running()
		}
	}
	return false
}

// refreshTargetIfChanged re-fetches status when the resolved target (list
// cursor or manual path) differs from the last one it was fetched for —
// called after anything that could move it, so the shown status never lags
// behind what's actually selected.
func (p *PrefixPane) refreshTargetIfChanged() tea.Cmd {
	t := p.Target()
	if t == p.target {
		return nil
	}
	p.target = t
	p.desc, p.descErr, p.descOK = nil, "", false
	if t == "" {
		return nil
	}
	return p.describe(t)
}

var (
	prefixPaneDim = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	prefixPaneErr = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	prefixPaneSel = lipgloss.NewStyle().Reverse(true)
	prefixPaneOK  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
)

// View renders the list, the manual-path row and the current target's
// status. w and h are the inner panel size.
func (p PrefixPane) View(w, h int) string {
	var b strings.Builder

	if p.editingPath {
		b.WriteString("prefix: " + p.pathInput.View() + "\n")
	} else if p.manual != "" {
		b.WriteString("prefix: " + prefixPaneOK.Render(shortPath(p.manual, max(8, w-8))) + dimHint(" (p: change)") + "\n")
	} else {
		b.WriteString(prefixPaneDim.Render("p: point at a prefix by path") + "\n")
	}

	if len(p.procs) == 0 {
		b.WriteString(prefixPaneDim.Render("no Wine prefixes found (p: point at one by path)") + "\n")
	}
	// The status can be several lines — an install refusal explains what to do
	// instead — so the list gives up the room rather than the message being
	// cut to its first line, which is how the remedy got lost before.
	statusLines := wrapTo(p.status, w)
	rows := max(1, h-6-(len(statusLines)-1))
	start := 0
	if p.cursor >= rows {
		start = p.cursor - rows + 1
	}
	var line string
	for i := start; i < len(p.procs) && i < start+rows; i++ {
		pr := p.procs[i]
		// The game's exe, not a process count: every booted prefix runs the
		// same handful of Wine services, so "10 proc" is true of all of them
		// and tells you nothing about which prefix you are looking at.
		note := "not running"
		if g := pr.Game(); g != "" {
			note = g
		} else if pr.Running() {
			note = fmt.Sprintf("%d proc", len(pr.Processes))
		}
		line = fmt.Sprintf("%s  %s", shortPath(pr.Prefix, max(8, w-18)),
			prefixPaneDim.Render(note))
		if p.manual == "" && i == p.cursor {
			line = prefixPaneSel.Render(line)
		}
		b.WriteString(line + "\n")
	}

	b.WriteString(p.viewStatus(w))

	b.WriteString("\n")
	for _, line := range statusLines {
		if p.err {
			line = prefixPaneErr.Render(line)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(prefixPaneDim.Render(truncate("i: install  u: uninstall  p: path  r: rescan", w)))
	return b.String()
}

// viewStatus renders what's installed in the current target, refreshed
// automatically as the target changes (see refreshTargetIfChanged).
func (p PrefixPane) viewStatus(w int) string {
	t := p.Target()
	if t == "" {
		return ""
	}
	if !p.descOK {
		return prefixPaneDim.Render("checking "+shortPath(t, max(8, w-9))+"…") + "\n"
	}
	if p.descErr != "" {
		return prefixPaneErr.Render(truncate("status: "+firstLine(p.descErr), w)) + "\n"
	}
	st := p.desc
	agentLine := "agent: " + prefixPaneErr.Render("not installed")
	if st.Exe != "" {
		agentLine = "agent: " + prefixPaneOK.Render("installed") + prefixPaneDim.Render(" ("+st.ExeMod.Format("2006-01-02 15:04")+")")
	}
	autostart := "autostart: " + prefixPaneErr.Render("none")
	if st.Value != "" {
		autostart = "autostart: " + prefixPaneOK.Render(truncate(st.Value, max(4, w-11)))
	}
	running := prefixPaneDim.Render("running: unknown")
	switch {
	case st.BusyKnown && st.Busy:
		running = "running: " + prefixPaneOK.Render(fmt.Sprintf("yes (%d proc)", len(st.Processes)))
	case st.BusyKnown:
		running = prefixPaneDim.Render("running: no")
	}
	return truncate(agentLine, w) + "\n" + truncate(autostart, w) + "\n" + running + "\n"
}

func dimHint(s string) string { return prefixPaneDim.Render(s) }

// shortPath keeps the tail of a long prefix path, which is the part that
// names the bottle.
func shortPath(s string, n int) string {
	if n <= 1 || len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

// wrapTo breaks a message into lines that fit a width, honouring the newlines
// already in it. Returns one empty line for an empty message so the layout
// does not jump as the status changes.
func wrapTo(s string, w int) []string {
	if w < 8 {
		w = 8
	}
	if strings.TrimSpace(s) == "" {
		return []string{""}
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		words := strings.Fields(para)
		if len(words) == 0 {
			out = append(out, "")
			continue
		}
		line := words[0]
		for _, word := range words[1:] {
			if len(line)+1+len(word) > w {
				out = append(out, line)
				line = word
				continue
			}
			line += " " + word
		}
		out = append(out, line)
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
