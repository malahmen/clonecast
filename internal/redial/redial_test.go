package redial

import (
	"testing"
	"time"
)

// A session that lasted long enough is a real one: start over at the floor.
func TestARealSessionResetsTheBackoff(t *testing.T) {
	wait, reset := Next(MinSession, 8*time.Second)
	if !reset {
		t.Fatal("a session at the threshold did not reset the backoff")
	}
	if wait != Min {
		t.Fatalf("wait = %s, want %s", wait, Min)
	}
	if _, reset := Next(time.Hour, Max); !reset {
		t.Fatal("a long session did not reset the backoff")
	}
}

// The bug: a dial that connects and dies at once used to reset the backoff and
// redial with NO sleep.
func TestAnImmediateRejectionStillWaits(t *testing.T) {
	for _, lasted := range []time.Duration{0, time.Millisecond, MinSession - time.Nanosecond} {
		wait, reset := Next(lasted, 2*time.Second)
		if reset {
			t.Fatalf("a %s session reset the backoff", lasted)
		}
		if wait < Min {
			t.Fatalf("a %s session waits %s, want at least %s", lasted, wait, Min)
		}
	}
}

// Repeated instant failures must climb to the ceiling and stop there, not
// Grow without bound.
func TestTheBackoffClimbsAndCaps(t *testing.T) {
	cur := Min
	for i := 0; i < 20; i++ {
		wait, reset := Next(0, cur)
		if reset {
			t.Fatalf("iteration %d reset the backoff", i)
		}
		cur = Grow(wait)
		if cur > Max {
			t.Fatalf("iteration %d exceeded the ceiling: %s", i, cur)
		}
	}
	if cur != Max {
		t.Fatalf("after 20 instant failures the backoff is %s, want %s", cur, Max)
	}
}

// Grow never returns below the floor, so a zero can't turn into a busy loop.
func TestGrowNeverReturnsZero(t *testing.T) {
	if got := Grow(0); got <= 0 {
		t.Fatalf("Grow(0) = %s, want a positive duration", got)
	}
}
