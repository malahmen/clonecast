package prefix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNoProc is returned where prefix discovery needs /proc and there is none
// (the macOS development build).
var ErrNoProc = errors.New("prefix discovery needs /proc (Linux only)")

// Routes for getting the autostart value into the prefix's registry.
const (
	// RouteAuto edits system.reg when the prefix is idle and shells out to a
	// wine command when it is booted. This is the default.
	RouteAuto = "auto"
	// RouteFile always edits <prefix>/system.reg. Refused on a booted prefix.
	RouteFile = "file"
	// RouteReg always runs `<wine cmd> reg add ...`, which needs --wine.
	RouteReg = "reg"
)

// InstallDirWin is where the agent lives inside the prefix. It must be under
// drive_c: under Bottles' Flatpak, Z:\ only exposes the sandbox's bind mounts,
// not $HOME, so a binary in ~/.local/bin is invisible to the bottle, while
// drive_c is always visible to its own prefix under every launcher.
const InstallDirWin = `C:\clonecast`

// ExeName is the agent binary's name inside the prefix.
const ExeName = "clonecast-agent.exe"

// Config describes one install/uninstall.
type Config struct {
	Prefix     string // host path to the Wine prefix
	ExePath    string // host path to the built clonecast-agent.exe
	Port       int    // agent's loopback listen port (0 = leave the agent's default)
	Title      string // target window title passed to the agent ("" = agent default)
	WineCmd    string // shell command that runs wine for this prefix, e.g. a flatpak invocation
	Route      string // RouteAuto (default), RouteFile or RouteReg
	ValueName  string // registry value name ("" = DefaultValueName)
	AssumeIdle bool   // skip the "is this prefix booted?" check and edit system.reg anyway
	KeepExe    bool   // uninstall: leave the copied exe in place
}

func (c Config) valueName() string {
	if c.ValueName == "" {
		return DefaultValueName
	}
	return c.ValueName
}

// Result reports what an install or uninstall actually did.
type Result struct {
	Prefix   string
	Route    string   // "file" or "reg": which route was used
	Arch     string   // prefix architecture from system.reg's #arch, if known
	ExeDest  string   // host path the agent was copied to / removed from
	WinPath  string   // the agent's path as the prefix sees it
	Command  string   // the registry value's data (the autostart command line)
	Changed  bool     // the registry actually changed
	CopiedTo string   // non-empty if the exe was copied this run
	Notes    []string // human-readable remarks (route reasoning, warnings)
}

func (r *Result) note(format string, a ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, a...))
}

// Status is what `clonecast agent status` reports.
type Status struct {
	Prefix    string
	Arch      string
	Exe       string // host path of the installed agent, "" if absent
	ExeSize   int64
	ExeMod    time.Time
	Value     string // the RunServices value's data, "" if absent
	ValueName string
	Busy      bool
	BusyKnown bool
	Processes []Running
}

