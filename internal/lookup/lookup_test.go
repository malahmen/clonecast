package lookup

import (
	"testing"
	"time"
)

func TestPacerSearchesAtMostEveryIntervalWhileMissing(t *testing.T) {
	p := Pacer{Every: time.Second}
	t0 := time.Unix(1000, 0)
	if !p.Due(t0) {
		t.Fatal("first search not due")
	}
	p.Missed(t0)
	if p.Due(t0.Add(500 * time.Millisecond)) {
		t.Error("searched again half a second after a miss")
	}
	if !p.Due(t0.Add(time.Second)) {
		t.Error("not due a second after a miss")
	}
}

func TestPacerLogsTheFirstDropAndCountsTheRest(t *testing.T) {
	p := Pacer{Every: time.Second}
	p.Missed(time.Unix(1000, 0))
	if !p.Drop() {
		t.Error("the first drop is not reported")
	}
	for i := 0; i < 4; i++ {
		if p.Drop() {
			t.Error("a later drop is reported")
		}
	}
	if n := p.Found(); n != 5 {
		t.Errorf("Found = %d dropped, want 5", n)
	}
	if !p.Due(time.Unix(1000, 1)) {
		t.Error("not due after the window was found")
	}
	if !p.Drop() {
		t.Error("the first drop of the next outage is not reported")
	}
}
