package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeLog(t *testing.T, home string, lines ...string) {
	t.Helper()
	p := filepath.Join(home, "log.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// captureStdout swaps os.Stdout for a pipe for the duration of fn.
func captureStdout(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	rc := fn()
	w.Close()
	os.Stdout = orig
	return <-done, rc
}

const (
	lineAliceBob   = `{"ts":1000.000,"from":"alice","to":"@bob","text":"hi bob"}`
	lineBobAlice   = `{"ts":1001.000,"from":"bob","to":"@alice","text":"hi alice"}`
	lineAliceCarol = `{"ts":1002.000,"from":"alice","to":"@carol","text":"hi carol"}`
	lineBroadcast  = `{"ts":1003.000,"from":"carol","to":"*","text":"hello room"}`
	lineJoined     = `{"ts":1004.000,"from":"dave","event":"joined"}`
)

var allLines = []string{lineAliceBob, lineBobAlice, lineAliceCarol, lineBroadcast, lineJoined}

func TestHistoryPrintsAllAsJSON(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)
	out, rc := captureStdout(t, func() int { return run([]string{"history"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(allLines) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(allLines), out)
	}
	for i, want := range allLines {
		if lines[i] != want {
			t.Errorf("line %d:\n  got:  %s\n  want: %s", i, lines[i], want)
		}
	}
}

func TestHistoryFilterFrom(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)
	out, rc := captureStdout(t, func() int { return run([]string{"history", "--from", "@alice"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if !strings.Contains(line, `"from":"alice"`) {
			t.Errorf("non-alice line leaked: %s", line)
		}
	}
	if !strings.Contains(out, "hi bob") || !strings.Contains(out, "hi carol") {
		t.Errorf("missing alice lines:\n%s", out)
	}
	if strings.Contains(out, "hi alice") {
		t.Errorf("bob->alice line leaked:\n%s", out)
	}
}

func TestHistoryFilterFromAcceptsBareNick(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)
	out, rc := captureStdout(t, func() int { return run([]string{"history", "--from", "alice"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if strings.Contains(out, "hi alice") {
		t.Errorf("bare-nick form should still filter:\n%s", out)
	}
}

func TestHistoryFilterTo(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)
	out, rc := captureStdout(t, func() int { return run([]string{"history", "--to", "@bob"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "@bob") {
		t.Errorf("want exactly one @bob line, got:\n%s", out)
	}
}

func TestHistoryToMeIncludesBroadcast(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)
	out, rc := captureStdout(t, func() int {
		return run([]string{"history", "--to", "me", "--as", "alice"})
	})
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if !strings.Contains(out, "hi alice") {
		t.Errorf("missing direct @alice line:\n%s", out)
	}
	if !strings.Contains(out, "hello room") {
		t.Errorf("missing broadcast line:\n%s", out)
	}
	if strings.Contains(out, "hi bob") || strings.Contains(out, "hi carol") {
		t.Errorf("non-recipient line leaked:\n%s", out)
	}
}

func TestHistoryToMeFailsWhenNoNickResolvable(t *testing.T) {
	cleanResolverEnv(t)
	if rc := run([]string{"history", "--to", "me"}); rc == 0 {
		t.Errorf("--to me with no resolvable nick should fail")
	}
}

func TestHistorySinceDuration(t *testing.T) {
	home := withTempHome(t)
	now := time.Now()
	old := now.Add(-2 * time.Hour).UnixMilli()
	recent := now.Add(-10 * time.Minute).UnixMilli()
	writeLog(t, home,
		`{"ts":`+fmtMs(old)+`,"from":"alice","to":"@bob","text":"old"}`,
		`{"ts":`+fmtMs(recent)+`,"from":"alice","to":"@bob","text":"recent"}`,
	)
	out, rc := captureStdout(t, func() int { return run([]string{"history", "--since", "1h"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if strings.Contains(out, "old") {
		t.Errorf("old line leaked through --since 1h:\n%s", out)
	}
	if !strings.Contains(out, "recent") {
		t.Errorf("recent line missing:\n%s", out)
	}
}

func TestHistorySinceAbsoluteDate(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home,
		`{"ts":1000.000,"from":"alice","to":"@bob","text":"way old"}`,
		`{"ts":4102444800.000,"from":"alice","to":"@bob","text":"future"}`, // 2100-01-01
	)
	out, rc := captureStdout(t, func() int {
		return run([]string{"history", "--since", "2099-01-01"})
	})
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if strings.Contains(out, "way old") {
		t.Errorf("pre-cutoff line leaked:\n%s", out)
	}
	if !strings.Contains(out, "future") {
		t.Errorf("post-cutoff line missing:\n%s", out)
	}
}

func TestHistoryTail(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)
	out, rc := captureStdout(t, func() int { return run([]string{"history", "--tail", "2"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	if lines[0] != lineBroadcast || lines[1] != lineJoined {
		t.Errorf("tail returned wrong lines:\n%s", out)
	}
}

func TestHistoryFiltersComposeAnd(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)
	out, rc := captureStdout(t, func() int {
		return run([]string{"history", "--from", "alice", "--to", "@bob"})
	})
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "hi bob") {
		t.Errorf("AND filter wrong, got:\n%s", out)
	}
}

func TestHistoryFormatText(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineAliceBob, lineJoined)
	out, rc := captureStdout(t, func() int { return run([]string{"history", "--format", "text"}) })
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if strings.Contains(out, "{") {
		t.Errorf("text format leaked JSON braces:\n%s", out)
	}
	if !strings.Contains(out, "alice") || !strings.Contains(out, "hi bob") {
		t.Errorf("text output missing fields:\n%s", out)
	}
	if !strings.Contains(out, "joined") {
		t.Errorf("text output missing event:\n%s", out)
	}
}

func TestHistoryEmptyLogIsEmptyOutput(t *testing.T) {
	withTempHome(t)
	out, rc := captureStdout(t, func() int { return run([]string{"history"}) })
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 for missing log", rc)
	}
	if out != "" {
		t.Errorf("want empty output, got %q", out)
	}
}

func TestHistoryRejectsBadFormat(t *testing.T) {
	withTempHome(t)
	if rc := run([]string{"history", "--format", "yaml"}); rc == 0 {
		t.Errorf("bad --format should fail")
	}
}

func fmtMs(ms int64) string {
	whole := ms / 1000
	frac := ms % 1000
	return formatInt(whole) + "." + zeroPad3(frac)
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func zeroPad3(n int64) string {
	if n < 0 {
		n = -n
	}
	s := formatInt(n)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}
