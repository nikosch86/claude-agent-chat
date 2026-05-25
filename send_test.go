package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// builtBinary is set by TestMain — used by the concurrency test that must
// spawn real OS processes to exercise O_APPEND across separate file
// descriptors.
var builtBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-chat-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "tempdir:", err)
		os.Exit(2)
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "agent-chat")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build failed:", err)
		os.Exit(2)
	}
	builtBinary = bin
	os.Exit(m.Run())
}

func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENT_CHAT_HOME", dir)
	return dir
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()
	var out []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		out = append(out, s.Text())
	}
	if err := s.Err(); err != nil {
		t.Fatalf("scan log: %v", err)
	}
	return out
}

func decodeOne(t *testing.T, line string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("decode %q: %v", line, err)
	}
	return m
}

func TestSendSingleRecipient(t *testing.T) {
	home := withTempHome(t)
	if rc := run([]string{"send", "--as", "alice", "@bob", "hi"}); rc != 0 {
		t.Fatalf("send exit code = %d, want 0", rc)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	m := decodeOne(t, lines[0])
	if m["from"] != "alice" || m["to"] != "@bob" || m["text"] != "hi" {
		t.Errorf("unexpected record: %v", m)
	}
	if _, ok := m["ts"].(float64); !ok {
		t.Errorf("ts not a number: %v", m["ts"])
	}
}

func TestSendMultiRecipientEmitsOneLinePerRecipient(t *testing.T) {
	home := withTempHome(t)
	if rc := run([]string{"send", "--as", "alice", "@bob", "@carol", "hi"}); rc != 0 {
		t.Fatalf("send exit code = %d, want 0", rc)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	gotTo := []string{decodeOne(t, lines[0])["to"].(string), decodeOne(t, lines[1])["to"].(string)}
	want := map[string]bool{"@bob": true, "@carol": true}
	for _, to := range gotTo {
		if !want[to] {
			t.Errorf("unexpected recipient %q", to)
		}
	}
}

func TestSendBroadcast(t *testing.T) {
	home := withTempHome(t)
	if rc := run([]string{"send", "--as", "alice", "*", "hi"}); rc != 0 {
		t.Fatalf("send exit code = %d, want 0", rc)
	}
	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if m := decodeOne(t, lines[0]); m["to"] != "*" {
		t.Errorf("to = %v, want *", m["to"])
	}
}

func TestSendCreatesParentDir(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "nested", "ac")
	t.Setenv("AGENT_CHAT_HOME", missing)
	if rc := run([]string{"send", "--as", "alice", "@bob", "hi"}); rc != 0 {
		t.Fatalf("send exit code = %d, want 0", rc)
	}
	if _, err := os.Stat(filepath.Join(missing, "log.jsonl")); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func TestTimestampMillisecondPrecision(t *testing.T) {
	home := withTempHome(t)
	before := float64(time.Now().UnixMilli()) / 1000.0
	if rc := run([]string{"send", "--as", "alice", "@bob", "hi"}); rc != 0 {
		t.Fatalf("send exit code = %d, want 0", rc)
	}
	after := float64(time.Now().UnixMilli()) / 1000.0

	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	// Inspect the raw string form to confirm ms (3-decimal) formatting.
	if !strings.Contains(lines[0], `"ts":`) {
		t.Fatalf("missing ts key: %s", lines[0])
	}
	// Pull out the ts substring up to the next comma.
	tsField := lines[0][strings.Index(lines[0], `"ts":`)+len(`"ts":`):]
	tsField = tsField[:strings.IndexByte(tsField, ',')]
	if dot := strings.IndexByte(tsField, '.'); dot < 0 || len(tsField)-dot-1 != 3 {
		t.Errorf("ts %q lacks 3-decimal ms formatting", tsField)
	}

	m := decodeOne(t, lines[0])
	got := m["ts"].(float64)
	if got < before-0.5 || got > after+0.5 {
		t.Errorf("ts %v outside [%v, %v]", got, before, after)
	}
}

func TestRejectsUnknownSubcommand(t *testing.T) {
	if rc := run([]string{"nope"}); rc != 2 {
		t.Errorf("unknown subcommand rc = %d, want 2", rc)
	}
}

func TestSendRejectsWhenNoNickResolvable(t *testing.T) {
	cleanResolverEnv(t)
	if rc := run([]string{"send", "@bob", "hi"}); rc == 0 {
		t.Errorf("send with no resolvable nick should fail")
	}
}

func TestSendRejectsBadRecipient(t *testing.T) {
	withTempHome(t)
	if rc := run([]string{"send", "--as", "alice", "bob-no-at", "hi"}); rc == 0 {
		t.Errorf("send with bare nick should fail")
	}
}

func TestSendRejectsEmptyText(t *testing.T) {
	home := withTempHome(t)
	if rc := run([]string{"send", "--as", "alice", "@bob", ""}); rc == 0 {
		t.Errorf("send with empty text should fail")
	}
	if _, err := os.Stat(filepath.Join(home, "log.jsonl")); !os.IsNotExist(err) {
		t.Errorf("log file should not exist after rejected send: err=%v", err)
	}
}

// TestConcurrentSendsAtomic spawns N real processes that each append one line,
// then verifies every line is intact JSON with the expected fields. This is
// the actual interleaving check the acceptance criterion calls for.
func TestConcurrentSendsAtomic(t *testing.T) {
	home := withTempHome(t)
	const n = 32

	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			cmd := exec.Command(builtBinary, "send", "--as", "alice", "@bob", fmt.Sprintf("msg-%03d", i))
			cmd.Env = append(os.Environ(), "AGENT_CHAT_HOME="+home)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- fmt.Errorf("proc %d: %v: %s", i, err, out)
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	lines := readLines(t, filepath.Join(home, "log.jsonl"))
	if len(lines) != n {
		t.Fatalf("got %d lines, want %d", len(lines), n)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		m := decodeOne(t, line)
		if m["from"] != "alice" || m["to"] != "@bob" {
			t.Errorf("garbled record (likely interleaved): %s", line)
			continue
		}
		text, ok := m["text"].(string)
		if !ok || !strings.HasPrefix(text, "msg-") {
			t.Errorf("garbled text field: %s", line)
			continue
		}
		seen[text] = true
	}
	if len(seen) != n {
		t.Errorf("distinct messages = %d, want %d", len(seen), n)
	}
}
