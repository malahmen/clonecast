// Package agentwire is the shared protocol between clonecast (Linux) and the
// in-bottle clonecast-agent (Windows): a trivial newline-delimited stream of
// key transitions carrying Linux evdev key codes. It also holds the evdev ->
// Win32 virtual-key/scancode mapping the agent needs for PostMessage. It is
// platform-neutral (no build tag) so both sides encode/decode identically.
package agentwire

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Frame is one key transition: a Linux evdev key code and whether it went down
// (true) or up (false).
type Frame struct {
	Code uint16
	Down bool
}

// Write encodes f as one line: "D <code>\n" for a press, "U <code>\n" for a
// release. Trusted localhost transport, so the format is deliberately trivial.
func (f Frame) Write(w io.Writer) error {
	tag := byte('U')
	if f.Down {
		tag = 'D'
	}
	_, err := fmt.Fprintf(w, "%c %d\n", tag, f.Code)
	return err
}

// Read decodes frames from r, calling handle for each, until r ends or errors.
// Malformed lines are skipped rather than aborting the stream.
func Read(r io.Reader, handle func(Frame)) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || (fields[0] != "D" && fields[0] != "U") {
			continue
		}
		code, err := strconv.ParseUint(fields[1], 10, 16)
		if err != nil {
			continue
		}
		handle(Frame{Code: uint16(code), Down: fields[0] == "D"})
	}
	return sc.Err()
}
