package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// tokenName is the file in clonecast's config directory holding the secret
// every agent must send in its hello (Kamino item 2). Its own file, readable
// by its owner only, rather than a line of clonecast.conf, which is not.
const tokenName = "agent-token"

func tokenPath() string {
	p := configPath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), tokenName)
}

// agentToken returns the secret, creating it on first use. `agent install`
// writes the same value beside each agent, and the registry refuses an agent
// that does not send it, so only agents this user installed can register.
func agentToken() (string, error) {
	return loadOrCreateToken(tokenPath())
}

// loadOrCreateToken is safe against another clonecast (or `agent install`)
// doing the same at the same moment (Kamino 139). The new token is written
// whole to a temporary file and linked into place: the link fails if the
// path exists, which is O_EXCL's guarantee, and a reader never sees a
// half-written file. The caller that loses the race reads the winner's
// token, so every process ends up with the one on disk.
func loadOrCreateToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("no config directory for the agent token")
	}
	t, err := readToken(path)
	if err != nil || t != "" {
		return t, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	t = hex.EncodeToString(raw)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tokenName+".*") // mode 0600
	if err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(t + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agent token: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}

	switch err := os.Link(tmp.Name(), path); {
	case err == nil:
		return t, nil
	case !errors.Is(err, os.ErrExist):
		return "", fmt.Errorf("agent token: %w", err)
	}
	// Lost the race, or the file is there but empty.
	won, err := readToken(path)
	if err != nil || won != "" {
		return won, err
	}
	// An empty file, left by a write cut short before writes were atomic:
	// replace it whole.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	return readToken(path)
}

// readToken is the token at path, "" when there is none (or the file is
// empty).
func readToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
