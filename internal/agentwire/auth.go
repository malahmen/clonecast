package agentwire

// The v3 handshake (Kamino 275). In v2 the agent sent its token in the hello
// to whatever answered on 127.0.0.1:48800, so anything listening there first
// got it. In v3 the token never goes on the wire, and clonecast proves it
// holds the token before the agent sends anything that depends on it:
//
//	agent     -> H 3 ... nonce=<a>     the hello, without token=
//	clonecast -> C <c> <mac1>          mac1 = HMAC-SHA256(token, "clonecast|" a "|" c)
//	agent        checks mac1; on a mismatch it closes without a word
//	agent     -> R <mac2>              mac2 = HMAC-SHA256(token, "agent|" c "|" a)
//	clonecast    checks mac2; on a mismatch it refuses the agent
//
// Both nonces are 16 random bytes in hex, fresh per connection, so a recorded
// R is worth nothing on the next one. The "clonecast|" and "agent|" prefixes
// keep the two MACs apart: a fake clonecast cannot get the agent to compute
// the clonecast's proof by echoing its nonce back. Every MAC is compared with
// hmac.Equal, in constant time.
//
// There is no fallback between the versions on either side: each side speaks
// only the protocol it is set to, because a fake clonecast could otherwise
// just ask for v2 and be sent the token. A mismatch is a refusal that says
// what to change (VersionMismatch).
//
// This file is the whole state machine, with no sockets and no clock, so the
// registry and the agent share it and it is tested here. Deadlines are the
// caller's: both ends run it under HelloTimeout.

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// DefaultProtocol is the protocol both sides use unless set otherwise.
const DefaultProtocol = Version

// ValidProtocol reports whether p is a protocol this build can be set to.
func ValidProtocol(p int) bool { return p >= MinVersion && p <= Version }

// ParseProtocol reads a protocol setting ("3" or "2").
func ParseProtocol(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || !ValidProtocol(p) {
		return 0, fmt.Errorf("agent protocol %q: want %d or %d", s, Version, MinVersion)
	}
	return p, nil
}

// nonceLen is the nonces' length in bytes, before hex.
const nonceLen = 16

// Challenge is clonecast's C line: its own nonce and its proof over the
// agent's.
type Challenge struct {
	Nonce string // c, hex
	MAC   string // mac1, hex
}

// Write encodes c as "C <nonce> <mac>\n".
func (c Challenge) Write(w io.Writer) error {
	_, err := fmt.Fprintf(w, "C %s %s\n", c.Nonce, c.MAC)
	return err
}

// Response is the agent's R line: its proof over clonecast's nonce.
type Response struct {
	MAC string // mac2, hex
}

// Write encodes r as "R <mac>\n".
func (r Response) Write(w io.Writer) error {
	_, err := fmt.Fprintf(w, "R %s\n", r.MAC)
	return err
}

// WriteVersionRefusal sends "V <ver>": clonecast refuses the hello's version
// and says which one it is set to, so the agent can log the mismatch too. An
// agent built before v3 ignores the line, as it ignores every unknown kind.
func WriteVersionRefusal(w io.Writer, ver int) error {
	_, err := fmt.Fprintf(w, "V %d\n", ver)
	return err
}

