package agentwin

import (
	"testing"
	"time"

	"github.com/malahmen/clonecast/internal/lookup"
)

// fakeWindows is a window list: each window has a title and is visible.
// It counts the tree walks.
type fakeWindows struct {
	titles  map[uintptr]string
	game    uintptr // what the prefix-wide search finds
	byTitle int
	walks   int
}

func (f *fakeWindows) Alive(h uintptr) bool {
	_, ok := f.titles[h]
	return ok
}
func (f *fakeWindows) Visible(h uintptr) bool { return f.Alive(h) }
func (f *fakeWindows) Title(h uintptr) string { return f.titles[h] }

func (f *fakeWindows) ByTitle(title string) uintptr {
	f.byTitle++
	for h, t := range f.titles {
		if t == title {
			return h
		}
	}
	return 0
}

func (f *fakeWindows) Game(uintptr) uintptr {
	f.walks++
	if f.Alive(f.game) {
		return f.game
	}
	return 0
}

func newTracker(title string) *Tracker {
	return &Tracker{
		Title:   title,
		Pace:    lookup.Pacer{Every: time.Second},
		Recheck: Recheck{Every: 5 * time.Second},
	}
}

// Kamino 278: with -title set and no window of that title, the prefix-wide
// fallback window failed the title check on every key, so every key walked
// the tree twice (the title search, then the prefix-wide one). It is now kept
// while it is alive and visible, and the title looked for on the recheck.
func TestTheFallbackWindowIsKeptWithoutAWalkPerKey(t *testing.T) {
	w := &fakeWindows{titles: map[uintptr]string{7: "World of Warcraft"}, game: 7}
	tr := newTracker("WoW")
	t0 := time.Unix(1000, 0)

	if got := tr.Current(t0, w).HWND; got != 7 {
		t.Fatalf("first key went to %#x, want the fallback 0x7", got)
	}
	for i := 1; i <= 50; i++ {
		if got := tr.Current(t0.Add(time.Duration(i)*50*time.Millisecond), w).HWND; got != 7 {
			t.Fatalf("key %d went to %#x, want 0x7", i, got)
		}
	}
	if w.byTitle != 1 || w.walks != 1 {
		t.Fatalf("51 keys within 5 s searched by title %d time(s) and walked %d time(s), want once each", w.byTitle, w.walks)
	}

	// The titled window appears; the recheck finds it.
	w.titles[9] = "WoW"
	res := tr.Current(t0.Add(5*time.Second), w)
	if res.HWND != 9 || res.Replaced != 7 {
		t.Fatalf("recheck: %+v, want the titled window 0x9 replacing 0x7", res)
	}
	if w.byTitle != 2 {
		t.Fatalf("the title was searched %d time(s), want twice: once more on the recheck", w.byTitle)
	}
	// From here the title decides: the titled window is kept, no recheck.
	walks := w.walks
	for i := 1; i <= 200; i++ {
		if got := tr.Current(t0.Add(5*time.Second+time.Duration(i)*50*time.Millisecond), w).HWND; got != 9 {
			t.Fatalf("key %d after the title appeared went to %#x, want 0x9", i, got)
		}
	}
	if w.byTitle != 2 || w.walks != walks {
		t.Fatalf("a titled window still searched: by title %d, walks %d", w.byTitle, w.walks-walks)
	}
}

// The fallback is kept only while it lasts: when it goes, the next key looks
// for the title first again.
func TestAGoneFallbackIsLookedForAgain(t *testing.T) {
	w := &fakeWindows{titles: map[uintptr]string{7: "World of Warcraft"}, game: 7}
	tr := newTracker("WoW")
	t0 := time.Unix(1000, 0)
	tr.Current(t0, w)

	delete(w.titles, 7) // WoW recreates its window
	w.titles[8] = "World of Warcraft"
	w.game = 8
	res := tr.Current(t0.Add(100*time.Millisecond), w)
	if res.HWND != 8 || !res.Searched {
		t.Fatalf("after the fallback went: %+v, want a search finding 0x8", res)
	}
	if w.byTitle != 2 {
		t.Fatalf("the title was searched %d time(s), want again when the fallback went", w.byTitle)
	}
}

// Without a title, a valid window is compared with the others only every
// Recheck.Every, not on every key.
func TestAPrefixWideWindowIsRecheckedOnlyEveryInterval(t *testing.T) {
	w := &fakeWindows{titles: map[uintptr]string{7: "World of Warcraft"}, game: 7}
	tr := newTracker("")
	t0 := time.Unix(1000, 0)
	for i := 0; i < 100; i++ {
		tr.Current(t0.Add(time.Duration(i)*40*time.Millisecond), w) // 4 s
	}
	if w.walks != 1 || w.byTitle != 0 {
		t.Fatalf("100 keys within 4 s walked %d time(s) and searched by title %d, want 1 and 0", w.walks, w.byTitle)
	}
	tr.Current(t0.Add(5*time.Second), w)
	if w.walks != 2 {
		t.Fatalf("walks %d after the recheck interval, want 2", w.walks)
	}
}

// A titled window is checked against its title: a window that loses it (WoW
// renames nothing, but a reused handle could) is searched for again.
func TestATitledWindowThatLosesItsTitleIsSearchedAgain(t *testing.T) {
	w := &fakeWindows{titles: map[uintptr]string{9: "WoW", 7: "other"}, game: 7}
	tr := newTracker("WoW")
	t0 := time.Unix(1000, 0)
	if got := tr.Current(t0, w).HWND; got != 9 {
		t.Fatalf("got %#x, want the titled 0x9", got)
	}
	w.titles[9] = "renamed"
	res := tr.Current(t0.Add(100*time.Millisecond), w)
	if res.HWND != 7 || !res.Searched {
		t.Fatalf("%+v, want a search that falls back to 0x7", res)
	}
}
