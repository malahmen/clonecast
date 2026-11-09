package prefix

import (
	"path/filepath"
	"testing"

	"github.com/malahmen/clonecast/internal/agentwire"
)

// The agent derives its prefix from where install puts it (Kamino 276), with
// its own copy of the location so it does not link this package. This keeps
// the two in step: what ExeDest writes is what PrefixFromExe takes apart.
func TestAgentDerivesThePrefixInstallUsed(t *testing.T) {
	if InstallDirWin != `C:\`+agentwire.InstallDirName || ExeName != agentwire.AgentExeName {
		t.Fatalf("install location %s\\%s and agentwire's %s/%s differ",
			InstallDirWin, ExeName, agentwire.InstallDirName, agentwire.AgentExeName)
	}
	pfx := filepath.Join(t.TempDir(), "pfx")
	if got := agentwire.PrefixFromExe(filepath.ToSlash(ExeDest(pfx))); !SamePrefix(got, pfx) {
		t.Errorf("PrefixFromExe(ExeDest(%s)) = %q", pfx, got)
	}
}
