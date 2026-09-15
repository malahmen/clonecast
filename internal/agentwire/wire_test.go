package agentwire

import (
	"errors"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	for _, want := range []Frame{
		{Code: 57, Down: true},     // space
		{Code: 57, Down: false},    // space up
		{Code: 0, Down: true},      // reserved, still a valid code
		{Code: 65535, Down: false}, // the top of the uint16 range
	} {
		var b strings.Builder
		if err := want.Write(&b); err != nil {
			t.Fatalf("write %v: %v", want, err)
		}
		line := strings.TrimSuffix(b.String(), "\n")
		if strings.Contains(line, "\n") {
			t.Fatalf("frame %v encoded to more than one line: %q", want, b.String())
		}
		msg, err := Decode(line)
		if err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		if msg.Kind != KindFrame {
			t.Fatalf("decode %q: kind = %v, want frame", line, msg.Kind)
		}
		if msg.Frame != want {
			t.Errorf("decode %q = %+v, want %+v", line, msg.Frame, want)
		}
	}
}

func TestHelloRoundTrip(t *testing.T) {
	want := Hello{
		Version: Version,
		// Spaces, '=' and non-ASCII in the values are the whole reason the
		// fields are escaped: a real WoW window title and a Bottles prefix
		// path both hit this.
		Prefix: "/home/u/.var/app/com.usebottles.bottles/data/bottles/bottles/Malahmen ção",
		Title:  "World of Warcraft = the game",
		Exe:    `C:\clonecast\clonecast-agent.exe`,
		PID:    1234,
		HWND:   0xdeadbeef,
	}
	var b strings.Builder
	if err := want.Write(&b); err != nil {
		t.Fatalf("write: %v", err)
	}
	line := strings.TrimSuffix(b.String(), "\n")
	// "H <ver>" plus five fields, none of which may contain a raw space.
	if strings.Count(line, " ") != 6 {
		t.Fatalf("hello line has unexpected spacing: %q", line)
	}
	msg, err := Decode(line)
	if err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	if msg.Kind != KindHello {
		t.Fatalf("kind = %v, want hello", msg.Kind)
	}
	if msg.Hello != want {
		t.Errorf("decode = %+v\nwant     %+v", msg.Hello, want)
	}
}

func TestHelloOmitsEmptyFields(t *testing.T) {
	var b strings.Builder
	if err := (Hello{Title: "Game"}).Write(&b); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := strings.TrimSpace(b.String())
	if got != "H 2 title=Game" {
		t.Fatalf("hello = %q, want %q", got, "H 2 title=Game")
	}
	msg, err := Decode(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Hello.Version != Version || msg.Hello.Title != "Game" {
		t.Fatalf("decode = %+v", msg.Hello)
	}
}

func TestDecodeKinds(t *testing.T) {
	for _, tc := range []struct {
		line string
		want Kind
	}{
		{"K", KindKeepalive},
		{"P", KindPing},
		{"", KindUnknown},
		{"   ", KindUnknown},
		{"F 1", KindUnknown},        // a future foreground report: ignored, not an error
		{"H 2 future=x", KindHello}, // an unknown hello field: ignored
	} {
		msg, err := Decode(tc.line)
		if err != nil {
			t.Errorf("decode %q: unexpected error %v", tc.line, err)
			continue
		}
		if msg.Kind != tc.want {
			t.Errorf("decode %q: kind = %v, want %v", tc.line, msg.Kind, tc.want)
		}
	}
}

func TestDecodeMalformed(t *testing.T) {
	for _, line := range []string{
		"D",             // no code
		"D 1 2",         // too many fields
		"U banana",      // non-numeric code
		"D 70000",       // out of uint16 range
		"H",             // no version
		"H two",         // non-numeric version
		"H 2 prefix",    // field without '='
		"H 2 pid=x",     // non-numeric pid
		"H 2 hwnd=zz",   // non-hex hwnd
		"H 2 title=%zz", // bad percent escape
	} {
		if _, err := Decode(line); err == nil {
			t.Errorf("decode %q: want an error, got none", line)
		}
	}
}

func TestDecodeVersionMismatch(t *testing.T) {
	msg, err := Decode("H 99 prefix=x")
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("decode: err = %v, want ErrUnsupportedVersion", err)
	}
	_ = msg
	if _, err := Decode("H 1 prefix=x"); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("v1 hello: err = %v, want ErrUnsupportedVersion", err)
	}
}

// TestReadSkipsMalformed is the property the agent's frame loop depends on:
// one corrupt line must not cost the connection and every keystroke after it.
func TestReadSkipsMalformed(t *testing.T) {
	stream := strings.Join([]string{
		"H 2 title=Game",
		"D 57",
		"D banana", // malformed: skipped
		"",         // blank: skipped
		"Z zzz",    // unknown kind: skipped
		"U 57",
		"K",
	}, "\n") + "\n"

	var got []Message
	if err := Read(strings.NewReader(stream), func(m Message) { got = append(got, m) }); err != nil {
		t.Fatalf("read: %v", err)
	}
	want := []Kind{KindHello, KindFrame, KindFrame, KindKeepalive}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(got), len(want), got)
	}
	for i, k := range want {
		if got[i].Kind != k {
			t.Errorf("message %d: kind = %v, want %v", i, got[i].Kind, k)
		}
	}
	if got[1].Frame != (Frame{Code: 57, Down: true}) || got[2].Frame != (Frame{Code: 57}) {
		t.Errorf("frames decoded wrong: %+v %+v", got[1].Frame, got[2].Frame)
	}
}

func TestKeepaliveAndPingWrite(t *testing.T) {
	var b strings.Builder
	if err := WriteKeepalive(&b); err != nil {
		t.Fatal(err)
	}
	if err := WritePing(&b); err != nil {
		t.Fatal(err)
	}
	if b.String() != "K\nP\n" {
		t.Fatalf("got %q, want %q", b.String(), "K\nP\n")
	}
}
