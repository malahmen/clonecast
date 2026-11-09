// Package agentwire is the shared protocol between clonecast (Linux) and the
// in-bottle clonecast-agent (Windows). It also holds the evdev -> Win32
// virtual-key/scancode mapping the agent needs for PostMessage. It is
// platform-neutral (no build tag) so both sides encode/decode identically.
//
// # The connection
//
// The agent dials clonecast, not the reverse (REFERENCE.md 4.17): clonecast
// listens once on a single loopback port and every agent connects in and stays
// connected, announcing which prefix and window it serves. Key frames then
// travel back down that same connection. This is what removes the static
// per-instance ports and the hand-written --agent title map.
//
// # The format
//
// A newline-delimited stream of ASCII lines, one message per line, fields
// separated by single spaces. The transport is trusted loopback, so the format
// is deliberately trivial to parse on both sides — no framing, no lengths, no
// dependencies.
//
//	agent -> clonecast
//	  H <ver> <key>=<value> ...   hello; the first line after connecting
//	  R <mac>                     v3: the agent's answer to the challenge
//	  K                           keepalive (also the answer to a P)
//
//	clonecast -> agent
//	  C <nonce> <mac>             v3: clonecast's proof and challenge
//	  V <ver>                     the hello's version is not the one clonecast
//	                              is set to; <ver> is the one it is set to
//	  D <evdev-code>              key down
//	  U <evdev-code>              key up
//	  P                           ping (liveness probe; answered with K)
//
// The v3 handshake (H, C, R) is in auth.go.
//
// Hello fields, all optional and order-independent:
//
//	prefix=  the agent's Wine prefix: WINEPREFIX from its environment, or,
//	         under plain `wine` which sets none, the prefix it is installed
//	         in, worked out from its own path (AnnouncedPrefix)
//	title=   the caption of the window the agent delivers to
//	exe=     the agent's own image path (diagnostics only)
//	pid=     the agent's Windows process id, decimal (diagnostics only)
//	hwnd=    the resolved game window handle, hex without 0x (diagnostics)
//	token=   v2 only: the secret `clonecast agent install` left beside the
//	         agent; clonecast refuses an agent without the right one
//	nonce=   v3 only: the agent's challenge, 16 random bytes in hex
//
// Values are percent-encoded (net/url query escaping), so a title or a path
// may contain spaces, '=' or non-ASCII and still survive a space-separated,
// line-oriented format.
//
// # Versioning
//
// The hello carries the protocol version so a future change is detectable: a
// peer that is handed a version it does not implement refuses the connection
// and says so, rather than guessing at the fields. This build implements 2 and
// 3, but each side speaks only the one it is set to (Kamino 275): there is no
// fallback, because a fake clonecast could otherwise just ask for v2 and be
// sent the token. Within a version, unknown
// hello fields and unknown line kinds are ignored, so additive changes (a new
// field, a new message kind such as the foreground report research §C1
// sketches) do not need a version bump and do not break an older peer.
package agentwire

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Version is the protocol version this build speaks, announced in the hello.
//
// 1 was the original static-port protocol: clonecast dialled the agent and the
// stream carried nothing but D/U frames. 2 reverses the connection and adds
// the hello and the keepalive. 3 replaces the token in the hello with a
// challenge-response, so the agent never sends its secret to whatever answers
// on clonecast's port (Kamino 275).
const Version = 3

// MinVersion is the oldest protocol this build still implements. 2 is kept so
// agents installed before 3 keep working when both sides are set to 2.
const MinVersion = 2

// Keepalive timings. The agent sends a K every KeepaliveInterval; clonecast
// drops a connection that has been silent for DeadAfter, which is what makes a
// wedged or killed agent disappear from the registry instead of lingering as a
// target that silently swallows keys. DeadAfter is several intervals so a
// missed tick (a busy wineserver, a suspended host) is not a disconnect.
const (
	KeepaliveInterval = 5 * time.Second
	DeadAfter         = 20 * time.Second
	// HelloTimeout is how long clonecast waits for the first line of a new
	// connection before hanging up on it.
	HelloTimeout = 10 * time.Second
)

// Kind identifies a decoded line.
type Kind int

const (
	// KindUnknown is a well-formed line this build has no meaning for. It is
	// ignored rather than refused, so a newer peer may add message kinds.
	KindUnknown Kind = iota
	KindFrame
	KindHello
	KindKeepalive
	KindPing
	KindChallenge // v3: C, clonecast -> agent
	KindResponse  // v3: R, agent -> clonecast
	KindVersion   // V, clonecast -> agent: the hello's version was refused
)

