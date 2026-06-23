package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// When the body reaches agent-chat as a single argv, the binary stores the
// backticks and $(...) byte-for-byte.
func TestSendStoresShellMetacharsVerbatim(t *testing.T) {
	home := withTempHome(t)
	body := "deploy `whoami` then $(id) done"
	if rc := run([]string{"send", "--as", "alice", "@bob", body}); rc != 0 {
		t.Fatalf("send rc = %d, want 0", rc)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if got := decodeOne(t, lines[0])["text"]; got != body {
		t.Errorf("text = %q, want verbatim %q", got, body)
	}
}

// A single-quoted body passed through a real shell reaches the log with its
// shell metacharacters intact, unexpanded.
func TestSendSingleQuotedBodyViaShellVerbatim(t *testing.T) {
	home := withTempHome(t)
	want := "deploy `whoami` then $(id) done"
	script := "'" + builtBinary + "' send --as alice @bob 'deploy `whoami` then $(id) done'"
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), "AGENT_CHAT_HOME="+home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("send failed: %v: %s", err, out)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if got := decodeOne(t, lines[0])["text"]; got != want {
		t.Errorf("text = %q, want verbatim %q", got, want)
	}
}

// A body carrying ANSI/control sequences renders with the control bytes
// escaped to \xNN, never raw.
func TestRenderTextEscapesControlBytes(t *testing.T) {
	r := Record{Ts: 1, From: "mallory", To: "@bob", Text: "safe\x1b[31mRED\x1b[0m\x07bell"}
	got := renderText(r)
	if strings.ContainsAny(got, "\x1b\x07") {
		t.Errorf("renderText leaked control bytes: %q", got)
	}
	if !strings.Contains(got, `\x1b`) || !strings.Contains(got, `\x07`) {
		t.Errorf("expected escaped control bytes in %q", got)
	}
	if !strings.Contains(got, "RED") {
		t.Errorf("printable content should survive: %q", got)
	}
}

// Color is off, so any ESC in the output could only come from the body.
func TestRenderWatchEscapesControlBytes(t *testing.T) {
	r := Record{Ts: 1, From: "mallory", To: "@bob", Text: "x\x1b[2Jy"}
	got := renderWatch(r, false, false)
	if strings.ContainsRune(got, 0x1b) {
		t.Errorf("renderWatch leaked ESC: %q", got)
	}
}

// Multi-byte printable runes pass through unescaped.
func TestSanitizeDisplayPreservesUTF8(t *testing.T) {
	in := "héllo — 世界 🚀"
	if got := sanitizeDisplay(in); got != in {
		t.Errorf("sanitizeDisplay mangled printable UTF-8: %q", got)
	}
}

// from, to, and event are not sanitised at write time, so the renderers must
// escape control bytes in those fields too, not just the body.
func TestRenderersEscapeControlBytesInNicks(t *testing.T) {
	r := Record{Ts: 1, From: "ev\x1bil", To: "@b\x1bob", Text: "hi"}
	if got := renderText(r); strings.ContainsRune(got, 0x1b) {
		t.Errorf("renderText leaked ESC in from/to: %q", got)
	}
	if got := renderWatch(r, false, false); strings.ContainsRune(got, 0x1b) {
		t.Errorf("renderWatch leaked ESC in from/to: %q", got)
	}
	ev := Record{Ts: 1, From: "x", Event: "joined\x1b[2J"}
	if got := renderText(ev); strings.ContainsRune(got, 0x1b) {
		t.Errorf("renderText leaked ESC in event: %q", got)
	}
}

// peers prints joined nicks straight to the terminal, so a control byte in
// `from` must be escaped there too. appendRecord forges such a record.
func TestPeersEscapesControlBytesInNick(t *testing.T) {
	home := withTempHome(t)
	esc := string(rune(0x1b))
	if err := appendRecord(Record{Ts: 1, From: "ev" + esc + "il", Event: "joined"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(builtBinary, "peers")
	cmd.Env = append(os.Environ(), "AGENT_CHAT_HOME="+home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("peers failed: %v: %s", err, out)
	}
	if strings.ContainsRune(string(out), 0x1b) {
		t.Errorf("peers leaked ESC to terminal: %q", out)
	}
	if !strings.Contains(string(out), `ev\x1bil`) {
		t.Errorf("peers should show the escaped nick, got: %q", out)
	}
}
