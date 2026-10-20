// Package lookup paces a search that keeps failing. The agent looks for its
// game window when a key arrives; while there is none (the game closed, or not
// started yet), searching the whole window tree and writing a log line for
// every key is CPU and log growth for nothing (Kamino item 39). It is its own
// package, platform-neutral, so the pacing is tested on Linux although the
// agent only builds for Windows.
package lookup

import "time"

// Pacer says when to search again after a miss, and counts what was dropped
// in the meantime. Not safe for concurrent use; the agent's resolver holds its
// own lock around it.
type Pacer struct {
	Every time.Duration // the least time between two searches that fail

	missing  bool
	missedAt time.Time
	dropped  int
}

// Due reports whether a search should run now: always, unless the last one
// failed less than Every ago.
func (p *Pacer) Due(now time.Time) bool {
	return !p.missing || now.Sub(p.missedAt) >= p.Every
}

// Missed records a search that found nothing.
func (p *Pacer) Missed(now time.Time) { p.missing, p.missedAt = true, now }

// Drop counts a key dropped for want of a window, and reports whether it is
// the first since the window went missing: the one worth a log line.
func (p *Pacer) Drop() bool {
	p.dropped++
	return p.dropped == 1
}

// Found records a search that succeeded and returns how many keys were
// dropped while there was nothing to find.
func (p *Pacer) Found() int {
	n := p.dropped
	p.missing, p.dropped = false, 0
	return n
}
