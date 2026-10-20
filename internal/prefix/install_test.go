package prefix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePrefix builds a directory that passes Validate: system.reg plus drive_c.
func fakePrefix(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "drive_c", "windows"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "system.reg"), []byte(sampleReg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeExe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ExeName)
	if err := os.WriteFile(p, []byte("MZ fake agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// The offline route is exercised with AssumeIdle because the "is this prefix
// booted?" check reads /proc, which the macOS development host does not have.
// On Linux the same path runs with the real check; see discover_linux.go.
func TestInstallAndUninstallOffline(t *testing.T) {
	pfx := fakePrefix(t)
	cfg := Config{
		Prefix:     pfx,
		ExePath:    fakeExe(t),
		Port:       48901,
		Title:      "World of Warcraft",
		Route:      RouteFile,
		AssumeIdle: true,
	}

	res, err := Install(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Route != RouteFile {
		t.Errorf("route = %q, want %q", res.Route, RouteFile)
	}
	if !res.Changed {
		t.Error("install reported no registry change")
	}
	if res.WinPath != `C:\clonecast\clonecast-agent.exe` {
		t.Errorf("windows path = %q", res.WinPath)
	}
	want := `C:\clonecast\clonecast-agent.exe -port 48901 -title "World of Warcraft"`
	if res.Command != want {
		t.Errorf("command = %q, want %q", res.Command, want)
	}

	// The binary landed under drive_c (never on Z:, which a Flatpak bottle
	// cannot see for $HOME).
	dest := filepath.Join(pfx, "drive_c", "clonecast", ExeName)
	if b, err := os.ReadFile(dest); err != nil || string(b) != "MZ fake agent" {
		t.Fatalf("agent not copied to %s: %v", dest, err)
	}

	st, err := Describe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != want {
		t.Errorf("status value = %q, want %q", st.Value, want)
	}
	if st.Exe != dest || st.ExeSize == 0 {
		t.Errorf("status exe = %q (%d bytes)", st.Exe, st.ExeSize)
	}
	if st.Arch != "win64" {
		t.Errorf("status arch = %q, want win64", st.Arch)
	}

	// Re-installing is idempotent.
	res2, err := Install(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Changed {
		t.Error("re-install reported a registry change")
	}

	if _, err := Uninstall(cfg); err != nil {
		t.Fatal(err)
	}
	st, err = Describe(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Value != "" {
		t.Errorf("value still present after uninstall: %q", st.Value)
	}
	if st.Exe != "" {
		t.Errorf("exe still present after uninstall: %q", st.Exe)
	}
	if _, err := os.Stat(filepath.Join(pfx, "drive_c", "clonecast")); !os.IsNotExist(err) {
		t.Error("empty install directory was left behind")
	}
	// Everything else in the prefix's registry survived the whole cycle.
	raw, err := os.ReadFile(filepath.Join(pfx, "system.reg"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sampleReg {
		t.Errorf("system.reg not restored to its original content:\n%s", raw)
	}
}

func TestCommandLineOmitsDefaults(t *testing.T) {
	if got := commandLine(Config{}); got != `C:\clonecast\clonecast-agent.exe` {
		t.Errorf("bare command = %q", got)
	}
	if got := commandLine(Config{Port: 48900}); got != `C:\clonecast\clonecast-agent.exe -port 48900` {
		t.Errorf("with port = %q", got)
	}
	if got := commandLine(Config{Title: `a "b"`}); !strings.HasSuffix(got, `-title "a \"b\""`) {
		t.Errorf("quoted title = %q", got)
	}
}

func TestValidateRejectsNonPrefixes(t *testing.T) {
	if err := Validate(""); err == nil {
		t.Error("empty prefix accepted")
	}
	if err := Validate(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("missing directory accepted")
	}
	bare := t.TempDir()
	if err := Validate(bare); err == nil {
		t.Error("directory without system.reg accepted")
	}
	if err := Validate(fakePrefix(t)); err != nil {
		t.Errorf("real-looking prefix rejected: %v", err)
	}
}

func TestRouteRegNeedsWineCommand(t *testing.T) {
	_, err := Install(Config{Prefix: fakePrefix(t), ExePath: fakeExe(t), Route: RouteReg})
	if err == nil || !strings.Contains(err.Error(), "--wine") {
		t.Errorf("error = %v, want a complaint about --wine", err)
	}
}

func TestUnknownRoute(t *testing.T) {
	_, err := Install(Config{Prefix: fakePrefix(t), ExePath: fakeExe(t), Route: "sideways"})
	if err == nil || !strings.Contains(err.Error(), "unknown --route") {
		t.Errorf("error = %v, want an unknown-route complaint", err)
	}
}

func TestFindAgentExeReportsWhereItLooked(t *testing.T) {
	if _, err := FindAgentExe(filepath.Join(t.TempDir(), "missing.exe")); err == nil {
		t.Error("explicit missing path accepted")
	}
	got := fakeExe(t)
	if p, err := FindAgentExe(got); err != nil || p != got {
		t.Errorf("explicit path = %q, %v", p, err)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:              `'plain'`,
		`HKLM\Some\Key`:      `'HKLM\Some\Key'`,
		`a "b" c`:            `'a "b" c'`,
		`it's`:               `'it'\''s'`,
		`$(rm -rf /) ; evil`: `'$(rm -rf /) ; evil'`,
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// argvW splits a command line the way CommandLineToArgvW does, for checking
// winArg against the reader that matters.
func argvW(cmd string) []string {
	var args []string
	var cur strings.Builder
	inQuote, started := false, false
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch {
		case c == '\\':
			n := 0
			for i < len(cmd) && cmd[i] == '\\' {
				n++
				i++
			}
			if i < len(cmd) && cmd[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
				} else {
					inQuote = !inQuote
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
				i--
			}
			started = true
		case c == '"':
			inQuote = !inQuote
			started = true
		case (c == ' ' || c == '\t') && !inQuote:
			if started {
				args = append(args, cur.String())
				cur.Reset()
				started = false
			}
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if started {
		args = append(args, cur.String())
	}
	return args
}

// Kamino item 41: a title ending in a backslash escaped the closing quote.
func TestCommandLineTitleSurvivesCommandLineToArgvW(t *testing.T) {
	for _, title := range []string{
		`World of Warcraft`,
		`ends in a backslash\`,
		`two at the end\\`,
		`a "quoted" word`,
		`slash before quote\"`,
		`C:\path\with spaces\`,
		``,
	} {
		cmd := commandLine(Config{Port: 48800, Title: title})
		args := argvW(cmd)
		if title == "" {
			if len(args) != 3 {
				t.Errorf("no title: %q", args)
			}
			continue
		}
		if len(args) != 5 || args[3] != "-title" || args[4] != title {
			t.Errorf("title %q: command %s reads back as %q", title, cmd, args)
		}
	}
}

// Kamino item 40: a failed registry import left the copied exe behind.
func TestInstallLeavesNothingWhenTheRegistryCannotBeRead(t *testing.T) {
	pfx := fakePrefix(t)
	reg := filepath.Join(pfx, "system.reg")
	if err := os.Chmod(reg, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(reg); err == nil {
		t.Skip("running as root: the file stays readable")
	}
	_, err := Install(Config{Prefix: pfx, ExePath: fakeExe(t), Route: RouteFile, AssumeIdle: true})
	if err == nil {
		t.Fatal("install succeeded with an unreadable system.reg")
	}
	if _, err := os.Stat(ExeDest(pfx)); !os.IsNotExist(err) {
		t.Errorf("the agent was left at %s: %v", ExeDest(pfx), err)
	}
	if _, err := os.Stat(filepath.Dir(ExeDest(pfx))); !os.IsNotExist(err) {
		t.Errorf("its directory was left behind: %v", err)
	}
}

// Kamino item 42: system.reg rewritten between reading and writing it (a
// wineserver flushing) must not be overwritten.
func TestSaveRefusesAFileChangedSinceItWasRead(t *testing.T) {
	pfx := fakePrefix(t)
	f, err := LoadRegFile(SystemReg(pfx))
	if err != nil {
		t.Fatal(err)
	}
	f.SetString(RunServicesKey, DefaultValueName, "X")
	flushed := sampleReg + "\n[Software\\\\Wine] 1\n"
	if err := os.WriteFile(SystemReg(pfx), []byte(flushed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Save(); err == nil {
		t.Fatal("Save overwrote a file rewritten since it was read")
	}
	if b, _ := os.ReadFile(SystemReg(pfx)); string(b) != flushed {
		t.Error("the rewritten file was changed")
	}
}

// Kamino item 2: the token sits beside the agent, readable by its owner only,
// and goes with it on uninstall.
func TestInstallWritesThePrivateTokenAndUninstallRemovesIt(t *testing.T) {
	pfx := fakePrefix(t)
	cfg := Config{Prefix: pfx, ExePath: fakeExe(t), Route: RouteFile, AssumeIdle: true, Token: "s3cret"}
	if _, err := Install(cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(TokenDest(pfx))
	if err != nil || string(b) != "s3cret" {
		t.Fatalf("token file = %q, %v", b, err)
	}
	if info, _ := os.Stat(TokenDest(pfx)); info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %o, want 600", info.Mode().Perm())
	}
	if res, _ := Install(cfg); strings.Contains(res.Command, "s3cret") {
		t.Error("the token is on the autostart command line, which any local user can read")
	}
	if _, err := Uninstall(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(TokenDest(pfx)); !os.IsNotExist(err) {
		t.Errorf("token left after uninstall: %v", err)
	}
}
