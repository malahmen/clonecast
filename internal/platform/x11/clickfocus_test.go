//go:build linux

package x11

import (
	"testing"

	"github.com/jezek/xgb/xproto"
)

// The two failures traced on the box (2026-10-19), and the cases that must be
// left alone.
func TestShouldCorrect(t *testing.T) {
	const malahmen, almerinda xproto.Window = 0x100, 0x200
	cases := []struct {
		name                string
		clicked, activeNow  xproto.Window
		clickedPID, focusPD int
		want                bool
	}{
		{"focus stayed on the client clicked away from", malahmen, malahmen, 73601, 73006, true},
		{"a third client took the focus", malahmen, malahmen, 73601, 74196, true},
		{"focus went nowhere", malahmen, malahmen, 73601, 0, true},
		{"focus followed", malahmen, malahmen, 73601, 73601, false},
		// The user has clicked elsewhere since: that activation decides.
		{"KWin moved on", malahmen, almerinda, 73601, 73006, false},
		{"the property was cleared", malahmen, 0, 73601, 73006, false},
		{"owner of the clicked window unknown", malahmen, malahmen, 0, 73006, false},
	}
	for _, c := range cases {
		if got := shouldCorrect(c.clicked, c.activeNow, c.clickedPID, c.focusPD); got != c.want {
			t.Errorf("%s: shouldCorrect = %v, want %v", c.name, got, c.want)
		}
	}
}
