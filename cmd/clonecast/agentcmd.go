package main

// The `clonecast agent ...` subcommand: one-time setup that hands a Wine
// prefix ownership of its own in-bottle agent (REFERENCE.md 7.11/4.16,
// research.md §C2).
//
//	clonecast agent list
//	clonecast agent install   --prefix <path> [--exe file] [--port N] [--title T]
//	                          [--wine "<cmd>"] [--route auto|file|reg] [--assume-idle]
//	clonecast agent uninstall --prefix <path> [--wine "<cmd>"] [--keep-exe]
//	clonecast agent status    --prefix <path>
//
// Installing writes the agent to <prefix>/drive_c/clonecast/ and registers it
// under HKLM\...\CurrentVersion\RunServices, the one autostart key that Wine's
// implicit `wineboot --init` processes on every prefix boot — so the agent
// comes up under Bottles, Lutris, Proton or umu alike, with no launcher
// integration and no dark-portal. It then dies with the wineserver, so the
// host never signals it (killing it from outside used to kill the game, 4.14).
//
// --port is clonecast's port, the one the installed agent dials on every
// prefix boot (REFERENCE.md 4.17) — not a port the agent listens on, which is
// what it meant while clonecast dialled in and every instance needed its own.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/malahmen/clonecast/internal/platform/agent"
	"github.com/malahmen/clonecast/internal/prefix"
)

// defaultAgentPort is the port in agent.DefaultListen, so `agent install` and
// `clonecast --listen` cannot drift apart.
func defaultAgentPort() int {
	_, port, err := net.SplitHostPort(agent.DefaultListen)
	if err != nil {
		return 48800
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 48800
	}
	return n
}

const agentUsage = `usage: clonecast agent <command> [flags]

commands:
  list        list Wine prefixes that have running processes (Linux)
  install     copy the agent into a prefix and autostart it on every prefix boot
  uninstall   remove the autostart entry (and the copied agent)
  status      show what is installed in a prefix

run "clonecast agent <command> -h" for that command's flags`

// runAgentCmd implements `clonecast agent ...`. It writes to out so it can be
// exercised without a terminal.
func runAgentCmd(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(agentUsage)
	}
	switch args[0] {
	case "list", "ls":
		return agentList(args[1:], out)
	case "install":
		return agentInstall(args[1:], out)
	case "uninstall", "remove":
		return agentUninstall(args[1:], out)
	case "status":
		return agentStatus(args[1:], out)
	case "-h", "--help", "help":
		fmt.Fprintln(out, agentUsage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], agentUsage)
	}
}

// commonFlags are shared by install/uninstall/status.
type agentFlags struct {
	fs  *flag.FlagSet
	cfg prefix.Config
}

func newAgentFlags(name string) *agentFlags {
	a := &agentFlags{fs: flag.NewFlagSet("agent "+name, flag.ContinueOnError)}
	a.fs.StringVar(&a.cfg.Prefix, "prefix", "", "path to the Wine prefix (required)")
	a.fs.StringVar(&a.cfg.ValueName, "name", prefix.DefaultValueName, "registry value name under RunServices")
	return a
}

func (a *agentFlags) writeFlags() {
	a.fs.StringVar(&a.cfg.WineCmd, "wine", "", `shell command that runs wine for this prefix, e.g. `+
		`"flatpak run --command=<runner>/bin/wine --env=WINEPREFIX=<prefix> com.usebottles.bottles" or "umu-run"`)
	a.fs.StringVar(&a.cfg.Route, "route", prefix.RouteAuto, "auto (edit system.reg when idle, wine when running), file, or reg")
	a.fs.BoolVar(&a.cfg.AssumeIdle, "assume-idle", false, "skip the running-prefix check and edit system.reg anyway")
}

func (a *agentFlags) parse(args []string) error {
	if err := a.fs.Parse(args); err != nil {
		return err
	}
	if a.cfg.Prefix == "" {
		return fmt.Errorf("--prefix is required (try `clonecast agent list`)")
	}
	return nil
}

