// Package tui is the Bubble Tea front end. It owns no broadcasting logic: it
// reads windows from the WindowManager, pushes target/filter/enabled changes
// into the Engine, and renders the Engine's notices.
//
// Layout (lazygit style, three panels):
//
//	┌ Targets ────────────┐┌ Keys ───────────────┐
//	│ [x] Game — 1        ││ mode: explicit      │
//	│ [ ] Steam           ││ A B C               │
//	└─────────────────────┘└─────────────────────┘
//	┌ Log ─────────────────────────────────────────┐
//	│ 12:00:01 A down -> 2 target(s)               │
//	└──────────────────────────────────────────────┘
//	 tab: pane  space: toggle  b: broadcast  q: quit
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
)

type pane int

const (
	paneTargets pane = iota
	paneKeys
	paneLog
	paneCount
)

type (
	noticeMsg  broadcast.Notice
	windowsMsg struct {
		wins []broadcast.Window
		err  error
	}
	refreshTick struct{}
)

// Model is the Bubble Tea model.
type Model struct {
	engine *broadcast.Engine
	wm     broadcast.WindowManager

	windows  []broadcast.Window
	selected map[broadcast.WindowID]bool
	cursor   int
	pane     pane

	keyInput textinput.Model
	editing  bool
	keyErr   string

	log    []string
	logVP  viewport.Model
	width  int
	height int
}

// New builds the model. The engine must already be running.
func New(engine *broadcast.Engine, wm broadcast.WindowManager) Model {
	ti := textinput.New()
	ti.Placeholder = `keys, e.g. "a b c f1" or "all"`
	ti.CharLimit = 200
	return Model{
		engine:   engine,
		wm:       wm,
		selected: map[broadcast.WindowID]bool{},
		keyInput: ti,
		logVP:    viewport.New(0, 0),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refreshWindows(), m.waitNotice(), tickRefresh())
}

func (m Model) refreshWindows() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		wins, err := m.wm.List(ctx)
		return windowsMsg{wins: wins, err: err}
	}
}

func (m Model) waitNotice() tea.Cmd {
	ch := m.engine.Notices()
	return func() tea.Msg {
		n, ok := <-ch
		if !ok {
			return nil
		}
		return noticeMsg(n)
	}
}

func tickRefresh() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return refreshTick{} })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case windowsMsg:
		if msg.err != nil {
			m.appendLog(fmt.Sprintf("list windows: %v", msg.err), true)
			return m, nil
		}
		m.windows = msg.wins
		if m.cursor >= len(m.windows) {
			m.cursor = max(0, len(m.windows)-1)
		}
		m.pushTargets()
		return m, nil

	case refreshTick:
		return m, tea.Batch(m.refreshWindows(), tickRefresh())

	case noticeMsg:
		m.appendLog(msg.Text, msg.Err)
		return m, m.waitNotice()

	case tea.KeyMsg:
		if m.editing {
			return m.updateEditing(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m Model) updateEditing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.editing = false
		m.keyErr = ""
		return m, nil
	case tea.KeyEnter:
		set, err := keys.ParseSet(m.keyInput.Value())
		if err != nil {
			m.keyErr = err.Error()
			return m, nil
		}
		m.engine.SetFilter(set)
		m.appendLog("keys: "+set.String(), false)
		m.editing = false
		m.keyErr = ""
		return m, nil
	}
	var cmd tea.Cmd
	m.keyInput, cmd = m.keyInput.Update(msg)
	return m, cmd
}

func (m Model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.pane = (m.pane + 1) % paneCount
	case "shift+tab":
		m.pane = (m.pane + paneCount - 1) % paneCount
	case "b":
		m.engine.SetEnabled(!m.engine.Enabled())
	case "r":
		return m, m.refreshWindows()
	case "j", "down":
		if m.pane == paneTargets && m.cursor < len(m.windows)-1 {
			m.cursor++
		} else if m.pane == paneLog {
			m.logVP.LineDown(1)
		}
	case "k", "up":
		if m.pane == paneTargets && m.cursor > 0 {
			m.cursor--
		} else if m.pane == paneLog {
			m.logVP.LineUp(1)
		}
	case " ", "enter":
		if m.pane == paneTargets && len(m.windows) > 0 {
			id := m.windows[m.cursor].ID
			m.selected[id] = !m.selected[id]
			m.pushTargets()
		}
	case "a":
		if m.pane == paneKeys {
			if m.engine.Filter().IsAll() {
				m.engine.SetFilter(keys.Set{})
			} else {
				m.engine.SetFilter(keys.All())
			}
			m.appendLog("keys: "+m.engine.Filter().String(), false)
		}
	case "e":
		if m.pane == paneKeys {
			m.editing = true
			m.keyInput.SetValue(m.engine.Filter().String())
			m.keyInput.Focus()
			return m, textinput.Blink
		}
	}
	return m, nil
}

