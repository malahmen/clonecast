package agentwire

// evdev -> Win32 mapping. Two facts keep this small:
//
//   - For the whole main keyboard block and the numpad, a Linux evdev key code
//     is numerically the PC "Set 1" make scancode Windows wants in
//     WM_KEYDOWN's lParam (KEY_A=30=0x1E, KEY_SPACE=57=0x39, KEY_ENTER=28=0x1C,
//     KEY_F1=59=0x3B, KEY_KP1=79=0x4F, ...). Linux derived those codes from the
//     AT/XT scancodes. So Scan == the evdev code for every non-extended key,
//     and only the virtual-key needs a table.
//   - The "grey" keys (arrows, the Insert/Delete/Home/End/PgUp/PgDn cluster,
//     the right Ctrl and Alt, keypad Enter and /) are E0-extended on the PC
//     set: their scancode is not the evdev code and the extended bit must be
//     set in lParam, or a game reads the right Ctrl as the left one and the
//     arrow keys as the numpad's 8/4/6/2. Those get an explicit scancode in
//     extScan.
//
// Virtual-key codes are the layout-independent VK_* constants games read from
// wParam. The modifiers carry the generic VK_SHIFT/VK_CONTROL/VK_MENU on both
// sides, as WM_KEYDOWN does on Windows: VK_RSHIFT and friends are only what
// GetKeyState answers, and the side is in the scancode and the extended bit.
// The numpad maps to its digit VKs, i.e. as with NumLock on.
//
// A key missing here is a key the agent cannot post, so the engine sends it
// by passthrough instead (Mappable, Kamino 185).

// vk maps evdev code -> Win32 virtual-key code.
var vk = map[uint16]uint16{
	// letters (VK is the ASCII uppercase)
	30: 0x41, 48: 0x42, 46: 0x43, 32: 0x44, 18: 0x45, 33: 0x46, 34: 0x47,
	35: 0x48, 23: 0x49, 36: 0x4A, 37: 0x4B, 38: 0x4C, 50: 0x4D, 49: 0x4E,
	24: 0x4F, 25: 0x50, 16: 0x51, 19: 0x52, 31: 0x53, 20: 0x54, 22: 0x55,
	47: 0x56, 17: 0x57, 45: 0x58, 21: 0x59, 44: 0x5A,
	// digits 1..9,0 (VK 0x31..0x39, 0x30)
	2: 0x31, 3: 0x32, 4: 0x33, 5: 0x34, 6: 0x35, 7: 0x36, 8: 0x37, 9: 0x38, 10: 0x39, 11: 0x30,
	// specials
	57: 0x20, // SPACE
	28: 0x0D, // ENTER (VK_RETURN)
	1:  0x1B, // ESC
	15: 0x09, // TAB
	14: 0x08, // BACKSPACE
	58: 0x14, // CAPSLOCK (VK_CAPITAL)
	69: 0x90, // NUMLOCK (extended — see extScan)
	70: 0x91, // SCROLLLOCK (VK_SCROLL)
	99: 0x2C, // SYSRQ / Print Screen (VK_SNAPSHOT, extended)
	// modifiers, both sides (see the note above on the generic VKs)
	42:  0x10, // LEFTSHIFT  -> VK_SHIFT
	54:  0x10, // RIGHTSHIFT -> VK_SHIFT (scancode 0x36, not extended)
	29:  0x11, // LEFTCTRL   -> VK_CONTROL
	97:  0x11, // RIGHTCTRL  -> VK_CONTROL (extended)
	56:  0x12, // LEFTALT    -> VK_MENU
	100: 0x12, // RIGHTALT   -> VK_MENU (extended)
	125: 0x5B, // LEFTMETA   -> VK_LWIN (extended)
	126: 0x5C, // RIGHTMETA  -> VK_RWIN (extended)
	127: 0x5D, // COMPOSE    -> VK_APPS (extended)
	// punctuation, US positions (VK_OEM_*)
	12: 0xBD, // MINUS      -> VK_OEM_MINUS
	13: 0xBB, // EQUAL      -> VK_OEM_PLUS
	26: 0xDB, // LEFTBRACE  -> VK_OEM_4
	27: 0xDD, // RIGHTBRACE -> VK_OEM_6
	39: 0xBA, // SEMICOLON  -> VK_OEM_1
	40: 0xDE, // APOSTROPHE -> VK_OEM_7
	41: 0xC0, // GRAVE      -> VK_OEM_3
	43: 0xDC, // BACKSLASH  -> VK_OEM_5
	51: 0xBC, // COMMA      -> VK_OEM_COMMA
	52: 0xBE, // DOT        -> VK_OEM_PERIOD
	53: 0xBF, // SLASH      -> VK_OEM_2
	86: 0xE2, // 102ND (the ISO key left of Z) -> VK_OEM_102
	// function keys F1..F12 (VK_F1=0x70)
	59: 0x70, 60: 0x71, 61: 0x72, 62: 0x73, 63: 0x74, 64: 0x75,
	65: 0x76, 66: 0x77, 67: 0x78, 68: 0x79, 87: 0x7A, 88: 0x7B,
	// numpad digits (VK_NUMPAD0=0x60..VK_NUMPAD9=0x69)
	82: 0x60, 79: 0x61, 80: 0x62, 81: 0x63, 75: 0x64,
	76: 0x65, 77: 0x66, 71: 0x67, 72: 0x68, 73: 0x69,
	// numpad operators
	55: 0x6A, // KPASTERISK -> VK_MULTIPLY
	78: 0x6B, // KPPLUS     -> VK_ADD
	74: 0x6D, // KPMINUS    -> VK_SUBTRACT
	83: 0x6E, // KPDOT      -> VK_DECIMAL
	98: 0x6F, // KPSLASH    -> VK_DIVIDE (extended)
	96: 0x0D, // KPENTER    -> VK_RETURN (extended)
	// navigation cluster (extended — see extScan)
	110: 0x2D, // INSERT
	111: 0x2E, // DELETE
	102: 0x24, // HOME
	107: 0x23, // END
	104: 0x21, // PAGEUP   (VK_PRIOR)
	109: 0x22, // PAGEDOWN (VK_NEXT)
	// arrows (extended — see extScan)
	103: 0x26, // UP
	105: 0x25, // LEFT
	106: 0x27, // RIGHT
	108: 0x28, // DOWN
}