func agentInstall(args []string, out io.Writer) error {
	a := newAgentFlags("install")
	a.writeFlags()
	a.fs.StringVar(&a.cfg.ExePath, "exe", "", "built clonecast-agent.exe (default: next to this binary, or ./bin/)")
	a.fs.IntVar(&a.cfg.Port, "port", defaultAgentPort(), "clonecast's listening port for the agent to dial (CLONECAST_PORT in the prefix's environment overrides it)")
	a.fs.StringVar(&a.cfg.Title, "title", "", `window title the agent delivers to (default: the agent's own, "World of Warcraft")`)
	if err := a.parse(args); err != nil {
		return err
	}
	res, err := prefix.Install(a.cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "installed into %s\n", res.Prefix)
	if res.Arch != "" {
		fmt.Fprintf(out, "  prefix arch : %s\n", res.Arch)
	}
	fmt.Fprintf(out, "  agent       : %s  (%s inside the prefix)\n", res.CopiedTo, res.WinPath)
	fmt.Fprintf(out, "  autostart   : HKLM\\%s, value %q\n", prefix.RunServicesKey, a.valueNameOr())
	fmt.Fprintf(out, "  command     : %s\n", res.Command)
	fmt.Fprintf(out, "  route       : %s (%s)\n", res.Route, routeWords(res.Route))
	printNotes(out, res.Notes)
	return nil
}

func agentUninstall(args []string, out io.Writer) error {
	a := newAgentFlags("uninstall")
	a.writeFlags()
	a.fs.BoolVar(&a.cfg.KeepExe, "keep-exe", false, "leave the copied agent binary in the prefix")
	if err := a.parse(args); err != nil {
		return err
	}
	res, err := prefix.Uninstall(a.cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "uninstalled from %s (route: %s)\n", res.Prefix, res.Route)
	printNotes(out, res.Notes)
	return nil
}

func agentStatus(args []string, out io.Writer) error {
	a := newAgentFlags("status")
	if err := a.parse(args); err != nil {
		return err
	}
	st, err := prefix.Describe(a.cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "prefix      : %s\n", st.Prefix)
	if st.Arch != "" {
		fmt.Fprintf(out, "arch        : %s\n", st.Arch)
	}
	if st.Exe != "" {
		fmt.Fprintf(out, "agent exe   : %s (%d bytes, %s)\n", st.Exe, st.ExeSize, st.ExeMod.Format("2006-01-02 15:04"))
	} else {
		fmt.Fprintf(out, "agent exe   : not installed\n")
	}
	if st.Value != "" {
		fmt.Fprintf(out, "autostart   : %s = %s\n", st.ValueName, st.Value)
	} else {
		fmt.Fprintf(out, "autostart   : no %q value under HKLM\\%s\n", st.ValueName, prefix.RunServicesKey)
	}
	switch {
	case !st.BusyKnown:
		fmt.Fprintf(out, "running     : unknown (needs /proc; Linux only)\n")
	case st.Busy:
		fmt.Fprintf(out, "running     : yes — %d process(es); system.reg is stale while it runs\n", len(st.Processes))
		for _, p := range st.Processes {
			fmt.Fprintf(out, "              pid %d  %s\n", p.PID, p.Command)
		}
	default:
		fmt.Fprintf(out, "running     : no (system.reg can be edited directly)\n")
	}
	return nil
}

func agentList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("agent list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	procs, err := prefix.Discover()
	if err != nil {
		return err
	}
	if len(procs) == 0 {
		fmt.Fprintln(out, "no running Wine prefixes found (start the game, or pass --prefix by hand)")
		return nil
	}
	for _, p := range procs {
		fmt.Fprintf(out, "%s\n", p.Prefix)
		for _, r := range p.Processes {
			fmt.Fprintf(out, "    pid %-7d %s\n", r.PID, r.Command)
		}
	}
	fmt.Fprintln(out, "\ninstall into one with: clonecast agent install --prefix <path>")
	return nil
}

func (a *agentFlags) valueNameOr() string {
	if a.cfg.ValueName == "" {
		return prefix.DefaultValueName
	}
	return a.cfg.ValueName
}

func routeWords(route string) string {
	switch route {
	case prefix.RouteFile:
		return "edited the prefix's system.reg directly"
	case prefix.RouteReg:
		return "ran `reg add` through the supplied wine command"
	}
	return route
}

func printNotes(out io.Writer, notes []string) {
	for _, n := range notes {
		for i, line := range strings.Split(n, "\n") {
			if i == 0 {
				fmt.Fprintf(out, "  - %s\n", line)
				continue
			}
			fmt.Fprintf(out, "    %s\n", line)
		}
	}
}

// agentCmdMain is the entry point used by main(): it prints to stdout and maps
// an error to a non-zero exit.
func agentCmdMain(args []string) int {
	if err := runAgentCmd(args, os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintln(os.Stderr, "clonecast agent:", err)
		return 1
	}
	return 0
}
