// Package prefix installs the in-bottle clonecast agent into a Wine prefix so
// that the *prefix* owns the agent's lifetime, not clonecast and not the game
// launcher (REFERENCE.md 7.11, research.md §C2).
//
// The mechanism is launcher-agnostic: every `wine <exe>` that starts a fresh
// wineserver implicitly runs `wineboot --init` (ntdll run_wineboot), and
// --init processes RunServicesOnce, RunServices and RunOnce — but not `Run`
// and not the Startup folder. Bottles, Lutris, Proton and umu never run a bare
// `wineboot` per launch, so an entry under
//
//	HKLM\Software\Microsoft\Windows\CurrentVersion\RunServices
//
// is the one hook that fires on every prefix boot under every launcher. The
// agent then inherits the launcher's environment and dies with the wineserver,
// so the host never has to signal it (which used to kill the game, 4.14).
//
// This file implements the offline route: editing <prefix>/system.reg directly.
// Wine's registry files are a documented plain-text format, so no wine binary
// is needed — but wineserver rewrites them wholesale on shutdown, so the edit
// is only valid while no wineserver is running for that prefix (see proc.go
// and discover_*.go for that check, and install.go for the route choice).
//
// The parser is deliberately line-preserving: every line is kept verbatim and
// only the lines we add or change are touched, so an edit round-trips an
// otherwise untouched system.reg byte for byte.
package prefix

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunServicesKey is the autostart key processed by `wineboot --init`.
//
// Note it is written once, in the 64-bit view. On a 64-bit prefix wineboot
// processes the run keys twice (it re-opens them with KEY_WOW64_32KEY), and
// this key is not redirected, so a single entry is started twice — which is
// why the agent takes a named mutex (research.md §B I7).
const RunServicesKey = `Software\Microsoft\Windows\CurrentVersion\RunServices`

// DefaultValueName is the registry value name (and log/exe basename) used for
// the agent's autostart entry.
const DefaultValueName = "clonecast-agent"

// RegFile is a parsed Wine registry file (system.reg / user.reg).
//
// It is a header followed by key blocks. A block owns its "[Key\\Path] <time>"
// line, the "#time="/"#class=" lines under it, its value lines, and any blank
// lines that follow it — i.e. the file is split at '[' lines, so joining the
// blocks back together reproduces the input exactly.
type RegFile struct {
	Path   string
	Arch   string // from "#arch=..." in the header, e.g. "win64"
	header []string
	blocks []*regBlock
}

type regBlock struct {
	key   string // unescaped key path, e.g. Software\Microsoft\...\RunServices
	lines []string
}

// LoadRegFile reads and parses a Wine registry file.
func LoadRegFile(path string) (*RegFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseRegFile(path, string(b)), nil
}

// ParseRegFile parses the contents of a Wine registry file.
func ParseRegFile(path, content string) *RegFile {
	f := &RegFile{Path: path}
	var cur *regBlock
	for _, line := range splitLines(content) {
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "[") {
			cur = &regBlock{key: parseKeyLine(trimmed)}
			f.blocks = append(f.blocks, cur)
			cur.lines = append(cur.lines, line)
			continue
		}
		if cur == nil {
			if rest, ok := cutPrefix(trimmed, "#arch="); ok {
				f.Arch = strings.TrimSpace(rest)
			}
			f.header = append(f.header, line)
			continue
		}
		cur.lines = append(cur.lines, line)
	}
	return f
}

// String renders the file back to text. Unmodified files round-trip exactly.
func (f *RegFile) String() string {
	var b strings.Builder
	for _, l := range f.header {
		b.WriteString(l)
	}
	for _, blk := range f.blocks {
		for _, l := range blk.lines {
			b.WriteString(l)
		}
	}
	return b.String()
}

// GetString returns the data of a REG_SZ value, and whether it was found. Key
// and value names are matched case-insensitively, as Wine matches them.
func (f *RegFile) GetString(key, name string) (string, bool) {
	blk := f.find(key)
	if blk == nil {
		return "", false
	}
	for _, line := range blk.lines[1:] {
		n, data, ok := parseValueLine(line)
		if !ok || !strings.EqualFold(n, name) {
			continue
		}
		s, ok := unquoteRegString(strings.TrimSpace(data))
		return s, ok
	}
	return "", false
}

// SetString sets a REG_SZ value, creating the key block if it is missing, and
// reports whether the file changed. Everything else in the file is untouched.
func (f *RegFile) SetString(key, name, data string) bool {
	want := fmt.Sprintf("%s=%s\n", quoteRegString(name), quoteRegString(data))
	blk := f.find(key)
	if blk == nil {
		f.blocks = append(f.blocks, newRegBlock(key, want))
		return true
	}
	for i, line := range blk.lines[1:] {
		n, _, ok := parseValueLine(line)
		if !ok || !strings.EqualFold(n, name) {
			continue
		}
		if line == want {
			return false
		}
		blk.lines[i+1] = want
		return true
	}
	// Insert after the key line and its "#time="/"#class=" metadata, before
	// the first value, so the block keeps Wine's own shape.
	at := 1
	for at < len(blk.lines) && strings.HasPrefix(blk.lines[at], "#") {
		at++
	}
	blk.lines = append(blk.lines[:at], append([]string{want}, blk.lines[at:]...)...)
	return true
}

