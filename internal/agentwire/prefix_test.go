package agentwire

import "testing"

// Kamino 276: a plain-`wine` agent has no WINEPREFIX and finds its prefix from
// its own path; anything that is not exactly the install location is refused.
func TestPrefixFromExe(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// What wine_get_unix_file_name returns: drive C through dosdevices.
		{"/home/u/.wine/dosdevices/c:/clonecast/clonecast-agent.exe", "/home/u/.wine"},
		{"/home/u/.wine/dosdevices/C:/Clonecast/CLONECAST-AGENT.EXE", "/home/u/.wine"},
		// The drive_c form, in any case.
		{"/home/u/.wine/drive_c/clonecast/clonecast-agent.exe", "/home/u/.wine"},
		{"/home/u/Games/wow/DRIVE_C/clonecast/clonecast-agent.exe", "/home/u/Games/wow"},
		{"/home/u/.wine//drive_c/./clonecast/clonecast-agent.exe", "/home/u/.wine"},
		{"/p/drive_c/clonecast/clonecast-agent.exe", "/p"},
		// Refused.
		{"", ""},
		{"clonecast-agent.exe", ""},
		{"home/u/.wine/drive_c/clonecast/clonecast-agent.exe", ""}, // relative
		{"/drive_c/clonecast/clonecast-agent.exe", ""},             // no prefix above it
		{"/dosdevices/c:/clonecast/clonecast-agent.exe", ""},
		{"/home/u/.wine/drive_c/other/clonecast-agent.exe", ""},
		{"/home/u/.wine/drive_c/clonecast/agent.exe", ""},
		{"/home/u/.wine/drive_d/clonecast/clonecast-agent.exe", ""},
		{"/home/u/.wine/dosdevices/d:/clonecast/clonecast-agent.exe", ""},
		{"/home/u/.wine/c:/clonecast/clonecast-agent.exe", ""}, // c: not under dosdevices
		{"/home/u/.wine/drive_c/clonecast/clonecast-agent.exe/x", ""},
		{"/home/u/.wine/drive_c/clonecast-agent.exe", ""},
		{"/home/u/build/clonecast-agent.exe", ""},
	} {
		if got := PrefixFromExe(tc.in); got != tc.want {
			t.Errorf("PrefixFromExe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAnnouncedPrefix(t *testing.T) {
	exe := "/home/u/.wine/dosdevices/c:/clonecast/clonecast-agent.exe"
	for _, tc := range []struct{ env, exe, want string }{
		{"/bottles/wow", exe, "/bottles/wow"}, // WINEPREFIX wins
		{"", exe, "/home/u/.wine"},            // plain wine
		{"   ", exe, "/home/u/.wine"},         // blank counts as unset, as clonecast reads it
		{"", "", ""},                          // real Windows, or the lookup failed
		{"", "/elsewhere/clonecast-agent.exe", ""},
	} {
		if got := AnnouncedPrefix(tc.env, tc.exe); got != tc.want {
			t.Errorf("AnnouncedPrefix(%q, %q) = %q, want %q", tc.env, tc.exe, got, tc.want)
		}
	}
}
