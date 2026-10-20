package broadcast

import (
	"strings"
	"sync"
	"testing"

	"github.com/malahmen/clonecast/internal/keys"
)

// The sink is what puts notices in clonecast's log file. It must get the
// status notices and must NOT get the line every broadcast key produces: that
// would be a record on disk of every key broadcast (REFERENCE.md 8).
func TestNoticeSinkGetsNoticesButNotKeys(t *testing.T) {
	rec := &recorder{active: "origin"}
	src := &fakeSource{ch: make(chan keys.Event, 8)}
	e := newEngine(rec, src)

	var mu sync.Mutex
	var sunk []string
	e.SetNoticeSink(func(n Notice) {
		mu.Lock()
		defer mu.Unlock()
		sunk = append(sunk, n.Text)
	})

	e.SetTargets([]WindowID{"t1", "origin"})
	a, _ := keys.Parse("a")
	e.SetFilter(keys.Of(a))
	e.SetEnabled(true)
	e.Notify("agent registered: Marx", false)
	run(t, e, src, keys.Event{Code: a, State: keys.Down})

	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(sunk, "\n")
	if !strings.Contains(joined, "agent registered: Marx") {
		t.Fatalf("sink missed a status notice; got:\n%s", joined)
	}
	if !strings.Contains(joined, "broadcast ON") {
		t.Fatalf("sink missed the broadcast toggle; got:\n%s", joined)
	}
	if strings.Contains(joined, "target(s)") {
		t.Fatalf("sink received a per-key delivery line; got:\n%s", joined)
	}
	// And the key really was broadcast, so the absence above means something.
	if !strings.Contains(strings.Join(rec.snapshot(), " "), "emit@t1:A/down") {
		t.Fatalf("the key was not broadcast: %v", rec.snapshot())
	}
}
