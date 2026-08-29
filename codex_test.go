package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// --emit codex prints the SessionStart envelope Codex hooks consume (the same
// hookSpecificOutput shape as Claude Code — Codex rejects anything else),
// writes a join record, and — with no session id on stdin — does not attempt
// to spawn a bridge.
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

// parseCodexContext pulls additionalContext out of the codex hook output.
// Codex parses SessionStart output with unknown fields rejected, so the only
// accepted shape is {hookSpecificOutput:{hookEventName:"SessionStart",
// additionalContext}} — a top-level additionalContext (or any other stray
// key) makes Codex report "invalid session start JSON output" and drop the
// primer.
func parseCodexContext(t *testing.T, out string) string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &top); err != nil {
		t.Fatalf("hook output is not JSON: %v\n%s", err, out)
	}
	for k := range top {
		switch k {
		case "hookSpecificOutput", "continue", "stopReason", "suppressOutput", "systemMessage":
		default:
			t.Fatalf("codex rejects unknown top-level key %q in SessionStart output:\n%s", k, out)
		}
	}
	var inner map[string]json.RawMessage
	if err := json.Unmarshal(top["hookSpecificOutput"], &inner); err != nil {
		t.Fatalf("no hookSpecificOutput object in codex hook output: %v\n%s", err, out)
	}
	for k := range inner {
		if k != "hookEventName" && k != "additionalContext" {
			t.Fatalf("codex rejects unknown hookSpecificOutput key %q:\n%s", k, out)
		}
	}
	var ev, ctx string
	if err := json.Unmarshal(inner["hookEventName"], &ev); err != nil || ev != "SessionStart" {
		t.Fatalf("hookEventName = %s, want \"SessionStart\"", inner["hookEventName"])
	}
	if err := json.Unmarshal(inner["additionalContext"], &ctx); err != nil {
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

// The writer buffers partial lines, frames peer traffic, and — unlike before —
// passes internal "[agent-chat]" notices through verbatim. Dropping them is
// what made a dying bridge look identical to a quiet chat.
func TestBridgeWriterFramesPeerLinesAndPassesNotices(t *testing.T) {
	var got []string
	w := &bridgeWriter{
		deliver: func(msg string) error { got = append(got, msg); return nil },
		cancel:  func() {},
	}
	line := `{"ts":1.000,"from":"bob","to":"@alice","text":"hi"}`
	w.Write([]byte(line[:10]))
	w.Write([]byte(line[10:] + "\n[agent-chat] inbox bridge stopped\n"))
	if len(got) != 2 {
		t.Fatalf("deliveries = %d, want 2 (peer line + internal notice): %q", len(got), got)
	}
	if want := "New agent-chat message:\n@bob: hi"; got[0] != want {
		t.Errorf("delivered %q, want %q", got[0], want)
	}
	if want := "[agent-chat] inbox bridge stopped"; got[1] != want {
		t.Errorf("notice delivered %q, want %q (verbatim, no message framing)", got[1], want)
	}
}

// A failed delivery must be reported to the caller — that return value is what
// makes drainListen hold the cursor instead of dropping the message.
func TestBridgeWriterReportsFailureToCaller(t *testing.T) {
	w := &bridgeWriter{
		cancel:  func() {},
		deliver: func(string) error { return context.DeadlineExceeded },
	}
	line := []byte(`{"ts":1.000,"from":"bob","to":"@alice","text":"x"}` + "\n")
	if _, err := w.Write(line); err == nil {
		t.Fatal("Write reported success for a failed delivery; the cursor would advance past a lost message")
	}
}

// Retries are rate-limited: the listen loop polls five times a second and a
// stalled cursor re-offers the same line every poll.
func TestBridgeWriterThrottlesRetries(t *testing.T) {
	now := time.Unix(0, 0)
	calls := 0
	w := &bridgeWriter{
		cancel:  func() {},
		now:     func() time.Time { return now },
		deliver: func(string) error { calls++; return context.DeadlineExceeded },
	}
	line := []byte(`{"ts":1.000,"from":"bob","to":"@alice","text":"x"}` + "\n")

	w.Write(line)
	for i := 0; i < 5; i++ {
		now = now.Add(bridgeRetryInterval / 10)
		w.Write(line)
	}
	if calls != 1 {
		t.Fatalf("deliver called %d times inside the backoff window, want 1", calls)
	}
	now = now.Add(bridgeRetryInterval)
	w.Write(line)
	if calls != 2 {
		t.Fatalf("deliver called %d times, want 2 once the backoff elapsed", calls)
	}
}

// The bridge gives up only after delivery has been broken for the whole grace
// period — a window, not an attempt count, because a stalled cursor retries
// the same message on every poll. A success clears the window.
func TestBridgeWriterGivesUpAfterGracePeriod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Unix(0, 0)
	fail := true
	w := &bridgeWriter{
		cancel: cancel,
		now:    func() time.Time { return now },
		deliver: func(string) error {
			if fail {
				return context.DeadlineExceeded
			}
			return nil
		},
	}
	line := []byte(`{"ts":1.000,"from":"bob","to":"@alice","text":"x"}` + "\n")

	w.Write(line)
	now = now.Add(bridgeDeliverGrace - time.Second)
	w.Write(line)
	if ctx.Err() != nil {
		t.Fatal("cancelled before the grace period elapsed")
	}

	// A success resets the window.
	fail = false
	now = now.Add(bridgeRetryInterval)
	w.Write(line)
	fail = true
	now = now.Add(bridgeDeliverGrace)
	w.Write(line)
	if ctx.Err() != nil {
		t.Fatal("a success must reset the failure window")
	}

	// Let the fresh window run out.
	now = now.Add(bridgeDeliverGrace)
	w.Write(line)
	if ctx.Err() == nil {
		t.Fatal("not cancelled after delivery was broken for a full grace period")
	}
}

// The codex path must not inherit Claude's Monitor cap: a body Monitor would
// clip into a preview travels whole into `codex queue`.
func TestCodexPathCarriesBodiesMonitorWouldClip(t *testing.T) {
	r := Record{Ts: 1.0, From: "bob", To: "@alice", Text: strings.Repeat("x", 2000)}
	line := encode(t, r)

	if claude := notifyLine(line, r); !bytes.Contains(claude, []byte(`"clipped":true`)) {
		t.Fatalf("expected the Claude cap to clip a 2000-byte body, got %d bytes", len(claude))
	}
	if codex := codexListenOpts().format(line, r); string(codex) != string(line) {
		t.Errorf("codex path clipped a %d-byte body it can carry whole", len(line))
	}
}

// It still clips past its own cap, so a body can never exceed what one argv
// element can hold.
func TestCodexPathStillClipsPastItsOwnCap(t *testing.T) {
	r := Record{Ts: 1.0, From: "bob", To: "@alice", Text: strings.Repeat("x", codexNotifyMaxBytes+1)}
	out := codexListenOpts().format(encode(t, r), r)
	if !bytes.Contains(out, []byte(`"clipped":true`)) {
		t.Errorf("oversized body not clipped on the codex path (%d bytes)", len(out))
	}
	if len(out) > codexNotifyMaxBytes {
		t.Errorf("notice itself is %d bytes, over the %d cap", len(out), codexNotifyMaxBytes)
	}
}

// codex-bridge refuses to start without a thread id.
func TestCodexBridgeRequiresThread(t *testing.T) {
	withTempHome(t)
	if rc := run([]string{"codex-bridge", "--as", "alice", "--foreground"}); rc != 2 {
		t.Errorf("rc = %d, want 2 when --thread is missing", rc)
	}
}
