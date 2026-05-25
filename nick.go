package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// resolveNick walks the runtime resolver decision tree from the design doc
// (§Identity → "At runtime — unified nick resolver"). First match wins:
//
//  1. --as NICK flag
//  2. $AGENT_CHAT_NICK env var
//  3. ~/.agent-chat/by-cwd/<sha256(git-root-or-cwd)>.nick lookup
//  4. ~/.config/agent-chat/nick (or $XDG_CONFIG_HOME/agent-chat/nick)
//  5. $USER
//
// Returns a descriptive error pointing at the env-var override when nothing
// resolves.
func resolveNick(asFlag string) (string, error) {
	if s := strings.TrimSpace(asFlag); s != "" {
		return s, nil
	}
	if s := strings.TrimSpace(os.Getenv("AGENT_CHAT_NICK")); s != "" {
		return s, nil
	}
	if s, ok := readByCwd(); ok {
		return s, nil
	}
	if s, ok := readConfigNick(); ok {
		return s, nil
	}
	if s := strings.TrimSpace(os.Getenv("USER")); s != "" {
		return s, nil
	}
	return "", fmt.Errorf("could not resolve nick (no --as, $AGENT_CHAT_NICK, by-cwd entry, ~/.config/agent-chat/nick, or $USER). " +
		"If this is a Claude Code session that failed to join, restart with CLAUDE_AGENT_CHAT_NICK=<nick>")
}

// cwdKey returns the directory used to key the by-cwd lookup: the git
// top-level when inside a repo, otherwise the current working directory.
func cwdKey() string {
	if root, err := gitRoot(); err == nil && root != "" {
		return root
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func byCwdPath() string {
	key := cwdKey()
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(chatHome(), "by-cwd", hex.EncodeToString(sum[:])+".nick")
}

func readByCwd() (string, bool) {
	p := byCwdPath()
	if p == "" {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", false
	}
	return s, true
}

func configNickPath() string {
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		return filepath.Join(v, "agent-chat", "nick")
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, ".config", "agent-chat", "nick")
}

func readConfigNick() (string, bool) {
	p := configNickPath()
	if p == "" {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	for _, line := range bytes.Split(b, []byte("\n")) {
		if s := strings.TrimSpace(string(line)); s != "" {
			return s, true
		}
	}
	return "", false
}
