// clonecast broadcasts keyboard input to several windows at once.
//
// Usage:
//
//	clonecast [--backend mock|linux] [--deliver agent|dance|xsend]
//	          [--listen 127.0.0.1:48800] [--xsend "Title=X Title,..."]
//	          [--keys "a b c"|all] [--toggle SCROLLLOCK] [--settle 30ms]
//	          [--gate-origin=false] [--log PATH]
//	          [--agent-exe PATH] [--agent-title TITLE] [--agent-wine "CMD"]
//
// --backend picks the platform (default "linux" on Linux, "mock" elsewhere).
// --deliver picks how broadcast keys reach targets: the in-bottle PostMessage
// "agent" (the default and the only path proven on real clients), the focus
// "dance" (legacy last resort, the default with --backend mock since no agent
// can register there), or "xsend" (experimental X11 XSendEvent; see
// REFERENCE.md 4.12).
//
// --listen is the one loopback address every in-bottle agent dials. Agents
// register themselves and are paired to compositor windows by Wine prefix
// (REFERENCE.md 4.17), so ticking a window in the TUI is all target selection
// takes: there is no port to pick per instance and no title map to write.
//
// The window that has focus is the master: it gets keys through passthrough
// and is skipped by delivery. --gate-origin=false lifts the default rule that
// the focused window must itself be a ticked target for anything to broadcast.
//
// --agent-exe/--agent-title/--agent-wine seed the TUI's Prefixes pane, which
// is the interactive face of `clonecast agent install/uninstall/status`
// (REFERENCE.md 4.19): install/remove the in-bottle agent in a Wine prefix
// (picked from the running-process list, or typed by path for an idle one)
// without leaving the TUI.
//
// Every flag below is also reachable from the TUI at runtime (Settings pane
// for the broadcast options, Prefixes pane for the agent-install ones), so
// flags are the starting values, not the only way to set them.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
	"github.com/malahmen/clonecast/internal/logcap"
	"github.com/malahmen/clonecast/internal/platform/agent"
	"github.com/malahmen/clonecast/internal/tui"
)

// platform bundles the three backend pieces. Built by newMockPlatform or, on
// Linux, newLinuxPlatform (see platform_linux.go).
type platform struct {
	src   broadcast.Source
	inj   broadcast.Injector
	wm    broadcast.WindowManager
	close func()
	// devCtl backs the Keyboards pane. nil on the mock backend, which has no
	// evdev devices to offer — the pane then says so rather than pretending.
	devCtl tui.DeviceController
	// setNotify, when set, routes the platform's own notices to the engine's
	// Log pane and log file. nil when the platform has none.
	setNotify func(func(text string, isErr bool))
}

func main() {
	// Subcommand dispatch. `clonecast agent ...` is one-time setup (install
	// the in-bottle agent into a Wine prefix, see agentcmd.go) and never
	// starts the TUI; everything else is the broadcaster.
	if len(os.Args) > 1 && os.Args[1] == "agent" {
		os.Exit(agentCmdMain(os.Args[2:]))
	}

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "clonecast:", err)
		os.Exit(1)
	}
}

