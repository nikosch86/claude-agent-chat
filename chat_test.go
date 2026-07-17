package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadRecords reads every record from the active log (empty if none written).
func loadRecords(t *testing.T) []Record {
	t.Helper()
	b, err := os.ReadFile(logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read log: %v", err)
	}
	var out []Record
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

func TestComposeRoutesRecipients(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		wantTo []string // one record expected per entry, in order
		body   string
	}{
		{"broadcast by default", "hello everyone", []string{"*"}, "hello everyone"},
		{"explicit broadcast", "* heads up", []string{"*"}, "heads up"},
		{"single direct", "@distro ping", []string{"@distro"}, "ping"},
		{"several direct", "@distro @clusterdl hi there", []string{"@distro", "@clusterdl"}, "hi there"},
		{"preserves inner spacing", "@distro   spaced   body", []string{"@distro"}, "spaced   body"},
		{"at-sign inside body stays", "@distro tell @clusterdl hi", []string{"@distro"}, "tell @clusterdl hi"},
		{"leading spaces before broadcast", "   just chatting", []string{"*"}, "just chatting"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempHome(t)
			if warn := compose("me", tc.input); warn != "" {
				t.Fatalf("unexpected warn: %q", warn)
			}
			got := loadRecords(t)
			if len(got) != len(tc.wantTo) {
				t.Fatalf("got %d records, want %d: %+v", len(got), len(tc.wantTo), got)
			}
			for i, r := range got {
				if r.From != "me" || r.To != tc.wantTo[i] || r.Text != tc.body {
					t.Errorf("record %d = {from:%q to:%q text:%q}, want {me %q %q}",
						i, r.From, r.To, r.Text, tc.wantTo[i], tc.body)
				}
			}
		})
	}
}

func TestComposeRejectsBodylessInput(t *testing.T) {
	withTempHome(t)
	warn := compose("me", "@distro @clusterdl")
	if warn == "" {
		t.Fatal("expected a warning for recipients-only input")
	}
	if recs := loadRecords(t); len(recs) != 0 {
		t.Fatalf("expected no records written, got %+v", recs)
	}
}

