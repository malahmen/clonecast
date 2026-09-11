// clonecast broadcasts keyboard input to several windows at once.
//
// Usage:
//
//	clonecast [--backend mock|linux] [--deliver dance|agent|xsend]
//	          [--agent "Title=host:port,..."] [--xsend "Title=X Title,..."]
//	          [--keys "a b c"|all] [--toggle SCROLLLOCK] [--settle 30ms] [--log PATH]
//
// --backend picks the platform (default "linux" on Linux, "mock" elsewhere).
// --deliver picks how broadcast keys reach targets: the focus "dance" (default),
// the in-bottle PostMessage "agent" (reliable for Wine/Proton; needs --agent to
// map each target window title to its agent's loopback endpoint), or "xsend"
// (experimental X11 XSendEvent; see REFERENCE.md 4.12).
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
	deliver := flag.String("deliver", "dance", "delivery backend: dance, agent, or xsend")
	agentSpec := flag.String("agent", "", `agent endpoints by window title, e.g. "Malahmen=127.0.0.1:48900,Marx=48901" (with --deliver agent)`)
	xsendSpec := flag.String("xsend", "", `xsend X-window-title overrides by window title, e.g. "Almerinda=World of Warcraft" (with --deliver xsend)`)
	keySpec := flag.String("keys", "", `initial key set: "all" or names like "a b c f1" (default: none)`)
	toggle := flag.String("toggle", "SCROLLLOCK", "key that toggles broadcasting on/off")
	settle := flag.Duration("settle", 30*time.Millisecond, "delay between focusing a target and injecting (dance backend)")
	logPath := flag.String("log", defaultLogPath(), "log file (stdout is the TUI)")
	flag.Parse()

	if err := setupLog(*logPath); err != nil {
		return err
	}

	cfg := broadcast.DefaultConfig()
	cfg.SettleDelay = *settle
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
	if *keySpec != "" {
		set, err := keys.ParseSet(*keySpec)
		if err != nil {
			return fmt.Errorf("--keys: %w", err)
		}
		engine.SetFilter(set)
	}

	closeDeliverer, err := selectDeliverer(engine, p.wm, *deliver, *agentSpec, *xsendSpec)
	if err != nil {
		return err
	}
	defer closeDeliverer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engineErr := make(chan error, 1)
	go func() { engineErr <- engine.Run(ctx) }()

	prog := tea.NewProgram(tui.New(engine, p.wm), tea.WithAltScreen())
	go func() {
		if err := <-engineErr; err != nil && ctx.Err() == nil {
			log.Error("engine stopped", "err", err)
			prog.Quit()
		}
	}()

	_, err = prog.Run()
	return err
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
