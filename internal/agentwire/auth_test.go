package agentwire

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// The plan's test vector, computed outside Go (Python's hmac): it pins the
// message format, domain prefixes and argument order included, which both
// ends must agree on byte for byte (Kamino 275).
func TestMACKnownAnswer(t *testing.T) {
	a, c := "00112233445566778899aabbccddeeff", "ffeeddccbbaa99887766554433221100"
	if got := hexOf(clonecastMAC("tok", a, c)); got != "98870f8b46447e8a561738ecf61cb84e2713de207388594e302f963760e90c51" {
		t.Errorf("mac1 = %s", got)
	}
	if got := hexOf(agentMAC("tok", c, a)); got != "3d4b884555fdfa4b5301aab2824fff5086b284fb62ecb61083f91bfdbbb937b9" {
		t.Errorf("mac2 = %s", got)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	var s strings.Builder
	for _, x := range b {
		s.WriteByte(digits[x>>4])
		s.WriteByte(digits[x&15])
	}
	return s.String()
}

// pair is one connection: the agent's end and clonecast's.
func pair(t *testing.T) (agent, clonecast net.Conn) {
	t.Helper()
	a, c := net.Pipe()
	deadline := time.Now().Add(3 * time.Second)
	_ = a.SetDeadline(deadline)
	_ = c.SetDeadline(deadline)
	t.Cleanup(func() { _ = a.Close(); _ = c.Close() })
	return a, c
}

// agentSide writes a v3 hello and runs the agent's handshake, as the agent's
// session does, and closes its end on an error, as the session does.
func agentSide(conn net.Conn, token string) error {
	auth, err := NewAgentAuth(token, rand.Reader)
	if err != nil {
		return err
	}
	if err := (Hello{Version: 3, Title: "Game", Nonce: auth.Nonce()}).Write(conn); err != nil {
		return err
	}
	err = auth.Run(bufio.NewScanner(conn), conn)
	if err != nil {
		_ = conn.Close()
	}
	return err
}

// clonecastSide reads the hello and runs clonecast's handshake.
func clonecastSide(conn net.Conn, token string) error {
	sc := bufio.NewScanner(conn)
	if !sc.Scan() {
		return errors.New("no hello")
	}
	msg, err := Decode(sc.Text())
	if err != nil {
		return err
	}
	auth, err := NewClonecastAuth(token, msg.Hello, rand.Reader)
	if err != nil {
		return err
	}
	return auth.Run(sc, conn)
}

func TestHandshakeSameToken(t *testing.T) {
	a, c := pair(t)
	done := make(chan error, 1)
	go func() { done <- agentSide(a, "s3cret") }()
	if err := clonecastSide(c, "s3cret"); err != nil {
		t.Fatalf("clonecast: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("agent: %v", err)
	}
}

// The token never goes on the wire in v3: everything the agent sends is
// recorded and searched for it.
func TestHandshakeNeverSendsTheToken(t *testing.T) {
	a, c := pair(t)
	var wire bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- clonecastSide(c, "s3cret-token") }()
	auth, _ := NewAgentAuth("s3cret-token", rand.Reader)
	_ = (Hello{Version: 3, Title: "Game", Nonce: auth.Nonce()}).Write(io.MultiWriter(a, &wire))
	if err := auth.Run(bufio.NewScanner(a), io.MultiWriter(a, &wire)); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wire.String(), "s3cret-token") || strings.Contains(wire.String(), "token=") {
		t.Fatalf("the agent sent the token: %q", wire.String())
	}
}

// A fake clonecast without the right token never gets an R: the agent checks
// the challenge first, sends nothing, and hangs up.
func TestFakeClonecastNeverReceivesTheResponse(t *testing.T) {
	a, c := pair(t)
	done := make(chan error, 1)
	go func() { done <- agentSide(a, "s3cret") }()

	sc := bufio.NewScanner(c)
	if !sc.Scan() {
		t.Fatal("no hello")
	}
	msg, err := Decode(sc.Text())
	if err != nil {
		t.Fatal(err)
	}
	fake, _ := NewClonecastAuth("guess", msg.Hello, rand.Reader)
	if err := fake.Challenge().Write(c); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrClonecastUnproven) {
		t.Fatalf("agent: err = %v, want ErrClonecastUnproven", err)
	}
	if sc.Scan() {
		t.Fatalf("the fake clonecast received %q", sc.Text())
	}
}

