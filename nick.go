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
//  3. $CLAUDE_AGENT_CHAT_NICK env var (the launch var the join hook honours), sanitized
//  4. ~/.agent-chat/by-cwd/<sha256(git-root-or-cwd)>.nick claim
//  5. directory re-derivation, the same way the join hook derives (git root
//     basename, then .agent-chat-nick), sanitized
//  6. ~/.config/agent-chat/nick (or $XDG_CONFIG_HOME/agent-chat/nick)
//
// There is deliberately no $USER tier: every caller of resolveNick is an
// agent-facing verb, and a session whose by-cwd claim was torn down must
// re-derive its own nick (tiers 3/5) or fail loudly — never silently relabel
// its traffic as the human. The human-facing verbs (chat, watch) go through
// resolveNickHuman, which appends the $USER fallback.
func resolveNick(asFlag string) (string, error) {
	if s, ok := resolveNickCommon(asFlag); ok {
		return s, nil
	}
	return "", errNoNick
}

// resolveNickHuman is resolveNick plus a final $USER fallback, for the verbs
// a human runs interactively (chat, watch) where defaulting to the username
// is identity, not impersonation.
func resolveNickHuman(asFlag string) (string, error) {
	if s, ok := resolveNickCommon(asFlag); ok {
		return s, nil
	}
	if s := strings.TrimSpace(os.Getenv("USER")); s != "" {
		return s, nil
	}
	return "", errNoNick
}

var errNoNick = fmt.Errorf("could not resolve nick (no --as, $AGENT_CHAT_NICK, $CLAUDE_AGENT_CHAT_NICK, by-cwd claim, git-root/.agent-chat-nick derivation, or ~/.config/agent-chat/nick). " +
	"If this is a Claude Code session that failed to join, restart with CLAUDE_AGENT_CHAT_NICK=<nick>; for a manual run, pass --as <nick>")

func resolveNickCommon(asFlag string) (string, bool) {
	if s := strings.TrimSpace(asFlag); s != "" {
		return s, true
	}
	if s := strings.TrimSpace(os.Getenv("AGENT_CHAT_NICK")); s != "" {
		return s, true
	}
	if s := sanitizeNick(os.Getenv("CLAUDE_AGENT_CHAT_NICK")); s != "" {
		return s, true
	}
	if s, ok := readByCwd(); ok {
		return s, true
	}
	cwd, _ := os.Getwd()
	if raw, _ := deriveDirNick(cwd); raw != "" {
		if s := sanitizeNick(raw); s != "" {
			return s, true
		}
	}
	if s, ok := readConfigNick(); ok {
		return s, true
	}
	return "", false
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
	nick, _, ok := readClaimFile(byCwdPath())
	return nick, ok
}

// A by-cwd claim file holds the nick on its first line and, when the claim
// was written by a Claude Code session, the owning session id on the second.
// The owner stamp is what lets hook-stop tell its own claim apart from one
// written by a different session sharing the directory (or one whose cwd it
// was relocated into after a worktree removal). Legacy files hold only the
// nick; their owner parses as "".
func parseClaim(b []byte) (nick, owner string) {
	lines := strings.SplitN(string(b), "\n", 3)
	nick = strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		owner = strings.TrimSpace(lines[1])
	}
	return nick, owner
}

func readClaimFile(p string) (nick, owner string, ok bool) {
	if p == "" {
		return "", "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", "", false
	}
	nick, owner = parseClaim(b)
	if nick == "" {
		return "", "", false
	}
	return nick, owner, true
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