// pushTargets sends the checked windows, in list order, to the engine. Windows
// that vanished since the last refresh are dropped from the selection.
func (m *Model) pushTargets() {
	ids := make([]broadcast.WindowID, 0, len(m.selected))
	present := map[broadcast.WindowID]bool{}
	for _, w := range m.windows {
		present[w.ID] = true
		if m.selected[w.ID] {
			ids = append(ids, w.ID)
		}
	}
	for id := range m.selected {
		if !present[id] {
			delete(m.selected, id)
		}
	}
	m.engine.SetTargets(ids)
}

func (m *Model) appendLog(text string, isErr bool) {
	line := time.Now().Format("15:04:05") + " " + text
	if isErr {
		line = errStyle.Render(line)
	}
	m.log = append(m.log, line)
	if len(m.log) > 500 {
		m.log = m.log[len(m.log)-500:]
	}
	m.logVP.SetContent(strings.Join(m.log, "\n"))
	m.logVP.GotoBottom()
}

// ---- rendering --------------------------------------------------------------

var (
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240"))
	activeStyle = borderStyle.BorderForeground(lipgloss.Color("62"))
	titleStyle  = lipgloss.NewStyle().Bold(true)
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	onStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	offStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203"))
	errStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	cursorStyle = lipgloss.NewStyle().Reverse(true)
)

func (m *Model) layout() {
	// two rows of panels: top row split in two, bottom row is the log
	topH := max(5, (m.height-4)/2)
	logH := max(3, m.height-topH-4)
	m.logVP.Width = max(10, m.width-4)
	m.logVP.Height = max(1, logH-2)
}

func (m Model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	topH := max(5, (m.height-4)/2)
	halfW := max(20, (m.width-4)/2)

	targets := m.panel("Targets", m.viewTargets(halfW-2, topH-2), halfW, topH, m.pane == paneTargets)
	keysP := m.panel("Keys", m.viewKeys(halfW-2), m.width-halfW-4, topH, m.pane == paneKeys)
	top := lipgloss.JoinHorizontal(lipgloss.Top, targets, keysP)

	logP := m.panel("Log", m.logVP.View(), m.width-2, m.logVP.Height+2, m.pane == paneLog)

	return lipgloss.JoinVertical(lipgloss.Left, m.header(), top, logP, m.footer())
}

func (m Model) header() string {
	state := offStyle.Render("BROADCAST OFF")
	if m.engine.Enabled() {
		state = onStyle.Render("BROADCAST ON")
	}
	return fmt.Sprintf(" %s  %s  %s",
		titleStyle.Render("clonecast"),
		state,
		dimStyle.Render(fmt.Sprintf("targets: %d  keys: %s", len(m.engine.Targets()), m.engine.Filter())),
	)
}

func (m Model) footer() string {
	if m.editing {
		return dimStyle.Render(" enter: apply  esc: cancel")
	}
	switch m.pane {
	case paneKeys:
		return dimStyle.Render(" tab: pane  a: all/none  e: edit set  b: broadcast on/off  r: refresh  q: quit")
	case paneLog:
		return dimStyle.Render(" tab: pane  j/k: scroll  b: broadcast on/off  q: quit")
	}
	return dimStyle.Render(" tab: pane  j/k: move  space: toggle target  b: broadcast on/off  r: refresh  q: quit")
}

func (m Model) panel(title, body string, w, h int, active bool) string {
	st := borderStyle
	if active {
		st = activeStyle
	}
	t := titleStyle.Render(title)
	return st.Width(w - 2).Height(h - 2).Render(t + "\n" + body)
}

func (m Model) viewTargets(w, h int) string {
	if len(m.windows) == 0 {
		return dimStyle.Render("no windows (r to refresh)")
	}
	var b strings.Builder
	rows := max(1, h-1)
	start := 0
	if m.cursor >= rows {
		start = m.cursor - rows + 1
	}
	for i := start; i < len(m.windows) && i < start+rows; i++ {
		win := m.windows[i]
		mark := "[ ]"
		if m.selected[win.ID] {
			mark = "[x]"
		}
		focus := " "
		if win.Focused {
			focus = "*"
		}
		line := fmt.Sprintf("%s%s %s", mark, focus, truncate(win.Title, w-8))
		line += dimStyle.Render("  " + truncate(win.Class, max(0, w-lipgloss.Width(line)-2)))
		if i == m.cursor && m.pane == paneTargets {
			line = cursorStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m Model) viewKeys(w int) string {
	f := m.engine.Filter()
	var b strings.Builder
	if f.IsAll() {
		b.WriteString("mode: " + onStyle.Render("all keys") + "\n")
	} else {
		b.WriteString("mode: explicit\n")
	}
	b.WriteString(wrap(f.String(), w) + "\n")
	if m.editing {
		b.WriteString("\n" + m.keyInput.View())
		if m.keyErr != "" {
			b.WriteString("\n" + errStyle.Render(m.keyErr))
		}
	} else {
		b.WriteString("\n" + dimStyle.Render("a: toggle all   e: edit"))
	}
	return b.String()
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

func wrap(s string, w int) string {
	if w <= 0 {
		return s
	}
	words := strings.Fields(s)
	var lines []string
	cur := ""
	for _, word := range words {
		if cur == "" {
			cur = word
		} else if len(cur)+1+len(word) > w {
			lines = append(lines, cur)
			cur = word
		} else {
			cur += " " + word
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n")
}
