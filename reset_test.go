package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// recordTsMs formats an absolute epoch-ms time into the ts string the
// wire format uses ("<sec>.<ms>" with 3-decimal precision).
func recordTsMs(t time.Time) string {
	ms := t.UnixMilli()
	return fmtMs(ms)
}

// foreignByCwdHash returns a sha256 hex of a synthetic "other-cwd" key,
// distinct from the current test cwd's key.
func foreignByCwdHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// writeForeignByCwd plants a by-cwd entry under a hash key that is NOT
// the current process's cwd — simulating another active session.
func writeForeignByCwd(t *testing.T, home, foreignKey, nick string) string {
	t.Helper()
	dir := filepath.Join(home, "by-cwd")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, foreignByCwdHash(foreignKey)+".nick")
	if err := os.WriteFile(p, []byte(nick), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHookStartCollisionRecentActivityEmitsNotJoined(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	foreign := writeForeignByCwd(t, home, "/some/other/repo", "alice")
	// Recent traffic from alice — within the stale window.
	writeLog(t, home,
		`{"ts":`+recordTsMs(time.Now().Add(-5*time.Minute))+`,"from":"alice","to":"@bob","text":"still here"}`,
	)

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}
	primer := parsePrimer(t, out)
	if !strings.Contains(primer, "NOT JOINED") {
		t.Errorf("primer missing NOT JOINED banner:\n%s", primer)
	}
	if !strings.Contains(primer, "CLAUDE_AGENT_CHAT_NICK=alice-2") {
		t.Errorf("primer missing suffixed hint:\n%s", primer)
	}

	// Foreign claim is untouched; no by-cwd written for the colliding session.
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign by-cwd entry should still exist: %v", err)
	}
	if _, ok := readByCwd(); ok {
		t.Errorf("colliding session should not have written its own by-cwd entry")
	}

	// No joined line should have been appended.
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	for _, l := range lines {
		if strings.Contains(l, `"event":"joined"`) {
			t.Errorf("collision path should not append joined: %s", l)
		}
	}
}

func TestHookStartCollisionStaleReclaims(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	foreign := writeForeignByCwd(t, home, "/some/other/repo", "alice")
	// Old traffic — well outside the 30-min window.
	writeLog(t, home,
		`{"ts":`+recordTsMs(time.Now().Add(-2*time.Hour))+`,"from":"alice","event":"joined"}`,
	)
	// Plant an agents/alice dir so we can confirm it's cleaned up.
	staleAgentDir := filepath.Join(home, "agents", "alice")
	if err := os.MkdirAll(staleAgentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleAgentDir, "cursor"), []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}
	primer := parsePrimer(t, out)
	if !strings.Contains(primer, "joined as `alice`") {
		t.Errorf("expected normal join primer after stale reclaim:\n%s", primer)
	}

	// Foreign claim should have been cleared.
	if _, err := os.Stat(foreign); !os.IsNotExist(err) {
		t.Errorf("foreign by-cwd entry should have been removed: err=%v", err)
	}
	// New claim should exist for our cwd.
	got, ok := readByCwd()
	if !ok || got != "alice" {
		t.Errorf("by-cwd should now be claimed by us, got %q ok=%v", got, ok)
	}

	// Log should contain a synthetic quit followed by the new joined.
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	var quitIdx, joinIdx int = -1, -1
	for i, l := range lines {
		if strings.Contains(l, `"event":"quit"`) && strings.Contains(l, `"from":"alice"`) && quitIdx == -1 {
			quitIdx = i
		}
		if strings.Contains(l, `"event":"joined"`) && strings.Contains(l, `"from":"alice"`) {
			joinIdx = i
		}
	}
	if quitIdx == -1 {
		t.Errorf("synthetic quit not appended:\n%v", lines)
	}
	if joinIdx == -1 || joinIdx <= quitIdx {
		t.Errorf("joined should follow synthetic quit (quit=%d join=%d):\n%v", quitIdx, joinIdx, lines)
	}

	// The new cursor exists (fresh — pointing past the new joined).
	if _, err := os.Stat(filepath.Join(home, "agents", "alice", "cursor")); err != nil {
		t.Errorf("new cursor not initialized: %v", err)
	}
}

