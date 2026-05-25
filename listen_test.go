package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureStderr is the stderr analogue of captureStdout in history_test.go.
func captureStderr(t *testing.T, fn func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	rc := fn()
	w.Close()
	os.Stderr = orig
	return <-done, rc
}

// withFastListen shrinks every listen-related interval so tests don't have to
// sleep for the production defaults. Restored on cleanup.
func withFastListen(t *testing.T) {
	t.Helper()
	prevPoll := listenPollInterval
	prevHB := listenHeartbeatInterval
	prevStale := listenerStaleThreshold
	listenPollInterval = 10 * time.Millisecond
	listenHeartbeatInterval = 20 * time.Millisecond
	listenerStaleThreshold = 200 * time.Millisecond
	t.Cleanup(func() {
		listenPollInterval = prevPoll
		listenHeartbeatInterval = prevHB
		listenerStaleThreshold = prevStale
	})
}

// syncBuf is an io.Writer the test main goroutine can poll safely while a
// background goroutine writes into it.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, max time.Duration, check func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if check() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return check()
}

// runListenInBackground pre-seeds the cursor at the current log EOF and then
// starts listenLoop in a goroutine. Pre-seeding sidesteps a race the
// production loop avoids by design: without a cursor, listenLoop defaults to
// currentLogSize() so manual `listen` runs don't replay history — but if the
// test's first writeLog wins that sample, the test would never see the
// emit. Writing the cursor here makes the test's "from this point onward"
// intent explicit and matches the production case (post-hook-start) one to one.
func runListenInBackground(t *testing.T, nick string, out io.Writer) (context.CancelFunc, <-chan int) {
	t.Helper()
	if _, ok := readCursor(nick); !ok {
		if err := writeCursor(nick, currentLogSize()); err != nil {
			t.Fatalf("seed cursor: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- listenLoop(ctx, nick, out) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Errorf("listen loop did not exit within 2s")
		}
	})
	return cancel, done
}

func TestListenEmitsDirectAndBroadcast(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)
	var buf syncBuf
	runListenInBackground(t, "alice", &buf)

	writeLog(t, home, lineBobAlice)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "hi alice") }) {
		t.Fatalf("direct @alice line not emitted: %q", buf.String())
	}

	writeLog(t, home, lineBroadcast)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "hello room") }) {
		t.Errorf("broadcast not emitted: %q", buf.String())
	}
}

func TestListenFiltersNonMatching(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)
	var buf syncBuf
	runListenInBackground(t, "alice", &buf)

	writeLog(t, home, lineAliceBob, lineAliceCarol)
	// Give the poll a chance to run; expect nothing.
	time.Sleep(80 * time.Millisecond)
	if got := buf.String(); got != "" {
		t.Errorf("non-matching traffic leaked: %q", got)
	}
}

func TestListenCursorReflectsEOFAfterEmit(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)
	var buf syncBuf
	runListenInBackground(t, "alice", &buf)

	writeLog(t, home, lineBobAlice)
	waitFor(t, time.Second, func() bool { return strings.Contains(buf.String(), "hi alice") })
	// Cursor should sit at the byte just past the line we emitted.
	waitFor(t, time.Second, func() bool {
		off, ok := readCursor("alice")
		if !ok {
			return false
		}
		fi, err := os.Stat(filepath.Join(home, "log.jsonl"))
		return err == nil && off == fi.Size()
	})
	off, ok := readCursor("alice")
	fi, _ := os.Stat(filepath.Join(home, "log.jsonl"))
	if !ok || off != fi.Size() {
		t.Errorf("cursor = %d (ok=%v), want %d (log EOF)", off, ok, fi.Size())
	}
}

func TestListenReplayAcrossRestarts(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)

	// First run: consume one message, then die.
	if err := writeCursor("alice", currentLogSize()); err != nil {
		t.Fatal(err)
	}
	var buf1 syncBuf
	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan int, 1)
	go func() { done1 <- listenLoop(ctx1, "alice", &buf1) }()
	writeLog(t, home, lineBobAlice)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf1.String(), "hi alice") }) {
		t.Fatal("first message not emitted before kill")
	}
	cancel1()
	<-done1

	// Message arrives while listener is dead.
	const secondLine = `{"ts":1010.000,"from":"bob","to":"@alice","text":"second after death"}`
	writeLog(t, home, secondLine)

	// Second run: should catch the missed message, and NOT replay the first.
	var buf2 syncBuf
	runListenInBackground(t, "alice", &buf2)
	if !waitFor(t, time.Second, func() bool { return strings.Contains(buf2.String(), "second after death") }) {
		t.Fatalf("missed message not replayed on restart: %q", buf2.String())
	}
	if strings.Contains(buf2.String(), "hi alice") {
		t.Errorf("already-emitted message replayed across restart: %q", buf2.String())
	}
}

