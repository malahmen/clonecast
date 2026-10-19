package main

import (
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
