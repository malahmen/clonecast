package main

import (
	"testing"
	"time"
)

// askByKeypress prints keypressTimeout with %s. As a bare 0 on non-Linux it
// was an int, so vet failed on darwin and windows (Kamino 139).
func TestKeypressTimeoutIsADuration(t *testing.T) {
	if _, ok := any(keypressTimeout).(time.Duration); !ok {
		t.Fatalf("keypressTimeout is %T, want time.Duration", keypressTimeout)
	}
}
