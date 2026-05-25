package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// withFastWatch shrinks watchPollInterval so follow-loop tests don't sit
// on the production 200ms cadence. Restored on cleanup.
func withFastWatch(t *testing.T) {
	t.Helper()
	prev := watchPollInterval
	watchPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { watchPollInterval = prev })
}

// runWatchInBackground starts watchLoop in a goroutine and returns the
// cancel func + completion channel. The watchLoop variant is used (not
// watchSession) so tests that want clean rendering output don't have the
// lifecycle `joined`/`quit` lines bleed into the log.
func runWatchInBackground(t *testing.T, out *syncBuf, filter string, tail int, color, withDate bool) context.CancelFunc {
	t.Helper()
	withFastWatch(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- watchLoop(ctx, out, filter, tail, color, withDate) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("watch loop did not exit within 2s")
		}
	})
	return cancel
}

func TestWatchBacklogRendersLastN(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)

	var buf syncBuf
	runWatchInBackground(t, &buf, "", 3, false, false)

	if !waitFor(t, time.Second, func() bool {
		return strings.Count(buf.String(), "\n") >= 3
	}) {
		t.Fatalf("backlog not rendered: %q", buf.String())
	}
	got := buf.String()
	// Tail=3 should drop the two oldest matched lines (lineAliceBob,
	// lineBobAlice) and keep lineAliceCarol, lineBroadcast, lineJoined.
	if strings.Contains(got, "hi bob") {
		t.Errorf("tail=3 leaked the oldest line: %q", got)
	}
	for _, want := range []string{"hi carol", "hello room", "joined"} {
		if !strings.Contains(got, want) {
			t.Errorf("backlog missing %q: %q", want, got)
		}
	}
}

func TestWatchFollowsNewLines(t *testing.T) {
	home := withTempHome(t)
	var buf syncBuf
	runWatchInBackground(t, &buf, "", 30, false, false)

	writeLog(t, home, lineBobAlice)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "hi alice") }) {
		t.Fatalf("new line not followed: %q", buf.String())
	}
}

func TestWatchFilterNarrowsToNick(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, allLines...)

	var buf syncBuf
	runWatchInBackground(t, &buf, "alice", 30, false, false)

	if !waitFor(t, time.Second, func() bool {
		return strings.Contains(buf.String(), "hi alice") && strings.Contains(buf.String(), "hello room")
	}) {
		t.Fatalf("filter @alice missed expected lines: %q", buf.String())
	}
	got := buf.String()
	// `dave joined` mentions neither alice nor broadcast — must not pass.
	if strings.Contains(got, "dave joined") {
		t.Errorf("filter @alice leaked unrelated event: %q", got)
	}
}

func TestWatchNoColorStripsAnsi(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineAliceBob)

	var buf syncBuf
	runWatchInBackground(t, &buf, "", 30, false, false)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "hi bob") }) {
		t.Fatalf("line not rendered: %q", buf.String())
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("no-color mode emitted ANSI escapes: %q", buf.String())
	}
}

func TestWatchColorAppliedToNicks(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineAliceBob)

	var buf syncBuf
	runWatchInBackground(t, &buf, "", 30, true, false)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "hi bob") }) {
		t.Fatalf("line not rendered: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("color mode did not emit ANSI escapes: %q", buf.String())
	}
}

func TestNickColorIsStablePerNick(t *testing.T) {
	if nickColor("alice") != nickColor("alice") {
		t.Fatalf("nickColor(alice) is unstable")
	}
	if nickColor("alice") != nickColor("alice") {
		t.Fatalf("nickColor(alice) is unstable across two calls")
	}
}

func TestNickColorDistributesAcrossNicks(t *testing.T) {
	seen := map[int]string{}
	collisions := 0
	for _, n := range []string{"alice", "bob", "carol", "dave", "erin", "frank"} {
		c := nickColor(n)
		if prev, ok := seen[c]; ok {
			collisions++
			_ = prev
		} else {
			seen[c] = n
		}
	}
	// Six nicks into a 12-entry palette should produce mostly distinct colors.
	// Demand at least 3 unique colors — any worse means the hash is degenerate.
	if len(seen) < 3 {
		t.Errorf("nick color distribution too narrow: %v", seen)
	}
}