// TestListenCrossProcess covers the acceptance criterion calling out "listen
// in one shell, send from another → JSON appears within ~1s". The in-process
// tests above exercise the same loop but in a single process; this one is
// the real two-process integration check.
func TestListenCrossProcess(t *testing.T) {
	home := withTempHome(t)

	cmd := exec.Command(builtBinary, "listen", "--as", "alice")
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

	// Brief settle so listen establishes its cursor at current EOF before
	// our send appends; otherwise we'd be racing the very first scan.
	time.Sleep(50 * time.Millisecond)

	sendCmd := exec.Command(builtBinary, "send", "--as", "bob", "@alice", "hi cross-process")
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
		line, err := br.ReadString('\n')
		ch <- readResult{line, err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read listen stdout: %v", res.err)
		}
		if !strings.Contains(res.line, "hi cross-process") {
			t.Errorf("listen stdout = %q", res.line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for listen to emit cross-process send")
	}
}

func TestSelfHealWarningFiresWhenListenerDeadAndUnread(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)
	// Pre-seed: cursor at 0, log has a matching line from someone else, no heartbeat.
	writeLog(t, home, lineBobAlice)
	if err := writeCursor("alice", 0); err != nil {
		t.Fatal(err)
	}

	stderr, rc := captureStderr(t, func() int {
		return run([]string{"send", "--as", "alice", "@bob", "ping"})
	})
	if rc != 0 {
		t.Fatalf("send rc = %d", rc)
	}
	if !strings.Contains(stderr, "listener appears stopped") {
		t.Errorf("warning missing on stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "1 unread") {
		t.Errorf("expected exactly 1 unread, got: %q", stderr)
	}
	if !strings.Contains(stderr, "Monitor(agent-chat listen") {
		t.Errorf("warning missing self-heal suggestion: %q", stderr)
	}
}

func TestSelfHealWarningSuppressedWhenCursorCurrent(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)
	writeLog(t, home, lineBobAlice)
	fi, _ := os.Stat(filepath.Join(home, "log.jsonl"))
	if err := writeCursor("alice", fi.Size()); err != nil {
		t.Fatal(err)
	}

	stderr, rc := captureStderr(t, func() int {
		return run([]string{"send", "--as", "alice", "@bob", "ping"})
	})
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if strings.Contains(stderr, "listener appears stopped") {
		t.Errorf("warning should be suppressed when cursor is current: %q", stderr)
	}
}

func TestSelfHealWarningSuppressedWhenHeartbeatFresh(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)
	writeLog(t, home, lineBobAlice)
	if err := writeCursor("alice", 0); err != nil {
		t.Fatal(err)
	}
	touchListenerHeartbeat("alice")

	stderr, _ := captureStderr(t, func() int {
		return run([]string{"send", "--as", "alice", "@bob", "ping"})
	})
	if strings.Contains(stderr, "listener appears stopped") {
		t.Errorf("warning should be suppressed when heartbeat is fresh: %q", stderr)
	}
}

func TestSelfHealWarningExcludesSelfSends(t *testing.T) {
	// A line authored by `nick` past the cursor must not count as "unread",
	// otherwise broadcasting to yourself would produce a phantom warning.
	home := withTempHome(t)
	withFastListen(t)
	writeLog(t, home, `{"ts":1000.000,"from":"alice","to":"*","text":"my own shout"}`)
	if err := writeCursor("alice", 0); err != nil {
		t.Fatal(err)
	}

	stderr, rc := captureStderr(t, func() int {
		return run([]string{"send", "--as", "alice", "@bob", "ping"})
	})
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if strings.Contains(stderr, "listener appears stopped") {
		t.Errorf("self-sent traffic should not trigger warning: %q", stderr)
	}
	_ = home
}

func TestSelfHealWarningFiresFromHistoryAndPeers(t *testing.T) {
	home := withTempHome(t)
	withFastListen(t)
	t.Setenv("AGENT_CHAT_NICK", "alice")
	writeLog(t, home, lineBobAlice)
	if err := writeCursor("alice", 0); err != nil {
		t.Fatal(err)
	}

	// history
	stderr, _ := captureStderr(t, func() int { return run([]string{"history"}) })
	if !strings.Contains(stderr, "listener appears stopped") {
		t.Errorf("history did not warn: %q", stderr)
	}

	// peers
	stderr2, _ := captureStderr(t, func() int { return run([]string{"peers"}) })
	if !strings.Contains(stderr2, "listener appears stopped") {
		t.Errorf("peers did not warn: %q", stderr2)
	}
}
