package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// --emit codex prints the flat {additionalContext} object Codex hooks consume
// (no hookSpecificOutput envelope), writes a join record, and — with no
// session id on stdin — does not attempt to spawn a bridge.
func TestHookStartEmitCodexJoins(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "codex"}) })
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	primer := parseCodexContext(t, out)
	if !strings.Contains(primer, "joined as `alice`") {
		t.Errorf("primer missing join line:\n%s", primer)
	}
	if !strings.Contains(primer, "delivered into this session automatically") {
		t.Errorf("codex primer must explain automatic delivery:\n%s", primer)
	}
	for _, banned := range []string{"REQUIRED FIRST ACTION", "Monitor(", "ToolSearch"} {
		if strings.Contains(primer, banned) {
			t.Errorf("codex primer must not contain %q (no Monitor tool exists there):\n%s", banned, primer)
		}
	}
	if !joinRecorded(t, home, "alice") {
		t.Errorf("hook-start --emit codex should write a join record for alice")
	}
}

// A nick held by another active session keeps the Claude-mode contract in
// codex mode: rc 0 with a NOT-JOINED context, and no join record.
func TestHookStartEmitCodexCollision(t *testing.T) {
	home, _ := cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	claimNickFromForeignCwd(t, home, "alice")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "codex"}) })
	if rc != 0 {
		t.Fatalf("collision rc = %d, want 0", rc)
	}
	if primer := parseCodexContext(t, out); !strings.Contains(primer, "NOT JOINED") {
		t.Errorf("expected NOT JOINED context; got:\n%s", primer)
	}
	if joinRecorded(t, home, "alice") {
		t.Errorf("a colliding session must not write a join record")
	}
}

func TestHookStartEmitCodexOptOut(t *testing.T) {
	cleanHookEnv(t)
	t.Setenv("CLAUDE_AGENT_CHAT_NICK", "alice")
	t.Setenv("CLAUDE_AGENT_CHAT", "0")

	out, rc := captureStdout(t, func() int { return run([]string{"hook-start", "--emit", "codex"}) })
	if rc != 0 || strings.TrimSpace(out) != "" {
		t.Fatalf("opt-out: rc=%d out=%q, want rc=0 and empty", rc, out)
	}
}

// parseCodexContext pulls additionalContext out of the flat codex hook object,
// failing if the output carries Claude's hookSpecificOutput envelope instead.
func parseCodexContext(t *testing.T, out string) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	if _, ok := m["hookSpecificOutput"]; ok {
		t.Fatalf("codex mode must not emit the Claude envelope:\n%s", out)
	}
	var ctx string
	if err := json.Unmarshal(m["additionalContext"], &ctx); err != nil {
		t.Fatalf("no additionalContext string in codex hook output: %v\n%s", err, out)
	}
	return ctx
}

func TestRenderBridgeLine(t *testing.T) {
	cases := []struct {
		name, line, want string
	}{
		{
			"plain text",
			`{"ts":1000.000,"from":"bob","to":"@alice","text":"hi there"}`,
			"@bob: hi there",
		},
		{
			"empty text still renders as a message",
			`{"ts":1000.000,"from":"bob","to":"@alice","text":""}`,
			"@bob: ",
		},
		{
			"file share with note",
			`{"ts":1000.000,"from":"bob","to":"@alice","path":"/x/artifacts/bob/f.txt","note":"logs"}`,
			"@bob shared a file: /x/artifacts/bob/f.txt — logs",
		},
		{
			"clipped notice",
			`{"ts":1000.000,"from":"bob","to":"@alice","clipped":true,"bytes":9999,"preview":"start of it","full":"agent-chat history --id 1000.000 --format text"}`,
			"@bob sent a 9999-byte message, too long to show here. It starts: \"start of it\" — run `agent-chat history --id 1000.000 --format text` to read all of it.",
		},
		{
			"non-JSON passes through raw",
			"not json at all",
			"not json at all",
		},
	}
	for _, c := range cases {
		if got := renderBridgeLine(c.line); got != c.want {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

// The writer buffers partial lines, skips internal "[agent-chat]" notices, and
// prefixes each delivery.
func TestBridgeWriterSplitsAndFilters(t *testing.T) {
	var got []string
	w := &bridgeWriter{
		deliver: func(msg string) error { got = append(got, msg); return nil },
		cancel:  func() {},
	}
	line := `{"ts":1.000,"from":"bob","to":"@alice","text":"hi"}`
	w.Write([]byte(line[:10]))
	w.Write([]byte(line[10:] + "\n[agent-chat] inbox listener stopped\n"))
	if len(got) != 1 {
		t.Fatalf("deliveries = %d, want 1 (internal notice must be filtered): %q", len(got), got)
	}
	if want := "New agent-chat message:\n@bob: hi"; got[0] != want {
		t.Errorf("delivered %q, want %q", got[0], want)
	}
}

// After bridgeMaxDeliverFails consecutive failures the writer cancels the
// bridge context (the session is gone); a success in between resets the count.
func TestBridgeWriterGivesUpAfterConsecutiveFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fail := true
	calls := 0
	w := &bridgeWriter{
		cancel: cancel,
		deliver: func(string) error {
			calls++
			if fail {
				return context.DeadlineExceeded
			}
			return nil
		},
	}
	line := []byte(`{"ts":1.000,"from":"bob","to":"@alice","text":"x"}` + "\n")

	// One failure short of the limit, then a success: counter must reset.
	for i := 0; i < bridgeMaxDeliverFails-1; i++ {
		w.Write(line)
	}
	fail = false
	w.Write(line)
	fail = true
	for i := 0; i < bridgeMaxDeliverFails-1; i++ {
		w.Write(line)
	}
	if ctx.Err() != nil {
		t.Fatal("cancelled too early: a success must reset the failure count")
	}
	w.Write(line)
	if ctx.Err() == nil {
		t.Fatalf("context not cancelled after %d consecutive failures (%d deliveries)", bridgeMaxDeliverFails, calls)
	}
}

// codex-bridge refuses to start without a thread id.
func TestCodexBridgeRequiresThread(t *testing.T) {
	withTempHome(t)
	if rc := run([]string{"codex-bridge", "--as", "alice", "--foreground"}); rc != 2 {
		t.Errorf("rc = %d, want 2 when --thread is missing", rc)
	}
}