// extScan holds the E0-extended keys' make scancodes (without the E0): the
// key is posted with this scancode and the extended bit set.
var extScan = map[uint16]uint16{
	103: 0x48, // UP
	105: 0x4B, // LEFT
	106: 0x4D, // RIGHT
	108: 0x50, // DOWN
	110: 0x52, // INSERT
	111: 0x53, // DELETE
	102: 0x47, // HOME
	107: 0x4F, // END
	104: 0x49, // PAGEUP
	109: 0x51, // PAGEDOWN
	97:  0x1D, // RIGHTCTRL
	100: 0x38, // RIGHTALT
	96:  0x1C, // KPENTER
	98:  0x35, // KPSLASH
	99:  0x37, // SYSRQ
	125: 0x5B, // LEFTMETA
	126: 0x5C, // RIGHTMETA
	127: 0x5D, // COMPOSE
	// NumLock is the odd one: its scancode IS the evdev code (0x45), yet
	// Windows' keystroke flags count it among the extended keys.
	69: 0x45,
}

// WinKey returns the Win32 virtual-key, the make scancode, whether the key is
// E0-extended (its extended bit must be set in lParam), and ok=false when the
// evdev code isn't mapped.
func WinKey(code uint16) (vkCode, scan uint16, extended, ok bool) {
	v, ok := vk[code]
	if !ok {
		return 0, 0, false, false
	}
	if s, isExt := extScan[code]; isExt {
		return v, s, true, true
	}
	return v, code, false, true // scancode == evdev code for the main block
}

// Mappable reports whether the agent can post this evdev code. It is what the
// Linux side asks before sending a key to an agent: one it cannot map would be
// logged and dropped in the bottle, while clonecast counted it as delivered
// (Kamino 185).
func Mappable(code uint16) bool {
	_, ok := vk[code]
	return ok
}
