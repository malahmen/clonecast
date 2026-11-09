package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// Kamino 139: two clonecasts starting at once (or `agent install` beside a
// running one) each saw no token, made one and wrote it, so one of them went
// on with a token the file no longer held and refused agents installed with
// the other. Every caller must come back with the token on disk.
func TestAgentTokenRaceAgreesOnOne(t *testing.T) {
	for round := 0; round < 20; round++ {
		p := filepath.Join(t.TempDir(), "clonecast", tokenName)
		const n = 16
		got := make([]string, n)
		errs := make([]error, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				got[i], errs[i] = loadOrCreateToken(p)
			}(i)
		}
		close(start)
		wg.Wait()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		onDisk := strings.TrimSpace(string(b))
		for i := range got {
			if errs[i] != nil {
				t.Fatalf("round %d caller %d: %v", round, i, errs[i])
			}
			if got[i] != onDisk {
				t.Fatalf("round %d caller %d got %.8s..., the file holds %.8s...", round, i, got[i], onDisk)
			}
		}
		left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "*"))
		if len(left) != 1 {
			t.Fatalf("round %d: the directory holds %v, want only the token", round, left)
		}
	}
}

// An empty token file (a write cut short before writes were atomic) is
// replaced, not trusted.
func TestAgentTokenReplacesAnEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), tokenName)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := loadOrCreateToken(p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if len(tok) != 64 || strings.TrimSpace(string(b)) != tok {
		t.Fatalf("token %q, file %q", tok, b)
	}
}
