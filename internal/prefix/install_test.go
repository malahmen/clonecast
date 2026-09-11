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