// A fake clonecast echoing the agent's own nonce, or sending garbage, gets
// nothing either.
func TestAgentRefusesAReflectedOrMalformedChallenge(t *testing.T) {
	auth, _ := NewAgentAuth("s3cret", rand.Reader)
	reflected := Challenge{Nonce: auth.Nonce(), MAC: hexOf(clonecastMAC("s3cret", auth.Nonce(), auth.Nonce()))}
	if _, err := auth.Answer(reflected); !errors.Is(err, ErrClonecastUnproven) {
		t.Errorf("reflected nonce: err = %v", err)
	}
	if _, err := auth.Answer(Challenge{Nonce: "zz", MAC: "zz"}); !errors.Is(err, ErrClonecastUnproven) {
		t.Errorf("malformed: err = %v", err)
	}
	// A real one still works, once.
	good := Challenge{Nonce: strings.Repeat("ab", 16)}
	good.MAC = hexOf(clonecastMAC("s3cret", auth.Nonce(), good.Nonce))
	if _, err := auth.Answer(good); err != nil {
		t.Fatalf("good challenge: %v", err)
	}
	if _, err := auth.Answer(good); !errors.Is(err, ErrClonecastUnproven) {
		t.Errorf("a second challenge on one connection was answered")
	}
}

func TestFakeAgentIsRefused(t *testing.T) {
	a, c := pair(t)
	done := make(chan error, 1)
	go func() { done <- clonecastSide(c, "s3cret") }()

	// The fake has no token, so it cannot check the challenge; it answers
	// with what it can compute.
	auth, _ := NewAgentAuth("guess", rand.Reader)
	_ = (Hello{Version: 3, Nonce: auth.Nonce()}).Write(a)
	sc := bufio.NewScanner(a)
	if !sc.Scan() {
		t.Fatal("no challenge")
	}
	msg, _ := Decode(sc.Text())
	_ = Response{MAC: hexOf(agentMAC("guess", msg.Challenge.Nonce, auth.Nonce()))}.Write(a)
	if err := <-done; !errors.Is(err, ErrAgentUnproven) {
		t.Fatalf("clonecast: err = %v, want ErrAgentUnproven", err)
	}
}

// A response recorded from one connection is refused on the next: clonecast's
// nonce is fresh each time, even for a replayed hello. A second response on
// the same connection is refused too.
func TestReplayedResponseIsRefused(t *testing.T) {
	h := Hello{Version: 3, Nonce: strings.Repeat("01", 16)}
	first, _ := NewClonecastAuth("s3cret", h, rand.Reader)
	ch := first.Challenge()
	recorded := Response{MAC: hexOf(agentMAC("s3cret", ch.Nonce, h.Nonce))}
	if err := first.Verify(recorded); err != nil {
		t.Fatalf("the genuine response: %v", err)
	}
	if err := first.Verify(recorded); !errors.Is(err, ErrAgentUnproven) {
		t.Errorf("the same response twice on one connection: err = %v", err)
	}
	second, _ := NewClonecastAuth("s3cret", h, rand.Reader)
	if second.Challenge().Nonce == ch.Nonce {
		t.Fatal("clonecast reused its nonce")
	}
	if err := second.Verify(recorded); !errors.Is(err, ErrAgentUnproven) {
		t.Errorf("a replayed response: err = %v, want ErrAgentUnproven", err)
	}
}

func TestClonecastAuthNeedsATokenAndANonce(t *testing.T) {
	if _, err := NewClonecastAuth("", Hello{Version: 3, Nonce: strings.Repeat("01", 16)}, rand.Reader); !errors.Is(err, ErrNoToken) {
		t.Errorf("no token: err = %v", err)
	}
	for _, n := range []string{"", "abc", strings.Repeat("zz", 16), strings.Repeat("01", 17)} {
		if _, err := NewClonecastAuth("s3cret", Hello{Version: 3, Nonce: n}, rand.Reader); !errors.Is(err, ErrBadNonce) {
			t.Errorf("nonce %q: err = %v", n, err)
		}
	}
	if _, err := NewAgentAuth("", rand.Reader); !errors.Is(err, ErrNoToken) {
		t.Errorf("agent with no token: err = %v", err)
	}
}

// The mismatch messages, word for word: they are what a user reads to fix it.
func TestVersionMismatchMessages(t *testing.T) {
	for _, c := range []struct {
		e    VersionMismatch
		want string
	}{
		{VersionMismatch{Clonecast: 3, Agent: 2}, "this agent speaks protocol 2: reinstall it, or set the agent protocol to 2"},
		{VersionMismatch{Clonecast: 2, Agent: 3}, "this agent speaks protocol 3: set the agent protocol to 3, or reinstall it with --protocol 2"},
		{VersionMismatch{Clonecast: 2, Agent: 3, AgentSide: true}, "clonecast speaks protocol 2: set its agent protocol to 3, or reinstall this agent with --protocol 2"},
		{VersionMismatch{Clonecast: 3, Agent: 2, AgentSide: true}, "clonecast speaks protocol 3: reinstall this agent (it is set to 2), or set clonecast's agent protocol to 2"},
	} {
		if got := c.e.Error(); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.e, got, c.want)
		}
	}
}

