package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --emit text prints the bare kilo primer (no hook envelope), exits 0, and
// writes a join record — the path the kilo plugin drives.
func TestHookStartEmitTextJoins(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "text"}) })
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	if json.Valid([]byte(strings.TrimSpace(out))) {
		t.Errorf("text mode must not emit the JSON hook envelope:\n%s", out)
	}
	if !strings.Contains(out, "ambient context") {
		t.Errorf("expected the passive kilo primer; got:\n%s", out)
	}
	for _, banned := range []string{"REQUIRED FIRST ACTION", "Monitor("} {
		if strings.Contains(out, banned) {
			t.Errorf("kilo primer must not contain %q (the plugin owns the listener):\n%s", banned, out)
		}
	}
	if !joinRecorded(t, home, "alice") {
		t.Errorf("hook-start --emit text should write a join record for alice")
	}
}

// The default (Claude) mode is unchanged: it still wraps the primer in the hook
// envelope and instructs the Monitor.
func TestHookStartDefaultStillEmitsEnvelope(t *testing.T) {
	cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	primer := parsePrimer(t, out) // fails if not the envelope
	if !strings.Contains(primer, "REQUIRED FIRST ACTION") {
		t.Errorf("claude primer must still instruct the Monitor:\n%s", primer)
	}
}

// A nick held by another active cwd must NOT join. In text mode this is
// signalled with exit code 3 so the plugin knows not to start a listener that
// would hijack the live owner's inbox.
func TestHookStartEmitTextCollisionExits3(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	claimNickFromForeignCwd(t, home, "alice")

	_, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "text"}) })
	if rc != 3 {
		t.Fatalf("text-mode collision rc = %d, want 3", rc)
	}
	if joinRecorded(t, home, "alice") {
		t.Errorf("a colliding session must not write a join record")
	}
}

// The same collision in the default mode keeps the legacy contract: rc 0 with
// the NOT-JOINED envelope (the plugin's exit-3 signal is text-mode only).
func TestHookStartDefaultCollisionExits0(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	claimNickFromForeignCwd(t, home, "alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("default-mode collision rc = %d, want 0", rc)
	}
	if primer := parsePrimer(t, out); !strings.Contains(primer, "NOT JOINED") {
		t.Errorf("expected NOT JOINED envelope; got:\n%s", primer)
	}
}

// Opt-out (CLAUDE_AGENT_CHAT=0) produces no output and exits 0 in text mode, so
// the plugin's empty-primer guard skips the session.
func TestHookStartEmitTextOptOut(t *testing.T) {
	cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	t.Setenv("CLAUDE_AGENT_CHAT", "0")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "text"}) })
	if rc != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("opt-out: rc=%d out=%q, want rc=0 and empty", rc, out)
	}
}

// --emit json returns the passive primer plus the capped missed mentions the
// plugin injects as a catch-up turn.
func TestHookStartEmitJSONCapsMissed(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "demo")
	// Five mentions, cursor at start => all five are "missed".
	var lines []string
	for i := 1; i <= 5; i++ {
		lines = append(lines, fmt.Sprintf(`{"ts":%d.000,"from":"peer","to":"@demo","text":"m%d"}`, 1700000000+i, i))
	}
	writeLog(t, home, lines...)
	if err := writeCursor("demo", 0); err != nil {
		t.Fatal(err)
	}

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "json"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	var got kiloHookOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid json: %v\n%s", err, out)
	}
	if !strings.Contains(got.Primer, "ambient context") {
		t.Errorf("primer missing from json: %q", got.Primer)
	}
	if len(got.Missed) != missedPreviewMax {
		t.Errorf("missed len = %d, want %d (capped)", len(got.Missed), missedPreviewMax)
	}
	if got.MoreHint == "" {
		t.Errorf("expected a moreHint pointing at history for the overflow")
	}
}

// A fresh join has no backlog: missed/moreHint are empty/omitted.
func TestHookStartEmitJSONFreshNoMissed(t *testing.T) {
	cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "demo")

	out, _ := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "json"}) })
	var got kiloHookOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid json: %v\n%s", err, out)
	}
	if len(got.Missed) != 0 || got.MoreHint != "" {
		t.Errorf("fresh join should carry no missed/hint: %+v", got)
	}
}

func TestHookStartEmitJSONCollisionExits3(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	claimNickFromForeignCwd(t, home, "alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "json"}) })
	if rc != 3 {
		t.Fatalf("json collision rc = %d, want 3", rc)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("collision should emit nothing on stdout; got %q", out)
	}
}

func TestMissedSectionUnderCapReturnsAll(t *testing.T) {
	in := []string{lineAliceBob, lineBobAlice}
	shown, hint := missedSection(in)
	if len(shown) != 2 || hint != "" {
		t.Errorf("under cap: shown=%d hint=%q, want 2 and empty", len(shown), hint)
	}
}

// claimNickFromForeignCwd makes `nick` look claimed by a different, currently
// active session: a by-cwd entry under a key other than ours plus a fresh log
// record from that nick (so recentActivity treats it as alive, not stale).
func claimNickFromForeignCwd(t *testing.T, home, nick string) {
	t.Helper()
	dir := filepath.Join(home, "by-cwd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "foreign.nick"), []byte(nick), 0o644); err != nil {
		t.Fatal(err)
	}
	recent := fmt.Sprintf(`{"ts":%d.000,"from":%q,"to":"*","text":"alive"}`, time.Now().Unix(), nick)
	writeLog(t, home, recent)
}

// joinRecorded reports whether the log holds a join event for nick.
func joinRecorded(t *testing.T, home, nick string) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, "log.jsonl"))
	if err != nil {
		return false
	}
	for _, ln := range strings.Split(string(b), "\n") {
		var r Record
		if json.Unmarshal([]byte(ln), &r) == nil && r.From == nick && r.Event == "joined" {
			return true
		}
	}
	return false
}