func TestHookStartSameCwdReclaimIsNotACollision(t *testing.T) {
	// Re-running hook-start in the same cwd with the same nick must succeed,
	// not trigger the NOT JOINED path.
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	if _, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) }); rc != 0 {
		t.Fatalf("first hook-start rc = %d", rc)
	}

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("second hook-start rc = %d", rc)
	}
	primer := parsePrimer(t, out)
	if strings.Contains(primer, "NOT JOINED") {
		t.Errorf("same-cwd re-join must not be flagged as collision:\n%s", primer)
	}
	// The by-cwd entry should still claim alice for this cwd.
	if got, _ := readByCwd(); got != "alice" {
		t.Errorf("by-cwd lost: %q", got)
	}
	_ = home
}

func TestResetEmitsQuitAndClearsState(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	if _, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) }); rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}

	if rc := run([]string{"reset", "alice"}); rc != 0 {
		t.Fatalf("reset rc = %d", rc)
	}

	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"event":"quit"`) || !strings.Contains(last, `"from":"alice"`) {
		t.Errorf("expected trailing alice quit, got: %s", last)
	}
	if _, ok := readByCwd(); ok {
		t.Errorf("by-cwd should be removed after reset")
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "alice")); !os.IsNotExist(err) {
		t.Errorf("agents/alice should be removed after reset: err=%v", err)
	}
}

func TestResetAcceptsAtPrefix(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	if _, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) }); rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}
	if rc := run([]string{"reset", "@alice"}); rc != 0 {
		t.Fatalf("reset @alice rc = %d", rc)
	}
	if _, ok := readByCwd(); ok {
		t.Errorf("by-cwd should be removed after reset @alice")
	}
	_ = home
}

func TestResetNoArgUsesResolvedNick(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	if _, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) }); rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}
	// hook-start wrote by-cwd which the resolver consults — so reset with
	// no arg should resolve to alice.
	t.Setenv("AGENT_CHAT_NICK", "alice")
	if rc := run([]string{"reset"}); rc != 0 {
		t.Fatalf("reset rc = %d", rc)
	}
	if _, ok := readByCwd(); ok {
		t.Errorf("by-cwd should be removed when reset uses resolved nick")
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"event":"quit"`) || !strings.Contains(last, `"from":"alice"`) {
		t.Errorf("expected alice quit, got: %s", last)
	}
}

func TestResetUnclaimedNickIsNoOp(t *testing.T) {
	home, _ := cleanHookEnv(t)
	// No hook-start was run; nick is unclaimed.
	if rc := run([]string{"reset", "alice"}); rc != 0 {
		t.Fatalf("reset rc = %d, want 0 for unclaimed nick", rc)
	}
	if _, err := os.Stat(filepath.Join(home, "log.jsonl")); !os.IsNotExist(err) {
		t.Errorf("reset on unclaimed nick must not create log: %v", err)
	}
}

func TestStaleWindowTunable(t *testing.T) {
	// Confirm the constant is the single tuning knob: shrinking it lets the
	// recent-activity check return false for traffic that would otherwise
	// count as recent.
	home, _ := cleanHookEnv(t)
	writeLog(t, home,
		`{"ts":`+recordTsMs(time.Now().Add(-5*time.Minute))+`,"from":"alice","to":"@bob","text":"x"}`,
	)
	if !recentActivity("alice", staleWindow, time.Now()) {
		t.Fatal("5-min-old traffic should be recent with default 30m window")
	}
	old := staleWindow
	t.Cleanup(func() { staleWindow = old })
	staleWindow = time.Minute
	if recentActivity("alice", staleWindow, time.Now()) {
		t.Errorf("5-min-old traffic should NOT be recent with 1m window")
	}
}
