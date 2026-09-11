package prefix

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A realistic slice of a Wine system.reg: the version banner, the ";;" comment,
// #arch, a key with a class, dword and hex values, a hex value continued over
// several lines, an existing RunServices key with someone else's entry, and a
// key that sorts after it.
const sampleReg = `WINE REGISTRY Version 2
;; All keys relative to \\Machine

#arch=win64

[Software\\Classes\\.bat] 1580000000
#time=1d5c0a1b2c3d4e5
@="batfile"
"Content Type"="application/x-bat"

[Software\\Microsoft\\Windows\\CurrentVersion] 1580000001
#time=1d5c0a1b2c3d4e6
#class="cls"
"ProgramFilesDir"="C:\\Program Files"
"CommonFilesDir"="C:\\Program Files\\Common Files"
"Version"=dword:0000000a
"Blob"=hex:11,22,33,44,55,66,77,88,99,aa,bb,cc,dd,ee,ff,00,11,22,33,44,55,66,\
  77,88,99,aa,bb,cc,dd,ee,ff

[Software\\Microsoft\\Windows\\CurrentVersion\\RunServices] 1580000002
#time=1d5c0a1b2c3d4e7
"winemenubuilder"="C:\\windows\\system32\\winemenubuilder.exe -a -r"

[Software\\Wine\\Drivers] 1580000003
#time=1d5c0a1b2c3d4e8
"Audio"="pulse"
`

func loadSample(t *testing.T) (*RegFile, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "system.reg")
	if err := os.WriteFile(path, []byte(sampleReg), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := LoadRegFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return f, path
}

func TestParseRoundTripsUnchanged(t *testing.T) {
	f, _ := loadSample(t)
	if got := f.String(); got != sampleReg {
		t.Fatalf("round-trip changed the file:\n--- got ---\n%s\n--- want ---\n%s", got, sampleReg)
	}
	if f.Arch != "win64" {
		t.Errorf("arch = %q, want win64", f.Arch)
	}
	if len(f.blocks) != 4 {
		t.Errorf("parsed %d key blocks, want 4", len(f.blocks))
	}
}

func TestSetStringIntoExistingKey(t *testing.T) {
	f, path := loadSample(t)
	cmd := `C:\clonecast\clonecast-agent.exe -port 48900 -title "World of Warcraft"`
	if !f.SetString(RunServicesKey, DefaultValueName, cmd) {
		t.Fatal("SetString reported no change")
	}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}

	// The value reads back through a fresh parse, escapes and all.
	reread, err := LoadRegFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reread.GetString(RunServicesKey, DefaultValueName)
	if !ok {
		t.Fatal("value not found after save")
	}
	if got != cmd {
		t.Errorf("value = %q, want %q", got, cmd)
	}

	// On disk it is a properly escaped single line inside the RunServices key.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantLine := `"clonecast-agent"="C:\\clonecast\\clonecast-agent.exe -port 48900 -title \"World of Warcraft\""`
	if !strings.Contains(string(raw), wantLine) {
		t.Errorf("system.reg does not contain the expected escaped line %s\ngot:\n%s", wantLine, raw)
	}

	// Nothing else moved: every original line survives, in order, and the only
	// difference is the one inserted line.
	diff := added(string(raw), sampleReg)
	if len(diff) != 1 || strings.TrimSpace(diff[0]) != wantLine {
		t.Errorf("unexpected changes to the file: %q", diff)
	}
	// The neighbouring keys and their values are intact.
	for _, want := range []struct{ key, name, data string }{
		{`Software\Classes\.bat`, "Content Type", "application/x-bat"},
		{`Software\Microsoft\Windows\CurrentVersion`, "ProgramFilesDir", `C:\Program Files`},
		{`Software\Microsoft\Windows\CurrentVersion\RunServices`, "winemenubuilder", `C:\windows\system32\winemenubuilder.exe -a -r`},
		{`Software\Wine\Drivers`, "Audio", "pulse"},
	} {
		got, ok := reread.GetString(want.key, want.name)
		if !ok || got != want.data {
			t.Errorf("%s\\%s = %q (found %v), want %q", want.key, want.name, got, ok, want.data)
		}
	}
	// The hex continuation lines are still attached to their key.
	if !strings.Contains(string(raw), "77,88,99,aa,bb,cc,dd,ee,ff\n") {
		t.Error("hex continuation line lost")
	}
	// A backup of the original was kept.
	bak, err := os.ReadFile(path + ".clonecast.bak")
	if err != nil || string(bak) != sampleReg {
		t.Errorf("backup missing or wrong (err=%v)", err)
	}
}

