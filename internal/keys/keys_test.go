package keys

import "testing"

func TestParse(t *testing.T) {
	for _, in := range []string{"a", "A", "KEY_A", " a "} {
		c, err := Parse(in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if Name(c) != "A" {
			t.Fatalf("Parse(%q) = %s, want A", in, Name(c))
		}
	}
	if _, err := Parse("nosuchkey"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestParseSet(t *testing.T) {
	s, err := ParseSet("a, b c")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Parse("a")
	d, _ := Parse("d")
	if !s.Allows(a) || s.Allows(d) {
		t.Fatalf("set %v: wrong membership", s)
	}
	if s.String() != "A B C" {
		t.Fatalf("String() = %q", s.String())
	}

	all, err := ParseSet("ALL")
	if err != nil || !all.IsAll() || !all.Allows(d) {
		t.Fatalf("all set broken: %v %v", all, err)
	}

	if (Set{}).Allows(a) {
		t.Fatal("zero set must allow nothing")
	}
}

func TestToggle(t *testing.T) {
	a, _ := Parse("a")
	b, _ := Parse("b")
	s := Of(a).Toggle(b).Toggle(a)
	if s.Allows(a) || !s.Allows(b) {
		t.Fatalf("toggle result %v", s)
	}
}
