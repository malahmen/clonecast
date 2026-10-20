package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentTokenIsCreatedOnceAndPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "clonecast", tokenName)
	first, err := loadOrCreateToken(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Errorf("token %d characters long, want 64", len(first))
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode %o, want 600", perm)
	}
	again, err := loadOrCreateToken(p)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Error("a second call made a new token; installed agents would be refused")
	}
}