func TestChatNickInUse(t *testing.T) {
	line := func(r Record) string {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	msg := line(Record{From: "human", To: "*", Text: "hi"})
	joined := line(Record{From: "human", Event: "joined"})
	other := line(Record{From: "distro", To: "*", Text: "hi"})

	if !chatNickInUse("human", []string{other, msg}) {
		t.Error("want true when the nick authored a message")
	}
	if chatNickInUse("human", []string{other, joined}) {
		t.Error("want false when the nick only has lifecycle events")
	}
	if chatNickInUse("human", []string{other}) {
		t.Error("want false when the nick is absent")
	}
}

// editorState renders the buffer with a "|" marking the cursor position.
func editorState(e *lineEditor) string {
	return string(e.line[:e.cursor]) + "|" + string(e.line[e.cursor:])
}

func typeStr(e *lineEditor, s string) {
	for _, r := range s {
		e.insert(r)
	}
}

func TestLineEditorEditing(t *testing.T) {
	e := &lineEditor{}
	typeStr(e, "abc")
	steps := []struct {
		name string
		do   func()
		want string
	}{
		{"typed", func() {}, "abc|"},
		{"left x2", func() { e.left(); e.left() }, "a|bc"},
		{"insert mid", func() { e.insert('X') }, "aX|bc"},
		{"backspace mid", func() { e.backspace() }, "a|bc"},
		{"delete under cursor", func() { e.del() }, "a|c"},
		{"home", func() { e.home() }, "|ac"},
		{"end", func() { e.end() }, "ac|"},
	}
	for _, s := range steps {
		s.do()
		if got := editorState(e); got != s.want {
			t.Fatalf("%s: got %q want %q", s.name, got, s.want)
		}
	}
}

func TestLineEditorBounds(t *testing.T) {
	e := &lineEditor{}
	e.left()
	e.del()
	e.backspace() // all no-ops on an empty line
	if got := editorState(e); got != "|" {
		t.Fatalf("empty edits changed state: %q", got)
	}
	typeStr(e, "hi")
	e.right() // already at end
	if got := editorState(e); got != "hi|" {
		t.Fatalf("right past end moved cursor: %q", got)
	}
	e.home()
	e.left() // already at start
	if got := editorState(e); got != "|hi" {
		t.Fatalf("left past start moved cursor: %q", got)
	}
}

func TestLineEditorUTF8(t *testing.T) {
	e := &lineEditor{}
	typeStr(e, "café")
	e.left() // between 'f' and 'é' — one rune step, not one byte
	if got := editorState(e); got != "caf|é" {
		t.Fatalf("left over multibyte rune: %q", got)
	}
	e.backspace()
	if got := editorState(e); got != "ca|é" {
		t.Fatalf("backspace near multibyte: %q", got)
	}
}

func TestLineEditorHistory(t *testing.T) {
	e := &lineEditor{}
	typeStr(e, "first")
	e.submit()
	typeStr(e, "second")
	e.submit()
	if got := editorState(e); got != "|" {
		t.Fatalf("not fresh after submit: %q", got)
	}
	seq := []struct {
		name string
		do   func()
		want string
	}{
		{"up once", e.prev, "second|"},
		{"up twice", e.prev, "first|"},
		{"up at oldest stays", e.prev, "first|"},
		{"down once", e.next, "second|"},
		{"down to fresh", e.next, "|"},
		{"down at fresh stays", e.next, "|"},
	}
	for _, s := range seq {
		s.do()
		if got := editorState(e); got != s.want {
			t.Fatalf("%s: got %q want %q", s.name, got, s.want)
		}
	}
}

func TestLineEditorHistoryStash(t *testing.T) {
	e := &lineEditor{}
	typeStr(e, "old")
	e.submit()
	typeStr(e, "dr") // a fresh, unsent partial line
	e.prev()
	if got := editorState(e); got != "old|" {
		t.Fatalf("prev into history: %q", got)
	}
	e.next() // must restore the stashed partial line
	if got := editorState(e); got != "dr|" {
		t.Fatalf("stash not restored: %q", got)
	}
}

func TestLineEditorHistoryDedupAndBlank(t *testing.T) {
	e := &lineEditor{}
	typeStr(e, "x")
	e.submit()
	typeStr(e, "x")
	e.submit() // immediate duplicate — not re-added
	typeStr(e, "   ")
	e.submit() // blank — not added
	if len(e.history) != 1 {
		t.Fatalf("history = %d, want 1", len(e.history))
	}
	typeStr(e, "y")
	e.submit()
	if len(e.history) != 2 {
		t.Fatalf("history = %d, want 2", len(e.history))
	}
}

func TestLineEditorRecallDoesNotMutateHistory(t *testing.T) {
	e := &lineEditor{}
	typeStr(e, "hello")
	e.submit()
	e.prev()      // recall "hello"
	e.backspace() // edit the recalled copy...
	e.insert('P') // -> "hellP"
	if got := string(e.history[0]); got != "hello" {
		t.Fatalf("editing a recalled line mutated history: %q", got)
	}
}

// escChan builds a closed channel pre-loaded with the bytes that follow ESC, so
// readEscape resolves without hitting its timeout.
func escChan(bytes ...byte) chan byte {
	ch := make(chan byte, len(bytes)+1)
	for _, b := range bytes {
		ch <- b
	}
	close(ch)
	return ch
}

func TestReadEscape(t *testing.T) {
	cases := []struct {
		name  string
		bytes []byte
		want  escKey
	}{
		{"csi up", []byte("[A"), keyUp},
		{"csi down", []byte("[B"), keyDown},
		{"csi right", []byte("[C"), keyRight},
		{"csi left", []byte("[D"), keyLeft},
		{"csi home letter", []byte("[H"), keyHome},
		{"csi end letter", []byte("[F"), keyEnd},
		{"ss3 up", []byte("OA"), keyUp},
		{"delete", []byte("[3~"), keyDelete},
		{"home tilde", []byte("[1~"), keyHome},
		{"end tilde", []byte("[4~"), keyEnd},
		{"ctrl-left (modified) still left", []byte("[1;5D"), keyLeft},
		{"shift-right (modified) still right", []byte("[1;2C"), keyRight},
		{"bare esc", nil, keyNone},
		{"unknown final", []byte("[Z"), keyNone},
		{"unmapped tilde param", []byte("[9~"), keyNone},
		{"non-csi intro", []byte("x"), keyNone},
		{"ss3 truncated", []byte("O"), keyNone},
		{"csi params truncated", []byte("[1"), keyNone},
		{"csi separator truncated", []byte("[1;"), keyNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := readEscape(escChan(tc.bytes...)); got != tc.want {
				t.Fatalf("readEscape(%q) = %v, want %v", tc.bytes, got, tc.want)
			}
		})
	}
}