func (k Kind) String() string {
	switch k {
	case KindFrame:
		return "frame"
	case KindHello:
		return "hello"
	case KindKeepalive:
		return "keepalive"
	case KindPing:
		return "ping"
	case KindChallenge:
		return "challenge"
	case KindResponse:
		return "response"
	case KindVersion:
		return "version refusal"
	}
	return "unknown"
}

// Frame is one key transition: a Linux evdev key code and whether it went down
// (true) or up (false).
type Frame struct {
	Code uint16
	Down bool
}

// Write encodes f as one line: "D <code>\n" for a press, "U <code>\n" for a
// release.
func (f Frame) Write(w io.Writer) error {
	tag := byte('U')
	if f.Down {
		tag = 'D'
	}
	_, err := fmt.Fprintf(w, "%c %d\n", tag, f.Code)
	return err
}

// Hello is what an agent announces on connect: enough for clonecast to pair
// the connection with a window on the Linux side (Prefix, and Title as the
// fallback) plus identity for the log (PID, HWND, Exe).
type Hello struct {
	Version int
	Prefix  string // WINEPREFIX inside the prefix's environment
	Title   string // the caption of the window the agent posts keys to (AnnouncedTitle)
	Exe     string // the agent binary's own path
	PID     int    // the agent's *Windows* pid — never a host pid
	HWND    uint64 // the resolved game window handle
	Token   string // v2: the install's shared secret; never logged, never sent in v3
	Nonce   string // v3: the agent's challenge (auth.go)
}

// AnnouncedTitle is the title an agent puts in its hello: the caption of the
// window it actually found, and only while it has found none, the -title it
// was given. Announcing the flag instead (Kamino 137) left the title empty for
// every agent installed with the defaults, which pass no -title, so clonecast
// paired on the prefix alone and gave the agent the first window of that
// prefix in KWin's order: a launcher or a dialog could get the keys.
func AnnouncedTitle(flagTitle, caption string) string {
	if caption != "" {
		return caption
	}
	return flagTitle
}

// Write encodes h as the hello line.
func (h Hello) Write(w io.Writer) error {
	ver := h.Version
	if ver == 0 {
		ver = Version
	}
	var b strings.Builder
	fmt.Fprintf(&b, "H %d", ver)
	if h.Prefix != "" {
		fmt.Fprintf(&b, " prefix=%s", url.QueryEscape(h.Prefix))
	}
	if h.Title != "" {
		fmt.Fprintf(&b, " title=%s", url.QueryEscape(h.Title))
	}
	if h.Exe != "" {
		fmt.Fprintf(&b, " exe=%s", url.QueryEscape(h.Exe))
	}
	if h.PID != 0 {
		fmt.Fprintf(&b, " pid=%d", h.PID)
	}
	if h.HWND != 0 {
		fmt.Fprintf(&b, " hwnd=%x", h.HWND)
	}
	if h.Token != "" {
		fmt.Fprintf(&b, " token=%s", url.QueryEscape(h.Token))
	}
	if h.Nonce != "" {
		fmt.Fprintf(&b, " nonce=%s", url.QueryEscape(h.Nonce))
	}
	b.WriteByte('\n')
	_, err := io.WriteString(w, b.String())
	return err
}

// Describe is a one-line human summary for logs.
func (h Hello) Describe() string {
	switch {
	case h.Prefix != "" && h.Title != "":
		return fmt.Sprintf("prefix %s, window %q", h.Prefix, h.Title)
	case h.Prefix != "":
		return "prefix " + h.Prefix
	case h.Title != "":
		return fmt.Sprintf("window %q", h.Title)
	}
	return "no prefix and no window title"
}

// Message is one decoded line. Only the field matching Kind is meaningful.
type Message struct {
	Kind      Kind
	Frame     Frame
	Hello     Hello
	Challenge Challenge
	Response  Response
	Version   int // KindVersion: the version clonecast is set to
}

// ErrUnsupportedVersion is returned by Decode for a hello whose protocol
// version this build does not implement. It is a hard error rather than a
// skipped line: the peer is talking a protocol we would only misread.
var ErrUnsupportedVersion = errors.New("unsupported agentwire protocol version")

// WriteKeepalive sends the agent's periodic "still here" line.
func WriteKeepalive(w io.Writer) error {
	_, err := io.WriteString(w, "K\n")
	return err
}

// WritePing sends clonecast's liveness probe. The agent answers with a
// keepalive; a ping is only needed when clonecast wants to provoke an answer
// (or an error) out of an otherwise idle connection.
func WritePing(w io.Writer) error {
	_, err := io.WriteString(w, "P\n")
	return err
}

