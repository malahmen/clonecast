package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/malahmen/clonecast/internal/platform/agent"
	"github.com/malahmen/clonecast/internal/tui"
)

// The registry is what the TUI's "agent proto" row changes (Kamino 275).
var _ tui.AgentProtocol = (*agent.Registry)(nil)

// installPrefix is a minimal prefix and a fake agent exe for `agent install`.
func installPrefix(t *testing.T) (pfx, exe string) {
	t.Helper()
	pfx = t.TempDir()
	if err := os.MkdirAll(filepath.Join(pfx, "drive_c", "windows"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg := "WINE REGISTRY Version 2\n;; All keys relative to \\\\Machine\n\n#arch=win64\n"
	if err := os.WriteFile(filepath.Join(pfx, "system.reg"), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(t.TempDir(), "clonecast-agent.exe")
	if err := os.WriteFile(exe, []byte("MZ fake agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	return pfx, exe
}

// `clonecast agent install --protocol 2` writes -protocol 2 into the
// autostart entry; the default, 3, writes nothing; anything else is refused.
func TestAgentInstallProtocol(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, c := range []struct {
		args    []string
		want    bool // -protocol 2 in the command
		wantErr bool
	}{
		{nil, false, false},
		{[]string{"--protocol", "3"}, false, false},
		{[]string{"--protocol", "2"}, true, false},
		{[]string{"--protocol", "4"}, false, true},
	} {
		pfx, exe := installPrefix(t)
		var out bytes.Buffer
		args := append([]string{"--prefix", pfx, "--exe", exe, "--route", "file", "--assume-idle"}, c.args...)
		err := agentInstall(args, &out)
		if c.wantErr {
			if err == nil || !strings.Contains(err.Error(), "--protocol") {
				t.Errorf("%v: err = %v, want --protocol refused", c.args, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%v: %v", c.args, err)
		}
		if got := strings.Contains(out.String(), "-protocol 2"); got != c.want || strings.Contains(out.String(), "-protocol 3") {
			t.Errorf("%v: output:\n%s", c.args, out.String())
		}
	}
}
