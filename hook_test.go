package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cleanHookEnv wipes everything hook-start consults and gives the test a
// fresh AGENT_CHAT_HOME and a non-git cwd. Tests opt back in to whichever
// signal they care about (env, git repo, .agent-chat-nick).
func cleanHookEnv(t *testing.T) (home, cwd string) {
	t.Helper()
	home = withTempHome(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AGENT_CHAT_NICK", "")
	t.Setenv("USER", "")
	t.Setenv("CLAUDE_AGENT_CHAT", "")
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "")
	cwd = t.TempDir()
	chdirTo(t, cwd)
	withDevNullStdin(t)
	return home, cwd
}

// parsePrimer pulls the additionalContext string out of the hook envelope.
func parsePrimer(t *testing.T, out string) string {
	t.Helper()
	var env struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	if env.HookSpecificOutput.HookEventName != "SessionStart" {
		t.Errorf("hookEventName = %q, want SessionStart", env.HookSpecificOutput.HookEventName)
	}
	return env.HookSpecificOutput.AdditionalContext
}

// lineContaining returns the single primer line containing sub, failing if
// there isn't exactly one. Lets a test assert on one line without matching
// substrings elsewhere in the primer.
func lineContaining(t *testing.T, text, sub string) string {
	t.Helper()
	var hits []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.Contains(ln, sub) {
			hits = append(hits, ln)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly one line containing %q, got %d:\n%s", sub, len(hits), text)
	}
	return hits[0]
}

// initGitRepo makes `dir` a git repo and returns the resolved (symlink-evaled)
// path that resolveNick / hook-start will see.
func initGitRepo(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestHookStartEmitsPrimerInGitRepo(t *testing.T) {
	home, cwd := cleanHookEnv(t)
	root := initGitRepo(t, cwd)
	chdirTo(t, root)

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}
	primer := parsePrimer(t, out)
	nick := filepath.Base(root)
	if !strings.Contains(primer, "joined as `"+nick+"`") {
		t.Errorf("primer missing nick: %s", primer)
	}

	// log got a joined event
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != 1 || !strings.Contains(lines[0], `"event":"joined"`) || !strings.Contains(lines[0], `"from":"`+nick+`"`) {
		t.Errorf("expected one joined line for %s, got: %v", nick, lines)
	}
}