// NewNonce returns 16 bytes from rnd, in hex.
func NewNonce(rnd io.Reader) (string, error) {
	b := make([]byte, nonceLen)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", fmt.Errorf("nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func isHex(s string, n int) bool {
	if len(s) != 2*n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validNonce(s string) bool { return isHex(s, nonceLen) }
func validMAC(s string) bool   { return isHex(s, sha256.Size) }

func mac(token, msg string) []byte {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// clonecastMAC is mac1, clonecast's proof; agentMAC is mac2, the agent's.
func clonecastMAC(token, a, c string) []byte { return mac(token, "clonecast|"+a+"|"+c) }
func agentMAC(token, c, a string) []byte     { return mac(token, "agent|"+c+"|"+a) }

// macMatches compares a received hex MAC with the expected one in constant
// time. Malformed hex is a mismatch.
func macMatches(gotHex string, want []byte) bool {
	got, err := hex.DecodeString(gotHex)
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

var (
	// ErrNoToken: v3 proves the install's token, so a side without one
	// cannot run it. clonecast always has one (agentToken); an agent has none
	// until it is installed.
	ErrNoToken = errors.New("protocol 3 needs the install's token, and there is none")
	// ErrBadNonce: a v3 hello without a valid nonce= cannot be challenged.
	ErrBadNonce = errors.New("the hello carries no valid nonce")
	// ErrClonecastUnproven: the challenge does not prove the token. The agent
	// sends nothing more on that connection.
	ErrClonecastUnproven = errors.New("the challenge does not prove the install's token: whatever answered is not the clonecast this agent was installed by")
	// ErrAgentUnproven: the response does not prove the token, or is a
	// second answer to the same challenge.
	ErrAgentUnproven = errors.New("the response does not prove the install's token")
)

// VersionMismatch is a hello in one protocol met by a side set to the other.
// Its message says what to change, from the side that logs it.
type VersionMismatch struct {
	Clonecast int // the protocol clonecast is set to
	Agent     int // the protocol the agent is set to
	// AgentSide is true for the agent's log, false for clonecast's.
	AgentSide bool
}

func (e *VersionMismatch) Error() string {
	if e.AgentSide {
		if e.Agent < e.Clonecast {
			return fmt.Sprintf("clonecast speaks protocol %d: reinstall this agent (it is set to %d), or set clonecast's agent protocol to %d",
				e.Clonecast, e.Agent, e.Agent)
		}
		return fmt.Sprintf("clonecast speaks protocol %d: set its agent protocol to %d, or reinstall this agent with --protocol %d",
			e.Clonecast, e.Agent, e.Clonecast)
	}
	if e.Agent < e.Clonecast {
		return fmt.Sprintf("this agent speaks protocol %d: reinstall it, or set the agent protocol to %d", e.Agent, e.Agent)
	}
	return fmt.Sprintf("this agent speaks protocol %d: set the agent protocol to %d, or reinstall it with --protocol %d",
		e.Agent, e.Agent, e.Clonecast)
}

// AgentHello is the hello an agent at protocol p sends: the token rides in it
// only at 2. At 3 it never does, re-announces included (Kamino 275).
func AgentHello(p int, token string, h Hello) Hello {
	h.Version, h.Token = p, ""
	if p == 2 {
		h.Token = token
	}
	return h
}

// CheckVersion is clonecast's decision on a hello's version under its
// setting: nil, or the mismatch to refuse with.
func CheckVersion(setting int, h Hello) error {
	if h.Version == setting {
		return nil
	}
	return &VersionMismatch{Clonecast: setting, Agent: h.Version}
}

// NoChallengeError is a v3 agent's connection that ended, or timed out,
// before clonecast's challenge: what a clonecast set to 2, or one built before
// v3, does with a v3 hello.
type NoChallengeError struct {
	Err error // the read error, nil when the connection was closed
}

func (e *NoChallengeError) Error() string {
	why := "the connection was closed"
	if e.Err != nil {
		why = e.Err.Error()
	}
	return fmt.Sprintf("no challenge from clonecast (%s): it is set to protocol 2, or is older than protocol 3; set its agent protocol to 3, or reinstall this agent with --protocol 2", why)
}

func (e *NoChallengeError) Unwrap() error { return e.Err }

// AgentAuth is the agent's side of one v3 handshake.
type AgentAuth struct {
	token    string
	nonce    string
	answered bool
}

// NewAgentAuth starts a handshake with a fresh nonce from rnd.
func NewAgentAuth(token string, rnd io.Reader) (*AgentAuth, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	n, err := NewNonce(rnd)
	if err != nil {
		return nil, err
	}
	return &AgentAuth{token: token, nonce: n}, nil
}

// Nonce is a, for the hello's nonce= field.
func (a *AgentAuth) Nonce() string { return a.nonce }

// Answer checks clonecast's proof and returns the response to send. On an
// error the agent sends nothing: no R, so a fake clonecast learns nothing it
// could replay against the real one. One challenge per connection.
func (a *AgentAuth) Answer(c Challenge) (Response, error) {
	if a.answered || !validNonce(c.Nonce) || c.Nonce == a.nonce ||
		!macMatches(c.MAC, clonecastMAC(a.token, a.nonce, c.Nonce)) {
		return Response{}, ErrClonecastUnproven
	}
	a.answered = true
	return Response{MAC: hex.EncodeToString(agentMAC(a.token, c.Nonce, a.nonce))}, nil
}

// Run is the agent's side after its hello (with Nonce) is written: it reads
// clonecast's first line from sc and, if it is a challenge that proves the
// token, writes the response to w. A V line is the mismatch, set as the
// agent at protocol 3 sees it; the connection ending first is a
// NoChallengeError.
func (a *AgentAuth) Run(sc *bufio.Scanner, w io.Writer) error {
	for sc.Scan() {
		msg, err := Decode(sc.Text())
		if err != nil {
			return ErrClonecastUnproven
		}
		switch msg.Kind {
		case KindUnknown:
			continue // a newer clonecast's additive line
		case KindVersion:
			return &VersionMismatch{Clonecast: msg.Version, Agent: Version, AgentSide: true}
		case KindChallenge:
			r, err := a.Answer(msg.Challenge)
			if err != nil {
				return err
			}
			return r.Write(w)
		default:
			return fmt.Errorf("clonecast sent a %s before its challenge", msg.Kind)
		}
	}
	return &NoChallengeError{Err: sc.Err()}
}

// ClonecastAuth is clonecast's side of one v3 handshake.
type ClonecastAuth struct {
	token  string
	agent  string // a
	nonce  string // c
	tested bool
}

// NewClonecastAuth starts the handshake for a v3 hello, with a fresh nonce
// from rnd.
func NewClonecastAuth(token string, h Hello, rnd io.Reader) (*ClonecastAuth, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	if !validNonce(h.Nonce) {
		return nil, ErrBadNonce
	}
	n, err := NewNonce(rnd)
	if err != nil {
		return nil, err
	}
	return &ClonecastAuth{token: token, agent: h.Nonce, nonce: n}, nil
}

// Challenge is the C line to send.
func (s *ClonecastAuth) Challenge() Challenge {
	return Challenge{Nonce: s.nonce, MAC: hex.EncodeToString(clonecastMAC(s.token, s.agent, s.nonce))}
}

// Verify checks the agent's response. Only the first call can succeed.
func (s *ClonecastAuth) Verify(r Response) error {
	first := !s.tested
	s.tested = true
	if !first || !macMatches(r.MAC, agentMAC(s.token, s.nonce, s.agent)) {
		return ErrAgentUnproven
	}
	return nil
}

// Run is clonecast's side after the hello: it writes the challenge to w and
// reads the response from sc. ErrAgentUnproven is the refusal; any other
// error is a connection that ended or sent something else.
func (s *ClonecastAuth) Run(sc *bufio.Scanner, w io.Writer) error {
	if err := s.Challenge().Write(w); err != nil {
		return err
	}
	for sc.Scan() {
		msg, err := Decode(sc.Text())
		if err != nil {
			return fmt.Errorf("instead of a response: %w", err)
		}
		switch msg.Kind {
		case KindUnknown, KindKeepalive:
			continue
		case KindResponse:
			return s.Verify(msg.Response)
		default:
			return fmt.Errorf("the agent sent a %s instead of a response", msg.Kind)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("no response to the challenge: %w", err)
	}
	return errors.New("no response to the challenge: the connection was closed")
}

// Complaints is how the agent logs a failed handshake once, not once per
// redial: an agent installed by another clonecast redials with backoff for as
// long as the prefix runs. Reset after a handshake that succeeds, so the next
// failure is logged again.
type Complaints struct {
	seen map[string]bool
}

// First reports whether err is new since the last Reset.
func (c *Complaints) First(err error) bool {
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	k := complaintKey(err)
	if c.seen[k] {
		return false
	}
	c.seen[k] = true
	return true
}

// Reset forgets what was logged.
func (c *Complaints) Reset() { c.seen = nil }

// complaintKey groups errors that differ only in detail such as the ports
// in a read error.
func complaintKey(err error) string {
	var vm *VersionMismatch
	var nc *NoChallengeError
	switch {
	case errors.As(err, &vm):
		return vm.Error()
	case errors.As(err, &nc):
		return "no challenge"
	case errors.Is(err, ErrClonecastUnproven):
		return "unproven"
	}
	return err.Error()
}
