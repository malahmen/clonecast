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

func loadOrCreateToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("no config directory for the agent token")
	}
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("agent token: %w", err)
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	t := hex.EncodeToString(raw)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("agent token: %w", err)
	}
	return t, nil
}