// Install copies the agent into the prefix and registers it under RunServices
// so that every boot of that prefix starts it, whatever launched the game.
func Install(cfg Config) (*Result, error) {
	if err := Validate(cfg.Prefix); err != nil {
		return nil, err
	}
	exe, err := FindAgentExe(cfg.ExePath)
	if err != nil {
		return nil, err
	}
	res := &Result{Prefix: CleanPrefix(cfg.Prefix), WinPath: InstallDirWin + `\` + ExeName}
	route, err := chooseRoute(cfg, res)
	if err != nil {
		return nil, err
	}
	res.Route = route

	dest := ExeDest(cfg.Prefix)
	res.ExeDest = dest
	if err := copyFile(exe, dest); err != nil {
		return nil, fmt.Errorf("copy agent into prefix: %w", err)
	}
	res.CopiedTo = dest

	res.Command = commandLine(cfg)
	name := cfg.valueName()
	switch route {
	case RouteFile:
		f, err := LoadRegFile(SystemReg(cfg.Prefix))
		if err != nil {
			return nil, err
		}
		res.Arch = f.Arch
		res.Changed = f.SetString(RunServicesKey, name, res.Command)
		if !res.Changed {
			res.note("registry value was already exactly this; system.reg untouched")
			return res, nil
		}
		if err := f.Save(); err != nil {
			return nil, fmt.Errorf("write system.reg: %w", err)
		}
		res.note("edited %s (backup: %s)", SystemReg(cfg.Prefix), SystemReg(cfg.Prefix)+".clonecast.bak")
	case RouteReg:
		out, err := runWine(cfg.WineCmd, cfg.Prefix,
			"reg", "add", `HKLM\`+RunServicesKey, "/v", name, "/t", "REG_SZ", "/d", res.Command, "/f")
		if err != nil {
			return nil, fmt.Errorf("%w\n%s", err, out)
		}
		res.Changed = true
		res.note("ran `reg add` through the supplied wine command")
		if s := strings.TrimSpace(out); s != "" {
			res.note("wine output: %s", firstLine(s))
		}
	}
	res.note("the agent starts on the prefix's next boot; it does not start an already-running prefix")
	return res, nil
}

// Uninstall removes the autostart entry and, unless KeepExe, the copied agent.
func Uninstall(cfg Config) (*Result, error) {
	if err := Validate(cfg.Prefix); err != nil {
		return nil, err
	}
	res := &Result{Prefix: CleanPrefix(cfg.Prefix), WinPath: InstallDirWin + `\` + ExeName}
	route, err := chooseRoute(cfg, res)
	if err != nil {
		return nil, err
	}
	res.Route = route
	name := cfg.valueName()

	switch route {
	case RouteFile:
		f, err := LoadRegFile(SystemReg(cfg.Prefix))
		if err != nil {
			return nil, err
		}
		res.Arch = f.Arch
		res.Changed = f.DeleteValue(RunServicesKey, name)
		if res.Changed {
			if err := f.Save(); err != nil {
				return nil, fmt.Errorf("write system.reg: %w", err)
			}
			res.note("removed %q from %s", name, RunServicesKey)
		} else {
			res.note("no %q value under %s; nothing to remove", name, RunServicesKey)
		}
	case RouteReg:
		out, err := runWine(cfg.WineCmd, cfg.Prefix,
			"reg", "delete", `HKLM\`+RunServicesKey, "/v", name, "/f")
		if err != nil {
			res.note("reg delete reported: %v (%s)", err, firstLine(strings.TrimSpace(out)))
		} else {
			res.Changed = true
		}
	}

	if !cfg.KeepExe {
		dest := ExeDest(cfg.Prefix)
		res.ExeDest = dest
		if err := os.Remove(dest); err == nil {
			res.note("removed %s", dest)
			// Remove the directory too, but only if we left it empty.
			_ = os.Remove(filepath.Dir(dest))
		} else if !os.IsNotExist(err) {
			res.note("could not remove %s: %v", dest, err)
		}
	}
	return res, nil
}

// Describe reports what is installed in a prefix.
func Describe(cfg Config) (*Status, error) {
	if err := Validate(cfg.Prefix); err != nil {
		return nil, err
	}
	st := &Status{Prefix: CleanPrefix(cfg.Prefix), ValueName: cfg.valueName()}
	if f, err := LoadRegFile(SystemReg(cfg.Prefix)); err == nil {
		st.Arch = f.Arch
		st.Value, _ = f.GetString(RunServicesKey, st.ValueName)
	} else {
		return nil, err
	}
	if info, err := os.Stat(ExeDest(cfg.Prefix)); err == nil {
		st.Exe = ExeDest(cfg.Prefix)
		st.ExeSize = info.Size()
		st.ExeMod = info.ModTime()
	}
	busy, hits, err := Busy(cfg.Prefix)
	if err == nil {
		st.Busy, st.BusyKnown, st.Processes = busy, true, hits
	}
	return st, nil
}

// ---- plumbing ---------------------------------------------------------------

// SystemReg is the prefix's HKEY_LOCAL_MACHINE registry file.
func SystemReg(prefix string) string { return filepath.Join(prefix, "system.reg") }

// ExeDest is where the agent is copied on the host.
func ExeDest(prefix string) string {
	return filepath.Join(prefix, "drive_c", "clonecast", ExeName)
}

// Validate checks that a path really looks like a Wine prefix.
func Validate(prefix string) error {
	if strings.TrimSpace(prefix) == "" {
		return errors.New("no prefix given (--prefix, or `clonecast agent list` to see running ones)")
	}
	info, err := os.Stat(prefix)
	if err != nil {
		return fmt.Errorf("prefix %s: %w", prefix, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("prefix %s is not a directory", prefix)
	}
	for _, must := range []string{"system.reg", "drive_c"} {
		if _, err := os.Stat(filepath.Join(prefix, must)); err != nil {
			return fmt.Errorf("%s does not look like a Wine prefix (no %s)", prefix, must)
		}
	}
	return nil
}

// commandLine builds the autostart command line stored in the registry. The
// path is fixed and space-free, so it needs no quoting; the title may contain
// spaces and is quoted the way CommandLineToArgvW expects.
func commandLine(cfg Config) string {
	cmd := InstallDirWin + `\` + ExeName
	if cfg.Port > 0 {
		cmd += fmt.Sprintf(" -port %d", cfg.Port)
	}
	if cfg.Title != "" {
		cmd += ` -title "` + strings.ReplaceAll(cfg.Title, `"`, `\"`) + `"`
	}
	return cmd
}

// chooseRoute picks between editing system.reg and shelling out to wine, and
// records why. The rule: a booted prefix must never have its system.reg edited
// (wineserver rewrites it from memory on shutdown and the edit vanishes), and
// an idle prefix should not be booted just to write one value.
func chooseRoute(cfg Config, res *Result) (string, error) {
	switch cfg.Route {
	case RouteReg:
		if cfg.WineCmd == "" {
			return "", errors.New(`--route reg needs --wine "<command that runs wine for this prefix>"`)
		}
		return RouteReg, nil
	case RouteFile:
		if cfg.AssumeIdle {
			res.note("route: system.reg edit (--assume-idle, no check made)")
			return RouteFile, nil
		}
		busy, hits, err := Busy(cfg.Prefix)
		if err != nil {
			return "", fmt.Errorf("cannot tell whether the prefix is running: %w (pass --assume-idle if it is shut down)", err)
		}
		if busy {
			return "", busyError(hits)
		}
		res.note("route: system.reg edit (prefix is idle)")
		return RouteFile, nil
	case "", RouteAuto:
	default:
		return "", fmt.Errorf("unknown --route %q (want auto, file or reg)", cfg.Route)
	}

	if cfg.AssumeIdle {
		res.note("route: system.reg edit (--assume-idle, no check made)")
		return RouteFile, nil
	}
	busy, hits, err := Busy(cfg.Prefix)
	switch {
	case err != nil && cfg.WineCmd != "":
		res.note("route: wine `reg` (cannot check whether the prefix is running: %v)", err)
		return RouteReg, nil
	case err != nil:
		return "", fmt.Errorf("cannot tell whether the prefix is running: %w (pass --assume-idle if it is shut down, or --wine to go through wine)", err)
	case busy && cfg.WineCmd != "":
		res.note("route: wine `reg` (prefix is running: %s)", processList(hits))
		return RouteReg, nil
	case busy:
		return "", busyError(hits)
	case cfg.WineCmd != "":
		res.note("route: system.reg edit (prefix is idle; --wine not needed, so not used)")
		return RouteFile, nil
	default:
		res.note("route: system.reg edit (prefix is idle)")
		return RouteFile, nil
	}
}

func busyError(hits []Running) error {
	return fmt.Errorf(`prefix is running (%s), so its system.reg is held by a wineserver and would be overwritten on shutdown.
Either shut the prefix down (wineserver -k, or close the game) and retry,
or pass --wine "<command that runs wine for this prefix>" to register through `+"`reg add`"+` instead, e.g.
  --wine 'flatpak run --command=/path/to/runner/bin/wine --env=WINEPREFIX=<prefix> com.usebottles.bottles'
  --wine 'WINEPREFIX=<prefix> umu-run'`, processList(hits))
}

func processList(hits []Running) string {
	if len(hits) == 0 {
		return "no processes listed"
	}
	var parts []string
	for i, h := range hits {
		if i == 3 {
			parts = append(parts, fmt.Sprintf("+%d more", len(hits)-3))
			break
		}
		parts = append(parts, fmt.Sprintf("pid %d %s", h.PID, filepath.Base(h.Command)))
	}
	return strings.Join(parts, ", ")
}

// runWine runs `<user's wine command> <args...>`.
//
// It goes through /bin/sh because the commands users have to supply are shell
// shaped — `flatpak run --command=... com.usebottles.bottles`, or an env
// assignment in front of umu-run — and we cannot know how to split them. Our
// own arguments are single-quoted, so registry paths and titles survive.
// WINEPREFIX is exported for commands that rely on the environment (a Flatpak
// invocation needs its own --env=, which is the user's business).
func runWine(wineCmd, prefix string, args ...string) (string, error) {
	if strings.TrimSpace(wineCmd) == "" {
		return "", errors.New("no --wine command given")
	}
	var b strings.Builder
	b.WriteString(wineCmd)
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", b.String())
	cmd.Env = append(os.Environ(), "WINEPREFIX="+CleanPrefix(prefix))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("wine command failed: %w\n  %s", err, b.String())
	}
	return string(out), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// FindAgentExe resolves the built agent binary: the explicit path if given,
// else the usual spots next to the clonecast binary and in the repo's bin/.
func FindAgentExe(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--exe %s: %w", explicit, err)
		}
		return explicit, nil
	}
	var candidates []string
	if self, err := os.Executable(); err == nil {
		dir := filepath.Dir(self)
		candidates = append(candidates, filepath.Join(dir, ExeName), filepath.Join(dir, "bin", ExeName))
	}
	candidates = append(candidates, ExeName, filepath.Join("bin", ExeName))
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			abs, err := filepath.Abs(c)
			if err != nil {
				return c, nil
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("could not find %s (looked in %s); build it with "+
		"`GOOS=windows GOARCH=386 CGO_ENABLED=0 go build -o bin/%s ./cmd/clonecast-agent` or pass --exe",
		ExeName, strings.Join(candidates, ", "), ExeName)
}

// copyFile copies src over dst, creating the directory. The destination is
// replaced through a temp file so a running agent's exe is never written in
// place (Wine, like Windows, keeps the image mapped).
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".clonecast-agent*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o755); err != nil {
		return err
	}
	return os.Rename(name, dst)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