// clonecast accepts only the version it is set to: a v2 and a v3 hello both
// parse, and each is refused under the other setting.
func TestCheckVersion(t *testing.T) {
	for _, line := range []string{"H 2 title=Game token=x", "H 3 title=Game nonce=" + strings.Repeat("01", 16)} {
		msg, err := Decode(line)
		if err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		for _, setting := range []int{2, 3} {
			err := CheckVersion(setting, msg.Hello)
			if (setting == msg.Hello.Version) != (err == nil) {
				t.Errorf("%q under %d: err = %v", line, setting, err)
			}
			var vm *VersionMismatch
			if err != nil && (!errors.As(err, &vm) || vm.Agent != msg.Hello.Version || vm.Clonecast != setting) {
				t.Errorf("%q under %d: err = %#v", line, setting, err)
			}
		}
	}
}

// A v3 agent meets a clonecast set to 2 (a V line), or one older than v3 (it
// hangs up on the hello): both say what to change.
func TestAgentRunWithoutAChallenge(t *testing.T) {
	auth, _ := NewAgentAuth("s3cret", rand.Reader)
	var out bytes.Buffer
	err := auth.Run(bufio.NewScanner(strings.NewReader("V 2\n")), &out)
	var vm *VersionMismatch
	if !errors.As(err, &vm) || vm.Clonecast != 2 || !vm.AgentSide {
		t.Fatalf("V 2: err = %v", err)
	}
	err = auth.Run(bufio.NewScanner(strings.NewReader("")), &out)
	var nc *NoChallengeError
	if !errors.As(err, &nc) || !strings.Contains(err.Error(), "protocol 2") {
		t.Fatalf("closed: err = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("the agent sent %q without a challenge", out.String())
	}
}

// The agent's read deadline reaches the handshake: a clonecast that says
// nothing is a timeout, reported as no challenge.
func TestAgentRunTimesOut(t *testing.T) {
	a, _ := pair(t)
	_ = a.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	auth, _ := NewAgentAuth("s3cret", rand.Reader)
	err := auth.Run(bufio.NewScanner(a), io.Discard)
	var nc *NoChallengeError
	var ne net.Error
	if !errors.As(err, &nc) || !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("err = %v, want a NoChallengeError wrapping a timeout", err)
	}
}

func TestParseProtocol(t *testing.T) {
	for s, want := range map[string]int{"2": 2, "3": 3} {
		if got, err := ParseProtocol(s); err != nil || got != want {
			t.Errorf("%q = %d, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "1", "4", "x"} {
		if _, err := ParseProtocol(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if DefaultProtocol != 3 {
		t.Errorf("DefaultProtocol = %d, want 3", DefaultProtocol)
	}
}

// The agent logs each kind of failure once until a handshake succeeds, even
// when the read errors differ in their ports.
func TestComplaintsOncePerKind(t *testing.T) {
	var c Complaints
	timeout1 := &NoChallengeError{Err: errors.New("read tcp 127.0.0.1:1->127.0.0.1:48800: i/o timeout")}
	timeout2 := &NoChallengeError{Err: errors.New("read tcp 127.0.0.1:2->127.0.0.1:48800: i/o timeout")}
	if !c.First(timeout1) || c.First(timeout2) {
		t.Error("two timeouts were not one complaint")
	}
	if !c.First(ErrClonecastUnproven) || c.First(ErrClonecastUnproven) {
		t.Error("a bad challenge was not logged exactly once")
	}
	if !c.First(&VersionMismatch{Clonecast: 2, Agent: 3, AgentSide: true}) {
		t.Error("a mismatch was hidden by another complaint")
	}
	c.Reset()
	if !c.First(ErrClonecastUnproven) {
		t.Error("Reset did not let the complaint through again")
	}
}

// The agent's hello carries the token at protocol 2 only (Kamino 275).
func TestAgentHelloCarriesTheTokenOnlyAtV2(t *testing.T) {
	h := Hello{Title: "Game", Token: "stale"}
	if got := AgentHello(3, "s3cret", h); got.Token != "" || got.Version != 3 {
		t.Errorf("v3 hello = %+v, want no token", got)
	}
	if got := AgentHello(2, "s3cret", h); got.Token != "s3cret" || got.Version != 2 {
		t.Errorf("v2 hello = %+v, want the token", got)
	}
}
