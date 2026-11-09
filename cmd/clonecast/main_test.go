package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/malahmen/clonecast/internal/broadcast"
	"github.com/malahmen/clonecast/internal/keys"
)

// Kamino 202, the owner's decision: --toggle takes any key, but a key other
// than the default is taken from every window, so it needs a yes typed on the
// terminal, and with no terminal it is refused.
func TestConfirmToggle(t *testing.T) {
	def := broadcast.DefaultConfig().ToggleKey
	if keys.Name(def) != "SCROLLLOCK" {
		t.Fatalf("default toggle = %s, want SCROLLLOCK", keys.Name(def))
	}
	enter, err := keys.Parse("ENTER")
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := confirmToggle(def, def, strings.NewReader(""), &out, false); err != nil || out.Len() != 0 {
		t.Fatalf("the default asked or refused: %v, %q", err, out.String())
	}
	for _, yes := range []string{"y\n", "Yes\n", " Y \n"} {
		out.Reset()
		if err := confirmToggle(enter, def, strings.NewReader(yes), &out, true); err != nil {
			t.Fatalf("%q refused: %v", yes, err)
		}
		if !strings.Contains(out.String(), "ENTER") || !strings.Contains(out.String(), "[y/N]") {
			t.Fatalf("prompt = %q", out.String())
		}
	}
	for _, no := range []string{"\n", "n\n", "", "sure\n"} {
		if err := confirmToggle(enter, def, strings.NewReader(no), &out, true); err == nil {
			t.Fatalf("%q taken as a yes", no)
		}
	}
	out.Reset()
	err = confirmToggle(enter, def, strings.NewReader("y\n"), &out, false)
	if err == nil || !strings.Contains(err.Error(), "terminal") || out.Len() != 0 {
		t.Fatalf("no terminal: %v, prompt %q; want a refusal saying how to confirm", err, out.String())
	}
}
