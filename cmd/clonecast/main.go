// clonecast broadcasts keyboard input to several windows at once.
//
// Usage:
//
//	clonecast [--backend mock|linux] [--deliver agent|dance|xsend]
//	          [--listen 127.0.0.1:48800] [--xsend "Title=X Title,..."]
//	          [--keys "a b c"|all] [--toggle SCROLLLOCK] [--settle 30ms]
//	          [--gate-origin=false] [--log PATH]
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
// Every flag below is also reachable from the TUI at runtime (Settings pane),
// so flags are the starting values, not the only way to set them.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
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
	logPath := flag.String("log", defaultLogPath(), "log file (stdout is the TUI)")
	flag.Parse()

	if err := setupLog(*logPath); err != nil {
		return err
	}

	cfg := broadcast.DefaultConfig()
	cfg.SettleDelay = *settle
	cfg.GateOrigin = *gateOrigin
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
		p, err = newLinuxPlatform()
	default:
		err = fmt.Errorf("unknown backend %q", *backend)
	}
	if err != nil {
		return err
	}
	defer p.close()

	engine := broadcast.New(p.src, p.inj, p.wm, cfg)

	// The agent registry listens for the whole run, whatever backend is
	// selected: agents connect when their prefix boots, which may be long
	// before (or after) the user switches delivery to them, and the TUI marks
	// the windows they cover either way.
	agents := agent.NewRegistry(agent.Options{
		Windows: p.wm.List,
		Notify:  engine.Notify,
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
		log.Warn("agent registry", "err", err)
		engine.Notify(err.Error(), true)
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
		engine.Notify(warn, true)
		log.Warn(warn)
	}
	engineErr := make(chan error, 1)
	go func() { engineErr <- engine.Run(ctx) }()

	prog := tea.NewProgram(tui.New(engine, p.wm, bs, agents), tea.WithAltScreen())
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

func defaultLogPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "clonecast.log"
	}
	return filepath.Join(dir, "clonecast", "clonecast.log")
}

func setupLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	log.SetOutput(f)
	log.SetLevel(log.DebugLevel)
	log.SetReportTimestamp(true)
	return nil
}
