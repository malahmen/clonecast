// clonecast broadcasts keyboard input to several windows at once.
//
// Usage:
//
//	clonecast [--backend mock|linux] [--keys "a b c"|all] [--toggle SCROLLLOCK]
//	          [--settle 30ms] [--log PATH]
//
// The default backend is "linux" on Linux and "mock" everywhere else.
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
	keySpec := flag.String("keys", "", `initial key set: "all" or names like "a b c f1" (default: none)`)
	toggle := flag.String("toggle", "SCROLLLOCK", "key that toggles broadcasting on/off")
	settle := flag.Duration("settle", 30*time.Millisecond, "delay between focusing a target and injecting")
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