// Decode parses one line. A line it does not recognise decodes as KindUnknown
// with no error — that is how additive protocol changes stay compatible —
// while a line that claims to be something it then isn't (a frame with a
// non-numeric code, a hello with a version we cannot read) is an error, so the
// caller can say so instead of silently dropping keys.
func Decode(line string) (Message, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return Message{}, nil
	}
	switch fields[0] {
	case "D", "U":
		if len(fields) != 2 {
			return Message{}, fmt.Errorf("malformed frame %q: want %q or %q", line, "D <code>", "U <code>")
		}
		code, err := strconv.ParseUint(fields[1], 10, 16)
		if err != nil {
			return Message{}, fmt.Errorf("malformed frame %q: bad evdev code: %w", line, err)
		}
		return Message{Kind: KindFrame, Frame: Frame{Code: uint16(code), Down: fields[0] == "D"}}, nil
	case "H":
		h, err := parseHello(fields[1:])
		if err != nil {
			return Message{}, fmt.Errorf("malformed hello %q: %w", line, err)
		}
		return Message{Kind: KindHello, Hello: h}, nil
	case "K":
		return Message{Kind: KindKeepalive}, nil
	case "P":
		return Message{Kind: KindPing}, nil
	case "C":
		if len(fields) != 3 || !validNonce(fields[1]) || !validMAC(fields[2]) {
			return Message{}, fmt.Errorf("malformed challenge %q: want %q", line, "C <nonce> <mac>")
		}
		return Message{Kind: KindChallenge, Challenge: Challenge{Nonce: fields[1], MAC: fields[2]}}, nil
	case "R":
		if len(fields) != 2 || !validMAC(fields[1]) {
			return Message{}, fmt.Errorf("malformed response %q: want %q", line, "R <mac>")
		}
		return Message{Kind: KindResponse, Response: Response{MAC: fields[1]}}, nil
	case "V":
		if len(fields) != 2 {
			return Message{}, fmt.Errorf("malformed version refusal %q: want %q", line, "V <ver>")
		}
		v, err := strconv.Atoi(fields[1])
		if err != nil {
			return Message{}, fmt.Errorf("malformed version refusal %q: %w", line, err)
		}
		return Message{Kind: KindVersion, Version: v}, nil
	}
	return Message{Kind: KindUnknown}, nil
}

func parseHello(fields []string) (Hello, error) {
	if len(fields) == 0 {
		return Hello{}, errors.New("no version")
	}
	ver, err := strconv.Atoi(fields[0])
	if err != nil {
		return Hello{}, fmt.Errorf("bad version %q", fields[0])
	}
	// Both versions this build implements parse; which one a side accepts
	// is its setting, checked by the caller (Kamino 275).
	if ver < MinVersion || ver > Version {
		return Hello{Version: ver}, fmt.Errorf("%w: peer speaks %d, this build speaks %d to %d", ErrUnsupportedVersion, ver, MinVersion, Version)
	}
	h := Hello{Version: ver}
	for _, kv := range fields[1:] {
		key, raw, ok := strings.Cut(kv, "=")
		if !ok {
			return Hello{}, fmt.Errorf("field %q has no '='", kv)
		}
		switch key {
		case "prefix", "title", "exe", "token", "nonce":
			val, err := url.QueryUnescape(raw)
			if err != nil {
				return Hello{}, fmt.Errorf("field %s: %w", key, err)
			}
			switch key {
			case "prefix":
				h.Prefix = val
			case "title":
				h.Title = val
			case "exe":
				h.Exe = val
			case "token":
				h.Token = val
			case "nonce":
				h.Nonce = val
			}
		case "pid":
			n, err := strconv.Atoi(raw)
			if err != nil {
				return Hello{}, fmt.Errorf("field pid: %w", err)
			}
			h.PID = n
		case "hwnd":
			n, err := strconv.ParseUint(raw, 16, 64)
			if err != nil {
				return Hello{}, fmt.Errorf("field hwnd: %w", err)
			}
			h.HWND = n
		default:
			// An unknown field from a newer peer: ignore it.
		}
	}
	return h, nil
}

// Read decodes messages from r, calling handle for each, until r ends or
// errors. Malformed lines are skipped rather than aborting the stream: one
// corrupt line must not cost the connection (and with it every subsequent
// keystroke). Callers that need to see the malformed ones — clonecast's
// registry, which must diagnose a bad hello — use Decode directly.
func Read(r io.Reader, handle func(Message)) error {
	return ReadFrom(bufio.NewScanner(r), handle)
}

// ReadFrom is Read on a scanner the caller already reads from: the agent's
// v3 handshake reads its first lines through it, and a second scanner on the
// same connection would lose whatever the first had buffered.
func ReadFrom(sc *bufio.Scanner, handle func(Message)) error {
	for sc.Scan() {
		msg, err := Decode(sc.Text())
		if err != nil || msg.Kind == KindUnknown {
			continue
		}
		handle(msg)
	}
	return sc.Err()
}
