package agentwire

// evdev -> Win32 mapping. Two facts keep this small:
//
//   - For the whole main keyboard block, a Linux evdev key code is numerically
//     the PC "Set 1" make scancode Windows wants in WM_KEYDOWN's lParam
//     (KEY_A=30=0x1E, KEY_SPACE=57=0x39, KEY_ENTER=28=0x1C, KEY_F1=59=0x3B,
//     ...). Linux derived those codes from the AT/XT scancodes. So Scan == the
//     evdev code for every non-extended key, and only the virtual-key needs a
//     table.
//   - The handful of "grey"/navigation keys (arrows here) are E0-extended on
//     the PC set: their scancode is not the evdev code and the extended bit
//     must be set in lParam. Those get an explicit scancode in extScan.
//
// Virtual-key codes are the layout-independent VK_* constants games read from
// wParam. Only keys useful for WoW multiboxing are mapped; extend as needed.

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
	42: 0x10, // LEFTSHIFT -> VK_SHIFT
	29: 0x11, // LEFTCTRL  -> VK_CONTROL
	56: 0x12, // LEFTALT   -> VK_MENU
	// function keys F1..F12 (VK_F1=0x70)
	59: 0x70, 60: 0x71, 61: 0x72, 62: 0x73, 63: 0x74, 64: 0x75,
	65: 0x76, 66: 0x77, 67: 0x78, 68: 0x79, 87: 0x7A, 88: 0x7B,
	// arrows (extended — see extScan)
	103: 0x26, // UP
	105: 0x25, // LEFT
	106: 0x27, // RIGHT
	108: 0x28, // DOWN
}

// extScan holds the E0-extended keys' make scancodes (evdev code != scancode).
var extScan = map[uint16]uint16{
	103: 0x48, // UP
	105: 0x4B, // LEFT
	106: 0x4D, // RIGHT
	108: 0x50, // DOWN
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
