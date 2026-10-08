package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The Devices pane: which keyboards exist, which are being captured, and
// changing that while running.
//
// Capture used to be decided once at startup from a blunt heuristic — any
// device reporting KEY_A and KEY_ENTER — which on a real desk matches both
// event nodes of one keyboard and a gaming mouse's macro interfaces. Nothing
// but typing says which device you actually use, so a wrong guess could only be
// corrected by editing a config file and restarting. This pane is the fix: the
// list says what exists and what is live, and a keypress changes it.

// Device is one keyboard discovery found.
type Device struct {
	Path string
	Name string
	Busy string // empty when grabbable; otherwise why not
}

// DeviceController is the capture side, kept behind an interface so this
// package stays free of evdev and remains testable with fabricated messages.
type DeviceController interface {
	Devices() ([]Device, error)
	Captured() []string
	Capture(paths []string) error
	Save(paths []string) error
}

type (
	deviceListMsg struct {
		devices []Device
		err     error
	}
	deviceCaptureMsg struct {
		paths []string
		err   error
	}
	deviceSaveMsg struct{ err error }
)

// DevicePane lists keyboards and switches capture between them.
type DevicePane struct {
	ctl      DeviceController
	devices  []Device
	captured map[string]bool
	cursor   int
	status   string
	err      bool
	busy     bool
}

func NewDevicePane(ctl DeviceController) DevicePane {
	return DevicePane{ctl: ctl, captured: map[string]bool{}}
}

// Refresh re-lists devices and re-reads what is captured.
func (p DevicePane) Refresh() tea.Cmd {
	ctl := p.ctl
	return func() tea.Msg {
		if ctl == nil {
			return deviceListMsg{err: fmt.Errorf("no device backend (mock run?)")}
		}
		d, err := ctl.Devices()
		return deviceListMsg{devices: d, err: err}
	}
}

func (p DevicePane) capture(paths []string) tea.Cmd {
	ctl := p.ctl
	return func() tea.Msg {
		err := ctl.Capture(paths)
		return deviceCaptureMsg{paths: paths, err: err}
	}
}

func (p DevicePane) save(paths []string) tea.Cmd {
	ctl := p.ctl
	return func() tea.Msg { return deviceSaveMsg{err: ctl.Save(paths)} }
}

// selection is the captured set as a stable, ordered list.
func (p DevicePane) selection() []string {
	var out []string
	for _, d := range p.devices {
		if p.captured[d.Path] {
			out = append(out, d.Path)
		}
	}
	return out
}

func (p DevicePane) Update(msg tea.Msg) (DevicePane, tea.Cmd) {
	switch msg := msg.(type) {
	case deviceListMsg:
		p.busy = false
		if msg.err != nil {
			p.status, p.err = "scan: "+msg.err.Error(), true
			return p, nil
		}
		p.devices, p.err = msg.devices, false
		p.captured = map[string]bool{}
		if p.ctl != nil {
			for _, c := range p.ctl.Captured() {
				p.captured[c] = true
			}
		}
		if p.cursor >= len(p.devices) {
			p.cursor = max(0, len(p.devices)-1)
		}
		p.status = fmt.Sprintf("%d keyboard(s), %d captured", len(p.devices), len(p.selection()))
		return p, nil

	case deviceCaptureMsg:
		p.busy = false
		if msg.err != nil {
			// The capture did not change, so the ticks must not either:
			// re-reading is what keeps the list honest about what is live.
			p.status, p.err = "capture: "+msg.err.Error(), true
			return p, p.Refresh()
		}
		p.err = false
		p.status = "capturing " + strings.Join(short(msg.paths), ", ")
		return p, p.Refresh()

	case deviceSaveMsg:
		p.busy = false
		if msg.err != nil {
			p.status, p.err = "save: "+msg.err.Error(), true
			return p, nil
		}
		p.err = false
		p.status = "saved as the default for next time"
		return p, nil

	case tea.KeyMsg:
		if p.busy {
			return p, nil
		}
		switch msg.String() {
		case "r":
			p.busy = true
			p.status, p.err = "scanning…", false
			return p, p.Refresh()
		case "j", "down":
			if p.cursor < len(p.devices)-1 {
				p.cursor++
			}
			return p, nil
		case "k", "up":
			if p.cursor > 0 {
				p.cursor--
			}
			return p, nil
		case " ", "enter":
			if p.cursor < 0 || p.cursor >= len(p.devices) {
				return p, nil
			}
			d := p.devices[p.cursor]
			next := map[string]bool{}
			for k, v := range p.captured {
				next[k] = v
			}
			next[d.Path] = !next[d.Path]

			var paths []string
			for _, dev := range p.devices {
				if next[dev.Path] {
					paths = append(paths, dev.Path)
				}
			}
			// Capturing nothing is not "capture nothing": an empty set means
			// "discover everything" further down, which is the behaviour this
			// pane exists to get away from.
			if len(paths) == 0 {
				p.status, p.err = "at least one keyboard has to stay captured", true
				return p, nil
			}
			p.busy = true
			p.status, p.err = "re-grabbing…", false
			return p, p.capture(paths)
		case "s":
			sel := p.selection()
			if len(sel) == 0 {
				p.status, p.err = "nothing captured to save", true
				return p, nil
			}
			p.busy = true
			return p, p.save(sel)
		}
	}
	return p, nil
}

func (p DevicePane) View(w, h int) string {
	var b strings.Builder
	if len(p.devices) == 0 {
		b.WriteString(prefixPaneDim.Render("no keyboards found (r to rescan)") + "\n")
	}

	statusLines := wrapTo(p.status, w)
	rows := max(1, h-5-(len(statusLines)-1))
	start := 0
	if p.cursor >= rows {
		start = p.cursor - rows + 1
	}
	for i := start; i < len(p.devices) && i < start+rows; i++ {
		d := p.devices[i]
		mark := "[ ]"
		if p.captured[d.Path] {
			mark = "[x]"
		}
		note := d.Name
		if d.Busy != "" {
			// A device another process holds exclusively (keyd,
			// input-remapper) cannot be captured, and saying so here is the
			// difference between choosing and guessing.
			note = d.Name + " — " + d.Busy
		}
		line := fmt.Sprintf("%s %-20s %s", mark, d.Path, note)
		line = truncate(line, w)
		if i == p.cursor {
			line = prefixPaneSel.Render(line)
		} else if d.Busy != "" {
			line = prefixPaneDim.Render(line)
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n")
	for _, l := range statusLines {
		if p.err {
			l = prefixPaneErr.Render(l)
		}
		b.WriteString(l + "\n")
	}
	b.WriteString(prefixPaneDim.Render(truncate("space: capture/release  s: save as default  r: rescan", w)))
	return b.String()
}

// short trims device paths for a status line.
func short(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.TrimPrefix(p, "/dev/input/"))
	}
	return out
}
