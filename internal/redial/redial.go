// Package redial holds the agent's reconnect policy.
//
// Its own package rather than a file in cmd/clonecast-agent because that
// package is //go:build windows: an unconstrained file there leaves `go build
// ./...` on the build host with a main package and no func main. Nothing in
// this policy is Windows-specific, and here it is unit-testable on the
// machine the agent is cross-compiled from — there is no Wine on the build
// host to run a windows/386 test binary under.
package redial

import "time"

const (
	Min = 500 * time.Millisecond
	Max = 30 * time.Second
	// MinSession is how long a connection must last to count as a real
	// session rather than an immediate rejection.
	//
	// Below it, the dial succeeded but nothing useful happened: a version
	// mismatch, or something else listening on clonecast's port that accepts
	// and hangs up. Both come back instantly and forever.
	MinSession = 5 * time.Second
)

// Next decides how long to wait after a connection ended, and whether
// the backoff should start over.
//
// The bug it fixes: the backoff was reset as soon as a DIAL succeeded, before
// the session ran. A session that ended at once therefore redialled with no
// sleep at all — a tight loop that spins the CPU, appends to a log nothing
// rotates, and loads the wineserver it shares with the game. The dial working
// is not the same thing as the connection being useful, and only the second
// one has earned a reset.
func Next(lasted, current time.Duration) (wait time.Duration, reset bool) {
	if lasted >= MinSession {
		return Min, true
	}
	wait = current
	if wait < Min {
		wait = Min
	}
	return wait, false
}

// Grow doubles a backoff up to the ceiling, and never returns less than the
// floor — doubling zero is zero, which would be a busy loop. Caught by a test
// rather than in the field: Next floors its result today, so nothing
// currently hands Grow a zero, but a helper whose answer can be "wait no time
// at all" is one caller away from spinning.
func Grow(current time.Duration) time.Duration {
	if current < Min {
		current = Min
	}
	if current >= Max {
		return Max
	}
	next := current * 2
	if next > Max {
		return Max
	}
	return next
}
