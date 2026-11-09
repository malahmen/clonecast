package agentwire

import "testing"

// TestWinKey pins the evdev -> Win32 table against the values Windows itself
// puts in WM_KEYDOWN (Kamino 185). The codes are linux/input-event-codes.h's.
// The extended column is what tells a game the right Ctrl from the left and
// an arrow from the numpad digit under it, so it is checked for every row.
func TestWinKey(t *testing.T) {
	cases := []struct {
		name string
		code uint16
		vk   uint16
		scan uint16
		ext  bool
	}{
		{"A", 30, 0x41, 0x1E, false},
		{"1", 2, 0x31, 0x02, false},
		{"0", 11, 0x30, 0x0B, false},
		{"SPACE", 57, 0x20, 0x39, false},
		{"ENTER", 28, 0x0D, 0x1C, false},
		{"F1", 59, 0x70, 0x3B, false},
		{"F12", 88, 0x7B, 0x58, false},
		{"CAPSLOCK", 58, 0x14, 0x3A, false},

		{"LEFTSHIFT", 42, 0x10, 0x2A, false},
		{"RIGHTSHIFT", 54, 0x10, 0x36, false}, // not extended on Windows
		{"LEFTCTRL", 29, 0x11, 0x1D, false},
		{"RIGHTCTRL", 97, 0x11, 0x1D, true},
		{"LEFTALT", 56, 0x12, 0x38, false},
		{"RIGHTALT", 100, 0x12, 0x38, true},

		{"MINUS", 12, 0xBD, 0x0C, false},
		{"EQUAL", 13, 0xBB, 0x0D, false},
		{"LEFTBRACE", 26, 0xDB, 0x1A, false},
		{"RIGHTBRACE", 27, 0xDD, 0x1B, false},
		{"SEMICOLON", 39, 0xBA, 0x27, false},
		{"APOSTROPHE", 40, 0xDE, 0x28, false},
		{"GRAVE", 41, 0xC0, 0x29, false},
		{"BACKSLASH", 43, 0xDC, 0x2B, false},
		{"COMMA", 51, 0xBC, 0x33, false},
		{"DOT", 52, 0xBE, 0x34, false},
		{"SLASH", 53, 0xBF, 0x35, false},

		{"KP0", 82, 0x60, 0x52, false},
		{"KP1", 79, 0x61, 0x4F, false},
		{"KP5", 76, 0x65, 0x4C, false},
		{"KP9", 73, 0x69, 0x49, false},
		{"KPASTERISK", 55, 0x6A, 0x37, false},
		{"KPPLUS", 78, 0x6B, 0x4E, false},
		{"KPMINUS", 74, 0x6D, 0x4A, false},
		{"KPDOT", 83, 0x6E, 0x53, false},
		{"KPSLASH", 98, 0x6F, 0x35, true},
		{"KPENTER", 96, 0x0D, 0x1C, true},
		{"NUMLOCK", 69, 0x90, 0x45, true},

		{"INSERT", 110, 0x2D, 0x52, true},
		{"DELETE", 111, 0x2E, 0x53, true},
		{"HOME", 102, 0x24, 0x47, true},
		{"END", 107, 0x23, 0x4F, true},
		{"PAGEUP", 104, 0x21, 0x49, true},
		{"PAGEDOWN", 109, 0x22, 0x51, true},
		{"UP", 103, 0x26, 0x48, true},
		{"LEFT", 105, 0x25, 0x4B, true},
		{"RIGHT", 106, 0x27, 0x4D, true},
		{"DOWN", 108, 0x28, 0x50, true},
	}
	for _, c := range cases {
		vk, scan, ext, ok := WinKey(c.code)
		if !ok {
			t.Errorf("%s (%d): not mapped", c.name, c.code)
			continue
		}
		if vk != c.vk || scan != c.scan || ext != c.ext {
			t.Errorf("%s (%d) = vk 0x%02X scan 0x%02X ext %v, want vk 0x%02X scan 0x%02X ext %v",
				c.name, c.code, vk, scan, ext, c.vk, c.scan, c.ext)
		}
		if !Mappable(c.code) {
			t.Errorf("Mappable(%s) = false for a mapped key", c.name)
		}
	}
}

// An unmapped code says so, both ways, rather than mapping to VK 0.
func TestWinKeyUnmapped(t *testing.T) {
	for _, code := range []uint16{0, 119 /* PAUSE */, 183 /* F13 */, 0x110 /* BTN_LEFT */} {
		if _, _, _, ok := WinKey(code); ok {
			t.Errorf("WinKey(%d) mapped, want ok=false", code)
		}
		if Mappable(code) {
			t.Errorf("Mappable(%d) = true, want false", code)
		}
	}
}

// Every extended key must also be in the VK table, or WinKey hides it.
func TestExtScanKeysAreMapped(t *testing.T) {
	for code := range extScan {
		if _, ok := vk[code]; !ok {
			t.Errorf("evdev %d has an extended scancode but no VK", code)
		}
	}
}
