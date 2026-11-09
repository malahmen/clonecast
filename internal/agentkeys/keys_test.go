package agentkeys

import (
	"testing"

	"github.com/malahmen/clonecast/internal/agentwire"
)

const (
	codeA     = 30  // KEY_A, scancode 0x1E
	codeUp    = 103 // KEY_UP, E0 48
	codePause = 119 // not mapped
	bit30     = 1 << 30
	bit31     = 1 << 31
)

func frame(t *testing.T, s *State, code uint16, down bool) Msg {
	t.Helper()
	m, ok := s.Frame(agentwire.Frame{Code: code, Down: down})
	if !ok {
		t.Fatalf("code %d not mapped", code)
	}
	return m
}

// The lParam layout Windows documents for WM_KEYDOWN/WM_KEYUP: repeat count in
// bits 0-15, scancode in 16-23, extended in 24, previous state in 30,
// transition in 31.
func TestLParam(t *testing.T) {
	cases := []struct {
		name             string
		scan             uint16
		ext, wasDown, up bool
		want             uint32
	}{
		{"press", 0x1E, false, false, false, 0x001E0001},
		{"repeat", 0x1E, false, true, false, 0x401E0001},
		{"release", 0x1E, false, true, true, 0xC01E0001},
		{"release of a key not seen down", 0x1E, false, false, true, 0xC01E0001},
		{"extended press", 0x48, true, false, false, 0x01480001},
		{"extended release", 0x48, true, true, true, 0xC1480001},
	}
	for _, c := range cases {
		if got := LParam(c.scan, c.ext, c.wasDown, c.up); got != c.want {
			t.Errorf("%s: LParam = 0x%08X, want 0x%08X", c.name, got, c.want)
		}
	}
}

// clonecast sends a held key's repeats as more Downs. The first is a press
// (bit 30 clear); every later one is a repeat (bit 30 set), as Windows posts
// them; the Up has bits 30 and 31 (Kamino 136).
func TestRepeatsSetThePreviousStateBit(t *testing.T) {
	var s State
	press := frame(t, &s, codeA, true)
	if press.Msg != WMKeyDown || press.VK != 0x41 || press.LParam&bit30 != 0 {
		t.Fatalf("first Down = %+v, want WM_KEYDOWN 'A' with bit 30 clear", press)
	}
	for i := 0; i < 3; i++ {
		rep := frame(t, &s, codeA, true)
		if rep.Msg != WMKeyDown || rep.LParam&bit30 == 0 {
			t.Fatalf("repeat %d = %+v (lParam 0x%08X), want WM_KEYDOWN with bit 30 set", i, rep, rep.LParam)
		}
		if rep.LParam&0xFFFF != 1 {
			t.Errorf("repeat %d: repeat count %d, want 1 (one message per repeat)", i, rep.LParam&0xFFFF)
		}
	}
	up := frame(t, &s, codeA, false)
	if up.Msg != WMKeyUp || up.LParam&(bit30|bit31) != bit30|bit31 {
		t.Fatalf("Up = %+v (lParam 0x%08X), want WM_KEYUP with bits 30 and 31", up, up.LParam)
	}
	// After the Up the key is a fresh press again.
	if again := frame(t, &s, codeA, true); again.LParam&bit30 != 0 {
		t.Errorf("Down after the Up has bit 30 set (0x%08X); it is a new press", again.LParam)
	}
}

// The state is per code: holding one key does not make another's press look
// like a repeat.
func TestRepeatStateIsPerCode(t *testing.T) {
	var s State
	frame(t, &s, codeA, true)
	if m := frame(t, &s, codeUp, true); m.LParam&bit30 != 0 {
		t.Errorf("UP's first Down has bit 30 while only A is held (0x%08X)", m.LParam)
	}
}

// When the session ends, every key still down gets its Up, with the same
// scancode and extended bit a real release would carry; released keys don't,
// and a second Release has nothing left (Kamino 136).
func TestReleaseUpsEveryHeldKey(t *testing.T) {
	var s State
	frame(t, &s, codeUp, true)
	frame(t, &s, codeA, true)
	frame(t, &s, codeA, true) // a repeat: still one key
	frame(t, &s, 31, true)    // S
	frame(t, &s, 31, false)   // released: not in the list

	got := s.Release()
	want := []Msg{
		{Msg: WMKeyUp, VK: 0x41, LParam: 0xC01E0001, Code: codeA},
		{Msg: WMKeyUp, VK: 0x26, LParam: 0xC1480001, Code: codeUp},
	}
	if len(got) != len(want) {
		t.Fatalf("Release = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Release[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if s.Held(codeA) || s.Held(codeUp) {
		t.Error("keys still held after Release")
	}
	if again := s.Release(); len(again) != 0 {
		t.Errorf("second Release = %+v, want nothing", again)
	}
}

// An unmappable code is refused and leaves no state behind to release.
func TestUnmappedCodeChangesNothing(t *testing.T) {
	var s State
	if _, ok := s.Frame(agentwire.Frame{Code: codePause, Down: true}); ok {
		t.Fatal("PAUSE mapped; pick another unmapped code for this test")
	}
	if s.Held(codePause) {
		t.Error("an unmapped Down was recorded as held")
	}
	if got := s.Release(); len(got) != 0 {
		t.Errorf("Release after an unmapped Down = %+v, want nothing", got)
	}
}
