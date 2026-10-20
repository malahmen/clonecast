package prefix

import (
	"os"
	"path/filepath"
	"testing"
)

func runnerWith(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "runner", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		mode := os.FileMode(0o755)
		if n == "wine-notexec" {
			n, mode = "wine", 0o644
		}
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func serverPath(dir string) string {
	// The form Bottles actually launches, so the cleaning is exercised.
	return filepath.Join(dir, "runner", "lib", "wine", "..", "..", "bin", "wineserver")
}

func TestWineCmdFromProcesses(t *testing.T) {
	t.Run("derives the sibling wine", func(t *testing.T) {
		dir := runnerWith(t, "wine", "wineserver")
		p := Proc{Processes: []Running{
			{Command: `C:\windows\system32\services.exe`}, // ignored: not a host path
			{Command: serverPath(dir)},
		}}
		want := filepath.Join(dir, "runner", "bin", "wine")
		if got := WineCmdFromProcesses(p); got != want {
			t.Fatalf("WineCmdFromProcesses = %q, want %q", got, want)
		}
	})

	t.Run("no wineserver means no answer", func(t *testing.T) {
		p := Proc{Processes: []Running{{Command: "/usr/bin/WoW.exe"}}}
		if got := WineCmdFromProcesses(p); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("wineserver without a sibling wine", func(t *testing.T) {
		dir := runnerWith(t, "wineserver")
		p := Proc{Processes: []Running{{Command: serverPath(dir)}}}
		if got := WineCmdFromProcesses(p); got != "" {
			t.Fatalf("got %q, want empty when wine is absent", got)
		}
	})

	t.Run("a wine that is not executable is not an answer", func(t *testing.T) {
		dir := runnerWith(t, "wineserver", "wine-notexec")
		p := Proc{Processes: []Running{{Command: serverPath(dir)}}}
		if got := WineCmdFromProcesses(p); got != "" {
			t.Fatalf("got %q, want empty when wine is not executable", got)
		}
	})

	// Traced 2026-10-20: the Bottles sandbox has its own /tmp, so the
	// runner's wine run on the host started a second wineserver for the
	// prefix instead of reaching the game's.
	t.Run("a sandboxed wineserver gets its wine through flatpak", func(t *testing.T) {
		dir := runnerWith(t, "wine", "wineserver")
		procRoot = t.TempDir()
		t.Cleanup(func() { procRoot = "/proc" })
		root := filepath.Join(procRoot, "4242", "root")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		info := "[Application]\nname=com.usebottles.bottles\nruntime=runtime/org.gnome.Platform\n\n[Instance]\ninstance-id=1\n"
		if err := os.WriteFile(filepath.Join(root, ".flatpak-info"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
		p := Proc{Prefix: "/bottles/dp-Marx", Processes: []Running{{PID: 4242, Command: serverPath(dir)}}}
		want := "flatpak run --command='" + filepath.Join(dir, "runner", "bin", "wine") +
			"' --env=WINEPREFIX='/bottles/dp-Marx' com.usebottles.bottles"
		if got := WineCmdFromProcesses(p); got != want {
			t.Fatalf("WineCmdFromProcesses =\n %q\nwant\n %q", got, want)
		}
	})

	t.Run("an idle prefix has nothing to read", func(t *testing.T) {
		if got := WineCmdFromProcesses(Proc{}); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func TestProcGameSkipsWineInfrastructure(t *testing.T) {
	p := Proc{Processes: []Running{
		{Command: "/x/bin/wineserver"},
		{Command: `C:\windows\system32\services.exe`},
		{Command: `C:\windows\system32\explorer.exe`},
		{Command: "/home/u/client/WoW.exe"},
	}}
	if got := p.Game(); got != "WoW.exe" {
		t.Fatalf("Game() = %q, want WoW.exe", got)
	}
	if !p.Running() {
		t.Fatal("Running() = false with live processes")
	}
	if got := (Proc{}).Game(); got != "" {
		t.Fatalf("Game() on an idle prefix = %q, want empty", got)
	}
}
