package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The desk this was found on: an ASUS keyboard with two nodes (only event6
// carries typing), keyd's virtual keyboard, and event numbers that move.
var desk = []candidate{
	{Path: "/dev/input/event6", Name: "ASUS ASUS Strix Tactic Pro Gaming Keyboard", State: "free",
		ID: "/dev/input/by-id/usb-ASUS_ASUS_Strix_Tactic_Pro_Gaming_Keyboard-event-kbd"},
	{Path: "/dev/input/event10", Name: "ASUS ASUS Strix Tactic Pro Gaming Keyboard", State: "free",
		ID: "/dev/input/by-id/usb-ASUS_ASUS_Strix_Tactic_Pro_Gaming_Keyboard-if01-event-kbd"},
	{Path: "/dev/input/event19", Name: "keyd virtual keyboard", State: "free", ID: "name:keyd virtual keyboard"},
}

func TestResolveIDs(t *testing.T) {
	cases := []struct {
		name   string
		found  []candidate
		ids    []string
		want   []string
		whyHas string
	}{
		{"by-id link", desk, []string{desk[0].ID}, []string{"/dev/input/event6"}, ""},
		{"name", desk, []string{"name:keyd virtual keyboard"}, []string{"/dev/input/event19"}, ""},
		{"several", desk, []string{desk[0].ID, "name:keyd virtual keyboard"}, []string{"/dev/input/event6", "/dev/input/event19"}, ""},
		// What was saved before IDs: trusted, it grabbed the wrong node.
		{"a bare event number is never trusted", desk, []string{"/dev/input/event10"}, nil, "device number"},
		{"gone", desk[2:], []string{desk[0].ID}, nil, "is gone"},
		// Two devices with one name and no by-id link: asking beats guessing.
		{"ambiguous name", []candidate{
			{Path: "/dev/input/event3", ID: "name:Lofree"}, {Path: "/dev/input/event4", ID: "name:Lofree"},
		}, []string{"name:Lofree"}, nil, "matches 2 devices"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := resolveIDs(c.found, c.ids)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("paths = %v, want %v (why %q)", got, c.want, why)
			}
			if c.whyHas == "" && why != "" || c.whyHas != "" && !strings.Contains(why, c.whyHas) {
				t.Fatalf("why = %q, want it to mention %q", why, c.whyHas)
			}
		})
	}
}

func TestIDsForSavesStableNamesNotNumbers(t *testing.T) {
	got := idsFor(desk, []string{"/dev/input/event6", "/dev/input/event19", "/dev/input/event99"})
	want := []string{desk[0].ID, "name:keyd virtual keyboard", "/dev/input/event99"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("idsFor = %v, want %v", got, want)
	}
}

// A choice saved by ID survives the numbers moving: the same by-id link now
// points at event7, and that is what gets grabbed.
func TestSavedIDFollowsARenumberedDevice(t *testing.T) {
	saved := idsFor(desk, []string{"/dev/input/event6"})
	moved := []candidate{desk[1], {Path: "/dev/input/event7", ID: desk[0].ID, Name: desk[0].Name, State: "free"}}
	got, why := resolveIDs(moved, saved)
	if why != "" || !reflect.DeepEqual(got, []string{"/dev/input/event7"}) {
		t.Fatalf("resolveIDs = %v (why %q), want [/dev/input/event7]", got, why)
	}
}

// Kamino 202: configSet replaces the file by rename, never by rewriting it in
// place, so a crash or a full disk mid-write cannot leave half a config. The
// lines it does not own and the file's mode survive.
func TestConfigSetReplacesTheFileWhole(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("# mine\nKEYBOARD=old\nOTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil { // past the umask
		t.Fatal(err)
	}
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := configSet(kbdKey, "new"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the config was rewritten in place, not replaced by rename")
	}
	if after.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the file's own 0600 kept", after.Mode().Perm())
	}
	b, _ := os.ReadFile(p)
	if string(b) != "# mine\n"+kbdKey+"=new\nOTHER=1\n" {
		t.Errorf("config = %q", b)
	}
	left, _ := os.ReadDir(filepath.Dir(p))
	if len(left) != 1 {
		t.Errorf("the directory holds %d files, want just the config (no temp left)", len(left))
	}

	// A first write creates the file with the default mode.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := configSet(kbdKey, "x"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(p); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("new config: %v, %v; want mode 0644", info, err)
	}
}