func TestWatchEventsRenderDimItalic(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineJoined)

	var buf syncBuf
	runWatchInBackground(t, &buf, "", 30, true, false)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "joined") }) {
		t.Fatalf("event line not rendered: %q", buf.String())
	}
	if !strings.Contains(buf.String(), ansiDimIt) {
		t.Errorf("joined line missing dim-italic escape: %q", buf.String())
	}
}

func TestWatchEventsRenderDimItalicSuppressedWithNoColor(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home, lineJoined)

	var buf syncBuf
	runWatchInBackground(t, &buf, "", 30, false, false)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "joined") }) {
		t.Fatalf("event line not rendered: %q", buf.String())
	}
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("no-color event leaked ANSI: %q", buf.String())
	}
}

func TestWatchPathLineRendersFileMarker(t *testing.T) {
	home := withTempHome(t)
	writeLog(t, home,
		`{"ts":1100.000,"from":"alice","to":"@bob","path":"/x/foo.txt","note":"draft"}`)

	var buf syncBuf
	runWatchInBackground(t, &buf, "", 30, false, false)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "[file]") }) {
		t.Fatalf("path line missing [file]: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "/x/foo.txt") {
		t.Errorf("path line missing the path: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "— draft") {
		t.Errorf("path line missing the note: %q", buf.String())
	}
}

func TestWatchDateSwitchesTimestampFormat(t *testing.T) {
	withTempHome(t)
	r, _ := parseRecord(lineAliceBob)

	plain := renderWatch(r, false, false)
	dated := renderWatch(r, false, true)

	// HH:MM:SS only
	if !regexp.MustCompile(`^\d{2}:\d{2}:\d{2} `).MatchString(plain) {
		t.Errorf("plain render lacks HH:MM:SS prefix: %q", plain)
	}
	// YYYY-MM-DD HH:MM:SS
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2} `).MatchString(dated) {
		t.Errorf("--date render lacks full datestamp prefix: %q", dated)
	}
}

func TestWatchEmitsJoinedOnStartAndQuitOnExit(t *testing.T) {
	home := withTempHome(t)
	withFastWatch(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var buf syncBuf
	go func() { done <- watchSession(ctx, "watcher", "", 30, false, false, &buf) }()

	logFile := filepath.Join(home, "log.jsonl")
	hasEvent := func(event string) bool {
		b, err := os.ReadFile(logFile)
		if err != nil {
			return false
		}
		want := `"event":"` + event + `"`
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, `"from":"watcher"`) && strings.Contains(l, want) {
				return true
			}
		}
		return false
	}

	if !waitFor(t, time.Second, func() bool { return hasEvent("joined") }) {
		t.Fatalf("joined event not appended on watch start")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchSession did not return after cancel")
	}

	if !hasEvent("quit") {
		t.Errorf("quit event not appended on watch exit")
	}
}

func TestWatchRejectsWhenNoNickResolvable(t *testing.T) {
	cleanResolverEnv(t)
	if rc := run([]string{"watch"}); rc == 0 {
		t.Errorf("watch with no resolvable nick should fail")
	}
}

// TestWatchCrossProcess is the integration check: one process runs
// `agent-chat watch` while another sends a message; the first sees it on
// stdout. Mirrors TestListenCrossProcess.
func TestWatchCrossProcess(t *testing.T) {
	home := withTempHome(t)

	cmd := exec.Command(builtBinary, "watch", "--as", "viewer", "--no-color", "--tail", "0")
	cmd.Env = append(os.Environ(), "AGENT_CHAT_HOME="+home)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Settle: let the watch process emit `joined` and start polling so its
	// follow loop is past the byte we're about to write.
	time.Sleep(80 * time.Millisecond)

	sendCmd := exec.Command(builtBinary, "send", "--as", "bob", "@viewer", "hi cross-process watch")
	sendCmd.Env = append(os.Environ(), "AGENT_CHAT_HOME="+home)
	if out, err := sendCmd.CombinedOutput(); err != nil {
		t.Fatalf("send: %v: %s", err, out)
	}

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		br := bufio.NewReader(stdout)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- readResult{line, err}
				return
			}
			if strings.Contains(line, "hi cross-process watch") {
				ch <- readResult{line, nil}
				return
			}
		}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("watch stdout read err: %v", res.err)
		}
		if !strings.Contains(res.line, "hi cross-process watch") {
			t.Errorf("unexpected line: %q", res.line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch to render the send")
	}
}
