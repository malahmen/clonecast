// Package tui is the Bubble Tea front end. It owns no broadcasting logic: it
// reads windows from the WindowManager, pushes target/filter/enabled/settings
// changes into the Engine, and renders the Engine's notices.
//
// Every command-line option that can change while clonecast runs is also a
// row in the Settings pane, so switching delivery backend, lifting the origin
// gate or retuning the settle delay never means restarting with other flags.
//
// Layout (lazygit style, four panels):
//
//	┌ Targets ──────────┐┌ Keys ─────────┐┌ Settings ─────┐
//	│ [x] M Game — 1    ││ mode: explicit││ delivery agent│
//	│ [x]   Game — 2    ││ A B C         ││ gate      ON  │
//	└───────────────────┘└───────────────┘└───────────────┘
//	┌ Log ─────────────────────────────────────────────────┐
//	│ 12:00:01 A down -> 2 target(s)                       │
//	└──────────────────────────────────────────────────────┘
//	 tab: pane  space: toggle  b: broadcast  q: quit
//
// "M" marks the master: the window that has focus right now. It receives keys
// through passthrough and is skipped by delivery, so the master is whatever
// you are looking at (REFERENCE.md 7.10) — tick every client and just switch
// windows. The marker is polled on its own fast ticker, not with the 5s
// window-list refresh, so it tracks focus live.
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

// Backends is the set of delivery backends the UI may switch between while
// the engine runs (see REFERENCE.md 7.9). cmd/clonecast implements it; a nil
// Backends just makes the delivery row read-only.
type Backends interface {
	// Names lists the usable backends, in cycle order.
	Names() []string
	// Current is the one in use.
	Current() string
	// Use switches to name, reporting why if it cannot.
	Use(name string) error
}

type pane int

const (
	paneTargets pane = iota
	paneKeys
	paneSettings
	paneLog
	paneCount
)

// Settings pane rows.
type setting int

const (
	setDelivery setting = iota
	setGate
	setSettle
	setToggle
	settingCount
)

// settlePresets is what space cycles through on the settle-delay row; `e`
// edits it to any duration.
var settlePresets = []time.Duration{
	0,
	10 * time.Millisecond,
	20 * time.Millisecond,
	30 * time.Millisecond,
	50 * time.Millisecond,
	75 * time.Millisecond,
	100 * time.Millisecond,
	150 * time.Millisecond,
}

// what the text input is currently editing
type editField int

const (
	editNone editField = iota
	editKeys
	editSettle
	editToggle
)

type (
	noticeMsg  broadcast.Notice
	windowsMsg struct {
		wins []broadcast.Window
		err  error
	}
	masterMsg struct {
		id  broadcast.WindowID
		err error
	}
	refreshTick struct{}
	masterTick  struct{}
)

// Model is the Bubble Tea model.
type Model struct {
	engine   *broadcast.Engine
	wm       broadcast.WindowManager
	backends Backends

	windows  []broadcast.Window
	selected map[broadcast.WindowID]bool
	cursor   int
	pane     pane

	// master is the focused window as of the last poll — the window that is
	// currently playing the keys live and is skipped by delivery.
	master    broadcast.WindowID
	masterErr bool

	setCursor setting

	input   textinput.Model
	editing editField
	editErr string

	log    []string
	logVP  viewport.Model
	width  int
	height int
}

// New builds the model. The engine must already be running. backends may be
// nil, in which case the delivery backend is shown but cannot be switched.
func New(engine *broadcast.Engine, wm broadcast.WindowManager, backends Backends) Model {
	ti := textinput.New()
	ti.CharLimit = 200
	return Model{
		engine:   engine,
		wm:       wm,
		backends: backends,
		selected: map[broadcast.WindowID]bool{},
		input:    ti,
		logVP:    viewport.New(0, 0),
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refreshWindows(), m.refreshMaster(), m.waitNotice(), tickRefresh(), tickMaster())
}

func (m Model) refreshWindows() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		wins, err := m.wm.List(ctx)
		return windowsMsg{wins: wins, err: err}
	}
}

// refreshMaster polls the focused window. It is one Active call (~1ms on
// KWin, REFERENCE.md 7.6), which is why it can run far more often than the
// full window list.
func (m Model) refreshMaster() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		id, err := m.wm.Active(ctx)
		return masterMsg{id: id, err: err}
	}
}