// DeleteValue removes a value from a key and reports whether it was there.
// An emptied key block is left in place: an empty key is valid, and removing
// keys we did not create is not our business.
func (f *RegFile) DeleteValue(key, name string) bool {
	blk := f.find(key)
	if blk == nil {
		return false
	}
	for i, line := range blk.lines[1:] {
		n, _, ok := parseValueLine(line)
		if !ok || !strings.EqualFold(n, name) {
			continue
		}
		blk.lines = append(blk.lines[:i+1], blk.lines[i+2:]...)
		return true
	}
	return false
}

// Save writes the file back atomically (temp file in the same directory, then
// rename), keeping a one-shot ".clonecast.bak" copy of the original. The
// caller must have established that no wineserver is running for this prefix:
// a live wineserver holds the registry in memory and rewrites the file on
// shutdown, which would silently discard the edit.
func (f *RegFile) Save() error {
	info, err := os.Stat(f.Path)
	if err != nil {
		return err
	}
	if orig, err := os.ReadFile(f.Path); err == nil {
		bak := f.Path + ".clonecast.bak"
		if _, err := os.Stat(bak); os.IsNotExist(err) {
			_ = os.WriteFile(bak, orig, info.Mode().Perm())
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(f.Path), filepath.Base(f.Path)+".clonecast*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.WriteString(f.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(name, f.Path)
}

func (f *RegFile) find(key string) *regBlock {
	for _, blk := range f.blocks {
		if strings.EqualFold(blk.key, key) {
			return blk
		}
	}
	return nil
}

// newRegBlock builds a fresh key block in Wine's own layout: a blank line, the
// bracketed key with its modification time in seconds since the epoch, a
// "#time=" line with the same instant as a hex FILETIME, then the value.
func newRegBlock(key, valueLine string) *regBlock {
	now := time.Now()
	// 100ns ticks between 1601-01-01 and 1970-01-01.
	const ticks1601to1970 = 116444736000000000
	ft := uint64(now.UnixNano()/100) + ticks1601to1970
	return &regBlock{
		key: key,
		lines: []string{
			"\n",
			fmt.Sprintf("[%s] %d\n", escapeRegKey(key), now.Unix()),
			fmt.Sprintf("#time=%x%08x\n", ft>>32, uint32(ft)),
			valueLine,
		},
	}
}

// ---- text helpers -----------------------------------------------------------

// splitLines splits content into lines, keeping the line terminators so the
// file round-trips exactly.
func splitLines(s string) []string {
	var out []string
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i+1])
		s = s[i+1:]
	}
	return out
}

func cutPrefix(s, p string) (string, bool) {
	if strings.HasPrefix(s, p) {
		return s[len(p):], true
	}
	return "", false
}

// parseKeyLine extracts the unescaped key path from a "[Some\\Key] 1234" line.
func parseKeyLine(line string) string {
	end := strings.LastIndexByte(line, ']')
	if end < 1 {
		end = len(line)
	}
	s, _ := unescapeReg(line[1:end])
	return s
}

// parseValueLine splits a value line into its (unescaped) name and the raw
// data text. The default value, written "@=...", has the empty name.
func parseValueLine(line string) (name, data string, ok bool) {
	t := strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(t, "@=") {
		return "", t[2:], true
	}
	if !strings.HasPrefix(t, `"`) {
		return "", "", false
	}
	// Find the closing quote of the name, honouring backslash escapes.
	for i := 1; i < len(t); i++ {
		if t[i] == '\\' {
			i++
			continue
		}
		if t[i] == '"' {
			if i+1 >= len(t) || t[i+1] != '=' {
				return "", "", false
			}
			n, ok := unescapeReg(t[1:i])
			if !ok {
				return "", "", false
			}
			return n, t[i+2:], true
		}
	}
	return "", "", false
}

// quoteRegString renders a Go string as a quoted Wine registry string.
// Wine's own dumper escapes the backslash, the quote and the control
// characters; ASCII text needs nothing more, and agent command lines are
// ASCII (paths under drive_c, a port number, a window title).
func quoteRegString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	b.WriteString(escapeRegBody(s))
	b.WriteByte('"')
	return b.String()
}

func escapeRegBody(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, `\x%04x`, r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// escapeRegKey renders a key path for a "[...]" line. Wine escapes '[' and ']'
// in key names too; our keys contain neither, but do it anyway for safety.
func escapeRegKey(key string) string {
	s := escapeRegBody(key)
	s = strings.ReplaceAll(s, "[", `\[`)
	s = strings.ReplaceAll(s, "]", `\]`)
	return s
}

// unquoteRegString unescapes a `"..."` registry string. It returns false for
// anything that is not a quoted string (dword:, hex:, hex(2): and friends).
func unquoteRegString(s string) (string, bool) {
	if len(s) < 2 || !strings.HasPrefix(s, `"`) || !strings.HasSuffix(s, `"`) {
		return "", false
	}
	return unescapeReg(s[1 : len(s)-1])
}

// unescapeReg reverses Wine's string escaping. Unknown escapes yield the
// escaped character itself, which is what Wine's parser does.
func unescapeReg(s string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			continue
		}
		i++
		if i >= len(s) {
			return "", false
		}
		switch s[i] {
		case 'a':
			b.WriteByte(7)
		case 'b':
			b.WriteByte(8)
		case 'e':
			b.WriteByte(27)
		case 'f':
			b.WriteByte(12)
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte(11)
		case 'x':
			// \xNNNN (up to four hex digits), Wine's escape for anything
			// outside printable ASCII.
			n := 0
			var v rune
			for n < 4 && i+1 < len(s) && isHex(s[i+1]) {
				i++
				v = v<<4 | rune(hexVal(s[i]))
				n++
			}
			if n == 0 {
				b.WriteByte('x')
				continue
			}
			b.WriteRune(v)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), true
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}