func TestReadEscapeTimeout(t *testing.T) {
	// A silent, still-open channel makes nextByte wait out its 50ms timeout and
	// resolve to keyNone rather than blocking on the missing sequence bytes.
	if got := readEscape(make(chan byte)); got != keyNone {
		t.Fatalf("readEscape on a silent channel = %v, want keyNone", got)
	}
}

func TestChatPrompt(t *testing.T) {
	if got, want := chatPrompt("neo", false), "neo> "; got != want {
		t.Errorf("chatPrompt(no color) = %q, want %q", got, want)
	}
	// With color the nick is wrapped in its ANSI color; the "> " suffix stays.
	got := chatPrompt("neo", true)
	if want := colorizeNick("neo", true) + "> "; got != want {
		t.Errorf("chatPrompt(color) = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "\x1b[") || !strings.HasSuffix(got, "> ") {
		t.Errorf("chatPrompt(color) = %q, want an ANSI-wrapped prompt ending in %q", got, "> ")
	}
}

func TestChatNote(t *testing.T) {
	const s = "heads up"
	if got, want := chatNote(s, false), s; got != want {
		t.Errorf("chatNote(no color) = %q, want %q", got, want)
	}
	if got, want := chatNote(s, true), ansiDimIt+s+ansiReset; got != want {
		t.Errorf("chatNote(color) = %q, want %q", got, want)
	}
}

func TestLineEditorRightAdvances(t *testing.T) {
	e := &lineEditor{}
	typeStr(e, "abc")
	e.home()
	e.right() // cursor < len: the advancing branch, not the end-of-line no-op
	if got := editorState(e); got != "a|bc" {
		t.Fatalf("right from start: got %q want %q", got, "a|bc")
	}
}

func TestComposeReportsSendFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// A home nested under a regular file makes appendRecord's MkdirAll fail.
	t.Setenv("AGENT_CHAT_HOME", filepath.Join(blocker, "home"))
	if warn := compose("me", "hello there"); !strings.HasPrefix(warn, "send failed:") {
		t.Fatalf("compose warn = %q, want a send-failure message", warn)
	}
}

func TestStdinIsTTY(t *testing.T) {
	// Validate stdinIsTTY against an independent char-device stat of stdin, so the
	// assertion holds whatever kind of stdin the test harness supplies.
	fi, err := os.Stdin.Stat()
	if err != nil {
		t.Skipf("cannot stat stdin: %v", err)
	}
	want := fi.Mode()&os.ModeCharDevice != 0
	if got := stdinIsTTY(); got != want {
		t.Fatalf("stdinIsTTY() = %v, want %v", got, want)
	}
}

func TestEnterRawWithoutTTY(t *testing.T) {
	// enterRaw shells out to stty against stdin; a non-tty stdin makes stty fail.
	// Guard on whether stty itself succeeds, so a real terminal skips cleanly.
	if _, err := sttyCapture("-g"); err == nil {
		t.Skip("stdin is a working terminal here; stty succeeds")
	}
	restore, err := enterRaw()
	if err == nil {
		if restore != nil {
			restore()
		}
		t.Error("enterRaw on a non-tty stdin: want error, got nil")
	}
}

// runChat entry paths that terminate before the interactive loop.

func TestChatRejectsWhenNoNickResolvable(t *testing.T) {
	cleanResolverEnv(t)
	stderr, rc := captureStderr(t, func() int { return run([]string{"chat"}) })
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr, "chat: could not resolve nick") {
		t.Errorf("stderr = %q, want resolver error", stderr)
	}
}

func TestChatRejectsNonTTYStdin(t *testing.T) {
	cleanResolverEnv(t)
	withStdin(t, "") // a pipe is not a character device
	stderr, rc := captureStderr(t, func() int { return run([]string{"chat", "--as", "alice"}) })
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
	if !strings.Contains(stderr, "stdin is not a terminal") {
		t.Errorf("stderr = %q, want non-terminal message", stderr)
	}
}

func TestChatFailsWhenRawModeUnavailable(t *testing.T) {
	cleanResolverEnv(t)
	// /dev/null is a character device, so the TTY check passes, but stty
	// cannot configure it and enterRaw fails before the loop starts.
	withDevNullStdin(t)
	if _, err := sttyCapture("-g"); err == nil {
		t.Skip("stty succeeds against this stdin; cannot exercise the raw-mode failure")
	}
	stderr, rc := captureStderr(t, func() int { return run([]string{"chat", "--as", "alice"}) })
	if rc != 1 {
		t.Errorf("rc = %d, want 1", rc)
	}
	if !strings.Contains(stderr, "chat: cannot read terminal state") {
		t.Errorf("stderr = %q, want raw-mode failure", stderr)
	}
}