func TestSetStringCreatesMissingKey(t *testing.T) {
	// A prefix that has never had RunServices (Wine only creates keys lazily).
	stripped := strings.Replace(sampleReg, `[Software\\Microsoft\\Windows\\CurrentVersion\\RunServices] 1580000002
#time=1d5c0a1b2c3d4e7
"winemenubuilder"="C:\\windows\\system32\\winemenubuilder.exe -a -r"
`, "", 1)
	f := ParseRegFile("system.reg", stripped)
	if _, ok := f.GetString(RunServicesKey, DefaultValueName); ok {
		t.Fatal("value present before install")
	}
	if !f.SetString(RunServicesKey, DefaultValueName, `C:\clonecast\clonecast-agent.exe`) {
		t.Fatal("SetString reported no change")
	}
	outText := f.String()
	if !strings.Contains(outText, `[Software\\Microsoft\\Windows\\CurrentVersion\\RunServices] `) {
		t.Errorf("new key block not written in Wine's bracket form:\n%s", outText)
	}
	if !strings.Contains(outText, "\n#time=") {
		t.Error("new key block has no #time= line")
	}
	// Re-parsing the result finds the value, i.e. what we wrote is what we read.
	if got, ok := ParseRegFile("system.reg", outText).GetString(RunServicesKey, DefaultValueName); !ok || got != `C:\clonecast\clonecast-agent.exe` {
		t.Errorf("re-parsed value = %q (found %v)", got, ok)
	}
	// The untouched part of the file is unchanged.
	if !strings.HasPrefix(outText, stripped) {
		t.Error("the new key was not appended cleanly at the end")
	}
}

func TestSetStringIsIdempotentAndReplaces(t *testing.T) {
	f, _ := loadSample(t)
	f.SetString(RunServicesKey, DefaultValueName, "A")
	before := f.String()
	if f.SetString(RunServicesKey, DefaultValueName, "A") {
		t.Error("second identical SetString reported a change")
	}
	if f.String() != before {
		t.Error("identical SetString rewrote the file")
	}
	if !f.SetString(RunServicesKey, DefaultValueName, "B") {
		t.Error("changing the data reported no change")
	}
	if got, _ := f.GetString(RunServicesKey, DefaultValueName); got != "B" {
		t.Errorf("value = %q, want B", got)
	}
	if strings.Count(f.String(), `"clonecast-agent"=`) != 1 {
		t.Error("value duplicated instead of replaced")
	}
}

func TestDeleteValueLeavesTheRest(t *testing.T) {
	f, _ := loadSample(t)
	f.SetString(RunServicesKey, DefaultValueName, "X")
	if !f.DeleteValue(RunServicesKey, DefaultValueName) {
		t.Fatal("DeleteValue reported nothing removed")
	}
	if f.String() != sampleReg {
		t.Errorf("delete did not restore the original file:\n%s", f.String())
	}
	if f.DeleteValue(RunServicesKey, DefaultValueName) {
		t.Error("second delete reported a removal")
	}
	if f.DeleteValue(`Software\Nope`, "x") {
		t.Error("delete on a missing key reported a removal")
	}
}

func TestCaseInsensitiveKeyAndValueMatching(t *testing.T) {
	f, _ := loadSample(t)
	// Wine matches keys and value names case-insensitively; an install must
	// not create a second RunServices key just because the case differs.
	f.SetString(strings.ToUpper(RunServicesKey), "CLONECAST-AGENT", "Y")
	if strings.Count(f.String(), "RunServices] ") != 1 {
		t.Error("a second RunServices key was created")
	}
	if got, ok := f.GetString(RunServicesKey, DefaultValueName); !ok || got != "Y" {
		t.Errorf("value = %q (found %v), want Y", got, ok)
	}
}

func TestEscapeRoundTrip(t *testing.T) {
	for _, s := range []string{
		`C:\clonecast\clonecast-agent.exe`,
		`a "quoted" thing`,
		`back\\slashes`,
		"tab\there",
		"café ☕",
		"",
	} {
		got, ok := unquoteRegString(quoteRegString(s))
		if !ok || got != s {
			t.Errorf("round trip of %q gave %q (ok=%v) via %s", s, got, ok, quoteRegString(s))
		}
	}
}

func TestGetStringIgnoresNonStringValues(t *testing.T) {
	f, _ := loadSample(t)
	key := `Software\Microsoft\Windows\CurrentVersion`
	if _, ok := f.GetString(key, "Version"); ok {
		t.Error("a dword: value was reported as a string")
	}
	if _, ok := f.GetString(key, "Blob"); ok {
		t.Error("a hex: value was reported as a string")
	}
	if got, ok := f.GetString(`Software\Classes\.bat`, ""); !ok || got != "batfile" {
		t.Errorf(`default value ("@=") = %q (found %v), want batfile`, got, ok)
	}
}

// added returns the lines present in got that are not in want, in order.
func added(got, want string) []string {
	have := map[string]int{}
	for _, l := range strings.Split(want, "\n") {
		have[l]++
	}
	var extra []string
	for _, l := range strings.Split(got, "\n") {
		if have[l] > 0 {
			have[l]--
			continue
		}
		extra = append(extra, l)
	}
	return extra
}