func TestHookStartWritesByCwdMapping(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	if rc := run([]string{"hook-start"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	got, ok := readByCwd()
	if !ok || got != "alice" {
		t.Errorf("by-cwd read = %q ok=%v, want alice", got, ok)
	}
	// Cursor file is initialized
	if _, err := os.Stat(filepath.Join(home, "agents", "alice", "cursor")); err != nil {
		t.Errorf("cursor not initialized: %v", err)
	}
}

func TestHookStartSkipsWhenEnvDisabled(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT", "0")
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if out != "" {
		t.Errorf("expected no stdout when disabled, got: %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, "log.jsonl")); !os.IsNotExist(err) {
		t.Errorf("log file should not exist when disabled: %v", err)
	}
	if _, ok := readByCwd(); ok {
		t.Errorf("by-cwd should not be written when disabled")
	}
}

func TestHookStartSkipsWhenRepoOptOut(t *testing.T) {
	home, cwd := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	if err := os.WriteFile(filepath.Join(cwd, ".no-agent-chat"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if out != "" {
		t.Errorf("expected no stdout when opted out, got: %q", out)
	}
	if _, err := os.Stat(filepath.Join(home, "log.jsonl")); !os.IsNotExist(err) {
		t.Errorf("log file should not exist on repo opt-out: %v", err)
	}
}

func TestHookStartNickPrecedence(t *testing.T) {
	// env > git basename > .agent-chat-nick
	_, cwd := cleanHookEnv(t)
	root := initGitRepo(t, cwd)
	chdirTo(t, root)
	if err := os.WriteFile(filepath.Join(root, ".agent-chat-nick"), []byte("from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "from-env")

	if rc := run([]string{"hook-start"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if got, _ := readByCwd(); got != "from-env" {
		t.Errorf("env should win, got %q", got)
	}

	// Now drop env: git basename should win over .agent-chat-nick.
	cleanHookEnv(t)
	root2 := initGitRepo(t, t.TempDir())
	chdirTo(t, root2)
	if err := os.WriteFile(filepath.Join(root2, ".agent-chat-nick"), []byte("from-file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"hook-start"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	want := filepath.Base(root2)
	if got, _ := readByCwd(); got != want {
		t.Errorf("git basename should win, got %q want %q", got, want)
	}

	// Now drop git (use a non-git cwd) — .agent-chat-nick should win.
	cleanHookEnv(t)
	cwd3, _ := os.Getwd()
	if err := os.WriteFile(filepath.Join(cwd3, ".agent-chat-nick"), []byte("\n\nfrom-file\nignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := run([]string{"hook-start"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if got, _ := readByCwd(); got != "from-file" {
		t.Errorf(".agent-chat-nick should win, got %q", got)
	}
}

func TestHookStartSkipsWhenNoNickAnywhere(t *testing.T) {
	home, _ := cleanHookEnv(t)
	if rc := run([]string{"hook-start"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if _, err := os.Stat(filepath.Join(home, "log.jsonl")); !os.IsNotExist(err) {
		t.Errorf("log file should not exist with no derivable nick: %v", err)
	}
	if _, ok := readByCwd(); ok {
		t.Errorf("by-cwd should not be written with no derivable nick")
	}
}

func TestSanitizeNick(t *testing.T) {
	cases := []struct{ in, want string }{
		{"foo-service", "foo-service"},
		{"Foo_Service-2", "Foo_Service-2"},
		{"-leading-dash-", "leading-dash"},
		{"---", ""},
		{"with spaces and !@#", "withspacesand"},
		{"unicode-✨-x", "unicode--x"},
		{strings.Repeat("a", 30), strings.Repeat("a", 24)},
		// length cap can leave a trailing dash that should be trimmed.
		{strings.Repeat("a", 24) + "-tail", strings.Repeat("a", 24)},
	}
	for _, c := range cases {
		if got := sanitizeNick(c.in); got != c.want {
			t.Errorf("sanitizeNick(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHookStartInlinesMissedMessages(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	// Seed the log with traffic the cursor will be set to skip past initially.
	writeLog(t, home, lineBobAlice, lineAliceBob, lineBroadcast)
	// Pre-create a cursor at offset 0 so the join replays everything matching.
	if err := writeCursor("alice", 0); err != nil {
		t.Fatal(err)
	}

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	primer := parsePrimer(t, out)

	if !strings.Contains(primer, "missed 2 mention") {
		t.Errorf("expected missed-count of 2 (bob->alice + broadcast), primer:\n%s", primer)
	}
	if !strings.Contains(primer, "hi alice") {
		t.Errorf("primer missing @alice line:\n%s", primer)
	}
	if !strings.Contains(primer, "hello room") {
		t.Errorf("primer missing broadcast line:\n%s", primer)
	}
	if strings.Contains(primer, "hi bob") {
		t.Errorf("primer leaked non-recipient line:\n%s", primer)
	}

	// Cursor advanced past the joined line.
	endOff, _ := readCursor("alice")
	fi, _ := os.Stat(filepath.Join(home, "log.jsonl"))
	if endOff != fi.Size() {
		t.Errorf("cursor = %d, want %d (log EOF)", endOff, fi.Size())
	}
}

func TestHookStartTruncatesManyMissed(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	// Five mentions to @alice, all behind a cursor at 0, so the join replays
	// them — more than missedPreviewMax, which must trigger truncation.
	writeLog(t, home,
		`{"ts":1001.000,"from":"bob","to":"@alice","text":"m1"}`,
		`{"ts":1002.000,"from":"bob","to":"@alice","text":"m2"}`,
		`{"ts":1003.000,"from":"bob","to":"@alice","text":"m3"}`,
		`{"ts":1004.000,"from":"bob","to":"@alice","text":"m4"}`,
		`{"ts":1005.000,"from":"bob","to":"@alice","text":"m5"}`,
	)
	if err := writeCursor("alice", 0); err != nil {
		t.Fatal(err)
	}

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	primer := parsePrimer(t, out)

	// Full count is still reported, and the agent is told how to get the rest.
	if !strings.Contains(primer, "missed 5 mention") {
		t.Errorf("expected full missed count of 5, primer:\n%s", primer)
	}
	// The recovery hint must anchor on time (--since), not --tail N: tail would
	// drop the oldest missed mentions if new traffic arrives before the agent
	// runs it. See missedSinceAnchor. Scope the check to the hint line — the
	// static guidance elsewhere in the primer legitimately mentions --tail.
	hint := lineContaining(t, primer, "for the rest")
	if !strings.Contains(hint, "--since ") {
		t.Errorf("recovery hint should anchor on --since, got: %q", hint)
	}
	if strings.Contains(hint, "--tail") {
		t.Errorf("recovery hint should not use the lossy --tail form, got: %q", hint)
	}
	// Only the latest missedPreviewMax (3) are inlined.
	for _, want := range []string{`"m3"`, `"m4"`, `"m5"`} {
		if !strings.Contains(primer, want) {
			t.Errorf("expected latest mention %s inlined, primer:\n%s", want, primer)
		}
	}
	for _, dropped := range []string{`"m1"`, `"m2"`} {
		if strings.Contains(primer, dropped) {
			t.Errorf("old mention %s should not be inlined, primer:\n%s", dropped, primer)
		}
	}
}

func TestHookStartAlwaysInstructsMonitor(t *testing.T) {
	// Even with a fresh heartbeat (a listener appears to be running), the primer
	// must still instruct the agent to start the Monitor. A fresh listener
	// cleanly takes over any existing one — which may be orphaned from a dead
	// session and silently eating this nick's messages — so starting is always
	// safe and suppressing it is what made sessions go deaf.
	_, _ = cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	touchListenerHeartbeat("alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}
	primer := parsePrimer(t, out)
	if !strings.Contains(primer, "REQUIRED FIRST ACTION") {
		t.Errorf("primer must always instruct Monitor start, even with a fresh heartbeat:\n%s", primer)
	}
}

func TestHookStartFreshHasNoMissed(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	writeLog(t, home, lineBobAlice, lineBroadcast)

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	primer := parsePrimer(t, out)
	if strings.Contains(primer, "missed") {
		t.Errorf("fresh join (no prior cursor) should have no missed-section; primer:\n%s", primer)
	}
}

func TestHookStopAppendsQuitAndCleansUp(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	if rc := run([]string{"hook-start"}); rc != 0 {
		t.Fatalf("hook-start rc = %d", rc)
	}

	// Have somebody else write to the log so the cursor advance is visible.
	writeLog(t, home, lineAliceBob)

	if rc := run([]string{"hook-stop"}); rc != 0 {
		t.Fatalf("hook-stop rc = %d", rc)
	}

	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	last := lines[len(lines)-1]
	if !strings.Contains(last, `"event":"quit"`) || !strings.Contains(last, `"from":"alice"`) {
		t.Errorf("last line is not alice's quit: %s", last)
	}

	if _, ok := readByCwd(); ok {
		t.Errorf("by-cwd mapping should be removed after hook-stop")
	}

	off, ok := readCursor("alice")
	if !ok {
		t.Fatalf("cursor missing after hook-stop")
	}
	fi, _ := os.Stat(filepath.Join(home, "log.jsonl"))
	if off != fi.Size() {
		t.Errorf("cursor = %d, want %d (log EOF after quit)", off, fi.Size())
	}
}

func TestHookStopNoOpWhenNoByCwd(t *testing.T) {
	home, _ := cleanHookEnv(t)
	if rc := run([]string{"hook-stop"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if _, err := os.Stat(filepath.Join(home, "log.jsonl")); !os.IsNotExist(err) {
		t.Errorf("hook-stop on unclaimed cwd should not touch log: %v", err)
	}
}

// TestWriteCursorAtomicUnderConcurrency hammers writeCursor from many
// goroutines and confirms readCursor never observes a torn write. The check
// that matters is "ok == true" for every read — a partial write would parse
// as empty and return (0, false), silently rewinding the listener.
func TestWriteCursorAtomicUnderConcurrency(t *testing.T) {
	withTempHome(t)
	const writers = 16
	const iters = 200

	// Seed so the reader's first read can't legitimately race with the
	// initial file creation — we're testing torn updates, not first-write.
	if err := writeCursor("alice", -1); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				if err := writeCursor("alice", int64(i*iters+j)); err != nil {
					t.Errorf("writeCursor: %v", err)
					return
				}
			}
		}()
	}
	// Reader goroutine that asserts the cursor is always readable.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if _, ok := readCursor("alice"); !ok {
				t.Errorf("readCursor saw a torn write")
				return
			}
		}
	}()
	wg.Wait()
	close(done)
}

func TestHookStartPrunesOldArtifacts(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	artDir := filepath.Join(home, "artifacts", "someone")
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldFile := filepath.Join(artDir, "old.txt")
	newFile := filepath.Join(artDir, "new.txt")
	for _, p := range []string{oldFile, newFile} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(oldFile, old, old); err != nil {
		t.Fatal(err)
	}

	if rc := run([]string{"hook-start"}); rc != 0 {
		t.Fatalf("rc = %d", rc)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old artifact should have been pruned: err=%v", err)
	}
	if _, err := os.Stat(newFile); err != nil {
		t.Errorf("fresh artifact should still exist: %v", err)
	}
}
