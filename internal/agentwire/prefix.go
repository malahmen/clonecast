package agentwire

import (
	"path"
	"strings"
)

// The agent's install location inside a prefix, as a Unix path's last three
// components under the prefix: <prefix>/drive_c/clonecast/clonecast-agent.exe,
// i.e. C:\clonecast\clonecast-agent.exe. These mirror prefix.InstallDirWin and
// prefix.ExeName (a test in internal/prefix keeps them in step); they are
// repeated here so the agent does not link the host-side install code.
const (
	InstallDirName = "clonecast"
	AgentExeName   = "clonecast-agent.exe"
)

// AnnouncedPrefix is the prefix the agent puts in its hello. WINEPREFIX wins
// when it is set, as it is under Bottles, Lutris and Proton. Plain `wine` sets
// none and runs in $HOME/.wine, so the agent then derives its prefix from its
// own install path instead (PrefixFromExe), which is right whatever HOME is
// (Kamino 276). clonecast normalises both sides (prefix.SamePrefix: absolute,
// no trailing slash, symlinks resolved) before comparing, so the agent sends
// the path as it has it. "" means unknown, and clonecast then pairs by title.
func AnnouncedPrefix(wineprefix, unixExe string) string {
	if strings.TrimSpace(wineprefix) != "" {
		return wineprefix
	}
	return PrefixFromExe(unixExe)
}

// PrefixFromExe returns the Wine prefix an installed agent runs in, given the
// Unix path of its own exe (Wine's wine_get_unix_file_name of
// C:\clonecast\clonecast-agent.exe). Wine reaches drive C through the prefix's
// dosdevices/c: link, so the path normally ends in
// dosdevices/c:/clonecast/clonecast-agent.exe; one ending in
// drive_c/clonecast/clonecast-agent.exe is accepted too. Either tail is
// stripped, compared case-insensitively since Windows names are. Anything
// else — an agent run from elsewhere, a relative or empty path — is "": a
// guessed prefix could pair the agent with another prefix's window, while ""
// only costs the prefix match.
func PrefixFromExe(unixExe string) string {
	if !strings.HasPrefix(unixExe, "/") {
		return ""
	}
	parts := strings.Split(path.Clean(unixExe), "/")
	n := len(parts)
	if n < 4 || !strings.EqualFold(parts[n-1], AgentExeName) || !strings.EqualFold(parts[n-2], InstallDirName) {
		return ""
	}
	var rest []string
	switch {
	case strings.EqualFold(parts[n-3], "drive_c"):
		rest = parts[:n-3]
	case n >= 5 && strings.EqualFold(parts[n-3], "c:") && strings.EqualFold(parts[n-4], "dosdevices"):
		rest = parts[:n-4]
	default:
		return ""
	}
	// "" when the tail sat directly under "/": there is no prefix directory.
	return strings.Join(rest, "/")
}
