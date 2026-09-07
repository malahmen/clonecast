// Package keys models physical keys as Linux evdev key codes and provides the
// allowlist ("key set") that decides which keys clonecast broadcasts.
//
// Key codes are used instead of characters on purpose: a broadcast "A" must be
// the same physical key on every target regardless of keyboard layout or
// modifier state. The names follow the kernel's KEY_* constants with the
// prefix stripped, so "A", "LEFTSHIFT", "F1" and "SPACE" are all valid.
package keys

import (
	"fmt"
	"sort"
	"strings"

	evdev "github.com/holoplot/go-evdev"
)

// Code is a Linux evdev key code (KEY_A, KEY_F1, ...).
type Code = evdev.EvCode

// State is the value of an EV_KEY event.
type State int32

const (
	Up     State = 0
	Down   State = 1
	Repeat State = 2
)

// Event is a single key transition coming from a physical keyboard.
type Event struct {
	Code  Code
	State State
}

func (e Event) String() string {
	return fmt.Sprintf("%s %s", Name(e.Code), e.State)
}

func (s State) String() string {
	switch s {
	case Up:
		return "up"
	case Down:
		return "down"
	case Repeat:
		return "repeat"
	}
	return fmt.Sprintf("state(%d)", int32(s))
}

// Name returns the short kernel name for a code ("A", "F1", "LEFTSHIFT").
func Name(c Code) string {
	if n, ok := evdev.KEYNames[c]; ok {
		return strings.TrimPrefix(n, "KEY_")
	}
	return fmt.Sprintf("KEY_%d", uint16(c))
}

// Parse turns a user supplied name into a code. Accepts "a", "A", "KEY_A".
func Parse(name string) (Code, error) {
	n := strings.ToUpper(strings.TrimSpace(name))
	if n == "" {
		return 0, fmt.Errorf("empty key name")
	}
	if !strings.HasPrefix(n, "KEY_") {
		n = "KEY_" + n
	}
	c, ok := evdev.KEYFromString[n]
	if !ok {
		return 0, fmt.Errorf("unknown key %q", name)
	}
	return c, nil
}

// Set is the broadcast allowlist. The zero value allows nothing; use All()
// for the "broadcast everything" mode.
type Set struct {
	all   bool
	codes map[Code]struct{}
}

// All returns a set that allows every key.
func All() Set { return Set{all: true} }

// Of returns a set allowing exactly the given codes.
func Of(codes ...Code) Set {
	s := Set{codes: make(map[Code]struct{}, len(codes))}
	for _, c := range codes {
		s.codes[c] = struct{}{}
	}
	return s
}

// ParseSet builds a set from a whitespace or comma separated list of names.
// The single word "all" (any case) yields All().
func ParseSet(spec string) (Set, error) {
	spec = strings.TrimSpace(spec)
	if strings.EqualFold(spec, "all") {
		return All(), nil
	}
	fields := strings.FieldsFunc(spec, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '\n'
	})
	codes := make([]Code, 0, len(fields))
	for _, f := range fields {
		c, err := Parse(f)
		if err != nil {
			return Set{}, err
		}
		codes = append(codes, c)
	}
	return Of(codes...), nil
}

// Allows reports whether the code should be broadcast.
func (s Set) Allows(c Code) bool {
	if s.all {
		return true
	}
	_, ok := s.codes[c]
	return ok
}

// IsAll reports whether the set is in "everything" mode.
func (s Set) IsAll() bool { return s.all }

// Codes returns the explicit codes sorted by name (kernel codes are not
// alphabetical: KEY_C < KEY_B), or nil in "all" mode.
func (s Set) Codes() []Code {
	if s.all {
		return nil
	}
	out := make([]Code, 0, len(s.codes))
	for c := range s.codes {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return Name(out[i]) < Name(out[j]) })
	return out
}

// Toggle adds or removes a code. Toggling in "all" mode switches to an explicit
// set containing everything except the code, which is rarely wanted, so callers
// should leave "all" mode first.
func (s Set) Toggle(c Code) Set {
	if s.all {
		return s
	}
	out := Of(s.Codes()...)
	if _, ok := out.codes[c]; ok {
		delete(out.codes, c)
	} else {
		out.codes[c] = struct{}{}
	}
	return out
}

func (s Set) String() string {
	if s.all {
		return "all"
	}
	codes := s.Codes()
	if len(codes) == 0 {
		return "none"
	}
	names := make([]string, len(codes))
	for i, c := range codes {
		names[i] = Name(c)
	}
	return strings.Join(names, " ")
}