func run() error {
	defBackend := "mock"
	if runtime.GOOS == "linux" {
		defBackend = "linux"
	}
	backend := flag.String("backend", defBackend, "mock or linux")
	deliver := flag.String("deliver", "", "delivery backend: agent (default), dance (legacy fallback), or xsend (experimental); default dance with --backend mock")
	listen := flag.String("listen", agent.DefaultListen, "loopback address in-bottle agents connect to and register on")
	xsendSpec := flag.String("xsend", "", `xsend X-window-title overrides by window title, e.g. "Almerinda=World of Warcraft" (with --deliver xsend)`)
	keySpec := flag.String("keys", "", `initial key set: "all" or names like "a b c f1" (default: none)`)
	toggle := flag.String("toggle", "SCROLLLOCK", "key that toggles broadcasting on/off")
	settle := flag.Duration("settle", 30*time.Millisecond, "delay between focusing a target and injecting (dance backend)")
	gateOrigin := flag.Bool("gate-origin", true, "only broadcast while the focused window is itself a ticked target")
	masterByAgent := flag.Bool("master-by-agent", false, "while broadcasting, send the focused game's own keys through its agent too, so a game that takes the X keyboard focus gets nothing (experimental)")
	logPath := flag.String("log", defaultLogPath(), "log file (stdout is the TUI)")
	agentExe := flag.String("agent-exe", "", "built clonecast-agent.exe for the TUI's Prefixes pane to install (default: look next to this binary or in ./bin)")
	agentTitle := flag.String("agent-title", "", `window title an agent installed from the TUI delivers to ("" = agent default, "World of Warcraft")`)
	agentWine := flag.String("agent-wine", "", `wine command for the TUI's Prefixes pane to install into an already-booted prefix, e.g. "flatpak run --command=<runner>/bin/wine --env=WINEPREFIX=<prefix> com.usebottles.bottles"`)
	kbdSpec := flag.String("keyboard", "", "comma-separated evdev paths to capture, e.g. /dev/input/event20 (default: every discovered keyboard)")
	listKbd := flag.Bool("list-keyboards", false, "list the keyboards discovery would capture and whether each can be grabbed, then exit")
	flag.Parse()

	// Answered before anything is grabbed, because choosing a device is what
	// this is for: discovery takes any device reporting KEY_A and KEY_ENTER,
	// which on a real desk also matches a gaming mouse's macro interface, both
	// nodes of one keyboard, and both the physical keyboard a remapper has
	// grabbed AND the virtual one that remapper publishes.
	if *listKbd {
		return listKeyboards()
	}

	if err := setupLog(*logPath); err != nil {
		return err
	}

	cfg := broadcast.DefaultConfig()
	cfg.SettleDelay = *settle
	cfg.GateOrigin = *gateOrigin
	cfg.MasterByAgent = *masterByAgent
	if *toggle != "" {
		c, err := keys.Parse(*toggle)
		if err != nil {
			return fmt.Errorf("--toggle: %w", err)
		}
		cfg.ToggleKey = c
	} else {
		cfg.ToggleKey = 0
	}

	var p *platform
	var err error
	switch *backend {
	case "mock":
		p = newMockPlatform()
	case "linux":
		// Which keyboard to capture is settled BEFORE anything is grabbed:
		// asking afterwards would mean grabbing first and guessing, which is
		// what made every keypress arrive twice.
		var kbd []string
		if kbd, err = resolveKeyboards(*kbdSpec); err == nil {
			p, err = newLinuxPlatform(kbd...)
		}
	default:
		err = fmt.Errorf("unknown backend %q", *backend)
	}
	if err != nil {
		return err
	}
	defer p.close()

	engine := broadcast.New(p.src, p.inj, p.wm, cfg)
	// With master by agent on, the master is KWin's active window alone, not
	// corrected by the X focus (REFERENCE.md 4.25); the Settings pane can
	// switch it while running.
	if c, ok := p.wm.(interface{ SetCorrectWhen(func() bool) }); ok {
		c.SetCorrectWhen(func() bool { return !engine.Config().MasterByAgent })
	}
	// Notices are what the README sends people to the log file for: agent
	// registrations, pairing, the origin gate, delivery failures.
	engine.SetNoticeSink(func(n broadcast.Notice) {
		if n.Err {
			log.Warn(n.Text)
		} else {
			log.Info(n.Text)
		}
	})
	if p.setNotify != nil {
		p.setNotify(engine.Notify)
	}

	// The agent registry listens for the whole run, whatever backend is
	// selected: agents connect when their prefix boots, which may be long
	// before (or after) the user switches delivery to them, and the TUI marks
	// the windows they cover either way.
	token, err := agentToken()
	if err != nil {
		return err
	}
	agents := agent.NewRegistry(agent.Options{
		Windows: p.wm.List,
		Notify:  engine.Notify,
		Token:   token,
	})
	defer agents.Close()
	if err := agents.Listen(*listen); err != nil {
		// On the real platform this is fatal: with nothing listening, no
		// agent can ever register and the default backend has no targets to
		// reach. With --backend mock nothing can register anyway, so a busy
		// port (a second `make run-mock`) must not stop the demo — the
		// Settings pane shows "(not listening)" and --listen/the TUI can
		// point it somewhere free.
		if *backend != "mock" {
			return err
		}
		engine.Notify("agent registry: "+err.Error(), true)
	}
	if *keySpec != "" {
		set, err := keys.ParseSet(*keySpec)
		if err != nil {
			return fmt.Errorf("--keys: %w", err)
		}
		engine.SetFilter(set)
	}

	bs := newBackends(engine, p.wm, agents, *xsendSpec)
	defer bs.Close()
	warn, err := bs.Start(defaultDeliver(*deliver, *backend))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go agents.Run(ctx) // keeps window <-> agent pairing fresh
	if warn != "" {
		// Not fatal: an agent registers on its own schedule, so this is a
		// state the run can grow out of without a restart.
		engine.Notify(warn, true) // to the log file too, through the sink
	}
	engineErr := make(chan error, 1)
	go func() { engineErr <- engine.Run(ctx) }()

	prefixCfg := tui.PrefixPaneConfig{
		ExePath: *agentExe,
		// Asked at install time, not read from *listen here: the TUI can
		// rebind the listener, after which a port captured now is wrong.
		Port:    func() int { return portFromAddr(agents.Addr()) },
		Title:   *agentTitle,
		WineCmd: *agentWine,
		Token:   token,
	}
	prog := tea.NewProgram(tui.New(engine, p.wm, bs, agents, prefixCfg, p.devCtl), tea.WithAltScreen())
	go func() {
		if err := <-engineErr; err != nil && ctx.Err() == nil {
			log.Error("engine stopped", "err", err)
			prog.Quit()
		}
	}()

	_, err = prog.Run()
	return err
}

// defaultDeliver resolves an empty --deliver. The agent backend is the
// reliable one and therefore the mainline default (REFERENCE.md 4.12/7.9,
// research issue I11); the mock platform has no Wine prefixes and so nothing
// that could register, so there the demo default stays the self-contained
// focus dance.
func defaultDeliver(deliver, backend string) string {
	if deliver != "" {
		return deliver
	}
	if backend == "mock" {
		return deliverDance
	}
	return deliverAgent
}

// portFromAddr pulls the port out of a "host:port" listen address, for
// seeding the Prefixes pane's install default: an agent installed from the
// TUI should dial the port clonecast is actually listening on, not a
// hardcoded one. 0 (the agent's own default) if addr doesn't parse.
func portFromAddr(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return n
}

func defaultLogPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "clonecast.log"
	}
	return filepath.Join(dir, "clonecast", "clonecast.log")
}

// logCap is the log's size cap; the full one is kept as <log>.1.
const logCap = 10 << 20

func setupLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := logcap.Open(path, logCap)
	if err != nil {
		return err
	}
	log.SetOutput(f)
	log.SetLevel(log.DebugLevel)
	log.SetReportTimestamp(true)
	return nil
}

// splitPaths turns a comma-separated --keyboard value into device paths,
// dropping empties so a trailing comma is not a device named "".
func splitPaths(spec string) []string {
	var out []string
	for _, p := range strings.Split(spec, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