// focusWindow makes the highlighted window the master by activating it, so
// the user can hand the keyboard to another client without leaving the TUI.
func (m Model) focusWindow(id broadcast.WindowID) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.wm.Activate(ctx, id); err != nil {
			return masterMsg{err: err}
		}
		got, err := m.wm.Active(ctx)
		return masterMsg{id: got, err: err}
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

func tickMaster() tea.Cmd {
	return tea.Tick(700*time.Millisecond, func(time.Time) tea.Msg { return masterTick{} })
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

	case masterMsg:
		if msg.err != nil {
			if !m.masterErr {
				m.appendLog("active window: "+msg.err.Error(), true)
			}
			m.masterErr = true
			return m, nil
		}
		m.masterErr = false
		m.master = msg.id
		return m, nil

	case refreshTick:
		return m, tea.Batch(m.refreshWindows(), tickRefresh())

	case masterTick:
		return m, tea.Batch(m.refreshMaster(), tickMaster())

	case noticeMsg:
		m.appendLog(msg.Text, msg.Err)
		return m, m.waitNotice()

	case tea.KeyMsg:
		if m.editing != editNone {
			return m.updateEditing(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m Model) updateEditing(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		m.editing, m.editErr = editNone, ""
		return m, nil
	case tea.KeyEnter:
		if err := m.applyEdit(); err != nil {
			m.editErr = err.Error()
			return m, nil
		}
		m.editing, m.editErr = editNone, ""
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// applyEdit pushes the text input's value into the engine, per what is being
// edited. The engine notices the change itself, so only the key set (which
// has no notice) is logged here.
func (m *Model) applyEdit() error {
	value := strings.TrimSpace(m.input.Value())
	switch m.editing {
	case editKeys:
		set, err := keys.ParseSet(value)
		if err != nil {
			return err
		}
		m.engine.SetFilter(set)
		m.appendLog("keys: "+set.String(), false)
	case editSettle:
		d, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("settle delay: %w (try 30ms)", err)
		}
		if d < 0 {
			return fmt.Errorf("settle delay: must not be negative")
		}
		m.engine.SetSettleDelay(d)
	case editToggle:
		if value == "" || strings.EqualFold(value, "none") {
			m.engine.SetToggleKey(0)
			return nil
		}
		c, err := keys.Parse(value)
		if err != nil {
			return fmt.Errorf("toggle key: %w", err)
		}
		m.engine.SetToggleKey(c)
	}
	return nil
}

func (m *Model) startEdit(f editField, value, placeholder string) tea.Cmd {
	m.editing = f
	m.editErr = ""
	m.input.Placeholder = placeholder
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.Focus()
	return textinput.Blink
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
		return m, tea.Batch(m.refreshWindows(), m.refreshMaster())
	case "j", "down":
		switch m.pane {
		case paneTargets:
			if m.cursor < len(m.windows)-1 {
				m.cursor++
			}
		case paneSettings:
			if m.setCursor < settingCount-1 {
				m.setCursor++
			}
		case paneLog:
			m.logVP.LineDown(1)
		}
	case "k", "up":
		switch m.pane {
		case paneTargets:
			if m.cursor > 0 {
				m.cursor--
			}
		case paneSettings:
			if m.setCursor > 0 {
				m.setCursor--
			}
		case paneLog:
			m.logVP.LineUp(1)
		}
	case " ", "enter":
		switch m.pane {
		case paneTargets:
			if len(m.windows) > 0 {
				id := m.windows[m.cursor].ID
				m.selected[id] = !m.selected[id]
				m.pushTargets()
			}
		case paneSettings:
			return m, m.changeSetting()
		}
	case "a":
		switch m.pane {
		case paneTargets:
			// Tick everything (or clear it). With the master skipped per
			// broadcast, "all clients ticked" is now the normal setup —
			// REFERENCE.md 7.10 / research §C1.
			m.toggleAllTargets()
		case paneKeys:
			if m.engine.Filter().IsAll() {
				m.engine.SetFilter(keys.Set{})
			} else {
				m.engine.SetFilter(keys.All())
			}
			m.appendLog("keys: "+m.engine.Filter().String(), false)
		}
	case "f":
		if m.pane == paneTargets && len(m.windows) > 0 {
			return m, m.focusWindow(m.windows[m.cursor].ID)
		}
	case "g":
		m.engine.SetGateOrigin(!m.engine.GateOrigin())
	case "e":
		switch m.pane {
		case paneKeys:
			return m, m.startEdit(editKeys, m.engine.Filter().String(), `keys, e.g. "a b c f1" or "all"`)
		case paneSettings:
			switch m.setCursor {
			case setSettle:
				return m, m.startEdit(editSettle, m.engine.Config().SettleDelay.String(), "duration, e.g. 30ms")
			case setToggle:
				return m, m.startEdit(editToggle, toggleName(m.engine.Config().ToggleKey), "key name, or none")
			default:
				return m, m.changeSetting()
			}
		}
	}
	return m, nil
}

// changeSetting applies space/enter on the Settings pane: cycle the delivery
// backend, flip the gate, step the settle delay through its presets, or fall
// back to editing the toggle key.
func (m *Model) changeSetting() tea.Cmd {
	switch m.setCursor {
	case setDelivery:
		if m.backends == nil {
			m.appendLog("delivery backend cannot be switched in this build", true)
			return nil
		}
		names := m.backends.Names()
		if len(names) < 2 {
			m.appendLog("no other delivery backend available for this run (see --deliver)", true)
			return nil
		}
		cur := m.backends.Current()
		next := names[0]
		for i, n := range names {
			if n == cur {
				next = names[(i+1)%len(names)]
				break
			}
		}
		if err := m.backends.Use(next); err != nil {
			m.appendLog("delivery "+next+": "+err.Error(), true)
			return nil
		}
		m.appendLog("delivery backend: "+next, false)
	case setGate:
		m.engine.SetGateOrigin(!m.engine.GateOrigin())
	case setSettle:
		cur := m.engine.Config().SettleDelay
		next := settlePresets[0]
		for _, d := range settlePresets {
			if d > cur {
				next = d
				break
			}
		}
		m.engine.SetSettleDelay(next)
	case setToggle:
		return m.startEdit(editToggle, toggleName(m.engine.Config().ToggleKey), "key name, or none")
	}
	return nil
}

// toggleAllTargets ticks every listed window, or unticks them all if they
// already are ticked.
func (m *Model) toggleAllTargets() {
	all := len(m.windows) > 0
	for _, w := range m.windows {
		if !m.selected[w.ID] {
			all = false
			break
		}
	}
	for _, w := range m.windows {
		m.selected[w.ID] = !all
	}
	m.pushTargets()
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

// masterTicked reports whether the current master is one of the ticked
// targets — i.e. whether the origin gate would let a key broadcast.
func (m Model) masterTicked() bool {
	return m.master != "" && m.selected[m.master]
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
	masterStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
)

func (m *Model) layout() {
	// two rows of panels: top row split in three, bottom row is the log
	topH := max(5, (m.height-4)/2)
	logH := max(3, m.height-topH-4)
	m.logVP.Width = max(10, m.width-4)
	m.logVP.Height = max(1, logH-2)
}

// widths splits the top row into targets | keys | settings.
func (m Model) widths() (int, int, int) {
	avail := max(48, m.width-4)
	targetW := max(20, avail*2/5)
	keysW := max(14, (avail-targetW)/2)
	setW := max(16, avail-targetW-keysW)
	return targetW, keysW, setW
}

func (m Model) View() string {
	if m.width == 0 {
		return "starting…"
	}
	topH := max(5, (m.height-4)/2)
	targetW, keysW, setW := m.widths()

	targets := m.panel("Targets", m.viewTargets(targetW-2, topH-2), targetW, topH, m.pane == paneTargets)
	keysP := m.panel("Keys", m.viewKeys(keysW-2), keysW, topH, m.pane == paneKeys)
	setP := m.panel("Settings", m.viewSettings(setW-2), setW, topH, m.pane == paneSettings)
	top := lipgloss.JoinHorizontal(lipgloss.Top, targets, keysP, setP)

	logP := m.panel("Log", m.logVP.View(), m.width-2, m.logVP.Height+2, m.pane == paneLog)

	return lipgloss.JoinVertical(lipgloss.Left, m.header(), top, logP, m.footer())
}

func (m Model) header() string {
	state := offStyle.Render("BROADCAST OFF")
	if m.engine.Enabled() {
		state = onStyle.Render("BROADCAST ON")
	}
	return fmt.Sprintf(" %s  %s  %s  %s",
		titleStyle.Render("clonecast"),
		state,
		dimStyle.Render(fmt.Sprintf("targets: %d  keys: %s", len(m.engine.Targets()), m.engine.Filter())),
		m.masterLine(),
	)
}

// masterLine names the focused window and, when the origin gate would block
// broadcasting because it is not ticked, says so — that is the one state
// where broadcasting looks enabled but deliberately does nothing.
func (m Model) masterLine() string {
	if m.masterErr {
		return errStyle.Render("master: unknown")
	}
	if m.master == "" {
		return dimStyle.Render("master: —")
	}
	name := string(m.master)
	for _, w := range m.windows {
		if w.ID == m.master {
			name = w.Title
			break
		}
	}
	line := masterStyle.Render("master: " + truncate(name, 28))
	if m.engine.GateOrigin() && !m.masterTicked() {
		line += " " + dimStyle.Render("(not a target: gated)")
	}
	return line
}

func (m Model) footer() string {
	if m.editing != editNone {
		return dimStyle.Render(" enter: apply  esc: cancel")
	}
	switch m.pane {
	case paneKeys:
		return dimStyle.Render(" tab: pane  a: all/none  e: edit set  b: broadcast  g: gate  r: refresh  q: quit")
	case paneSettings:
		return dimStyle.Render(" tab: pane  j/k: move  space: change  e: edit  b: broadcast  g: gate  q: quit")
	case paneLog:
		return dimStyle.Render(" tab: pane  j/k: scroll  b: broadcast  g: gate  q: quit")
	}
	return dimStyle.Render(" tab: pane  j/k: move  space: tick  a: tick all  f: make master  b: broadcast  g: gate  r: refresh  q: quit")
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
		// M marks the master: focused, so it plays the keys live through
		// passthrough and delivery skips it.
		master := " "
		if m.isMaster(win) {
			master = masterStyle.Render("M")
		}
		line := fmt.Sprintf("%s %s %s", mark, master, truncate(win.Title, max(0, w-8)))
		line += dimStyle.Render("  " + truncate(win.Class, max(0, w-lipgloss.Width(line)-2)))
		if i == m.cursor && m.pane == paneTargets {
			line = cursorStyle.Render(line)
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// isMaster prefers the fast Active poll and falls back to the (slower) window
// list's own Focused flag before the first poll lands.
func (m Model) isMaster(win broadcast.Window) bool {
	if m.master != "" {
		return win.ID == m.master
	}
	return win.Focused
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
	if m.editing == editKeys {
		b.WriteString("\n" + m.input.View())
		if m.editErr != "" {
			b.WriteString("\n" + errStyle.Render(m.editErr))
		}
	} else {
		b.WriteString("\n" + dimStyle.Render("a: toggle all   e: edit"))
	}
	return b.String()
}

// viewSettings renders the runtime-tunable flags. Everything here starts from
// a command-line flag and can be changed without restarting.
func (m Model) viewSettings(w int) string {
	cfg := m.engine.Config()
	backend := "dance"
	if m.backends != nil {
		backend = m.backends.Current()
	}
	gate := offStyle.Render("OFF")
	if cfg.GateOrigin {
		gate = onStyle.Render("ON")
	}
	rows := [settingCount]struct{ label, value string }{
		setDelivery: {"delivery", backend},
		setGate:     {"origin gate", gate},
		setSettle:   {"settle", cfg.SettleDelay.String()},
		setToggle:   {"toggle key", toggleName(cfg.ToggleKey)},
	}
	var b strings.Builder
	for i, r := range rows {
		line := fmt.Sprintf("%-11s %s", r.label, r.value)
		if setting(i) == m.setCursor && m.pane == paneSettings {
			line = cursorStyle.Render(line)
		}
		b.WriteString(line + "\n")
	}
	if m.editing == editSettle || m.editing == editToggle {
		b.WriteString("\n" + m.input.View())
		if m.editErr != "" {
			b.WriteString("\n" + errStyle.Render(m.editErr))
		}
	} else {
		b.WriteString("\n" + dimStyle.Render(truncate("space: change  e: edit", w)))
	}
	return b.String()
}

func toggleName(c keys.Code) string {
	if c == 0 {
		return "none"
	}
	return keys.Name(c)
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
