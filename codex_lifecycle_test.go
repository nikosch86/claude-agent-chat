package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The farewell is queued only for an eviction that leaves a live session
// deaf. A quiet stop means delivery continues elsewhere or the session is
// ending; a session the bridge itself judged gone has nobody to tell; and an
// eviction is silent only when the thread is demonstrably gone — lock seen
// held earlier, every fresh probe free now. When in doubt, the session is
// told: a stale farewell replayed on resume is a nuisance, a live session
// left silently deaf loses messages.
func TestBridgeFarewellPolicy(t *testing.T) {
	held, free, missing, unknown := lockHeld, lockFree, lockMissing, lockUnknown
	cases := []struct {
		name     string
		reason   bridgeExitReason
		seenHeld bool
		samples  []lockState
		want     bool
	}{
		{"evicted, thread alive", exitEvicted, true, []lockState{held, held, held, held}, true},
		{"evicted, lock unheld for a moment then held again", exitEvicted, true, []lockState{free, held, held, held}, true},
		{"evicted, lock state unknown (no flock on this platform)", exitEvicted, true, []lockState{unknown, unknown, unknown, unknown}, true},
		{"evicted after /new or an unclean exit: seen held, now free throughout", exitEvicted, true, []lockState{free, free, free, free}, false},
		{"evicted, lock file deleted after having been held", exitEvicted, true, []lockState{missing, missing, missing, missing}, false},
		{"evicted, lock never seen held (remote thread) and missing now", exitEvicted, false, nil, true},
		{"evicted, lock never seen held but free now", exitEvicted, false, []lockState{free, free, free, free}, true},
		{"signalled before any reason was recorded", exitUnknown, true, []lockState{held}, true},
		{"quiet stop: hook-stop or same-thread handover", exitQuiet, true, []lockState{held, held, held, held}, false},
		{"session gone per watcher or delivery grace", exitSessionGone, true, nil, false},
	}
	for _, c := range cases {
		if got := wantFarewell(c.reason, c.seenHeld, c.samples); got != c.want {
			t.Errorf("%s: wantFarewell = %v, want %v", c.name, got, c.want)
		}
	}
}

// bridgeRun.farewell wires the policy to the probe: an evicted bridge
// re-samples the lock, and only a unanimous "free" after "seen held" keeps
// it quiet.
func TestBridgeRunFarewellSamplesTheLock(t *testing.T) {
	oldN, oldGap := bridgeFarewellSamples, bridgeFarewellSampleGap
	bridgeFarewellSamples, bridgeFarewellSampleGap = 3, time.Millisecond
	t.Cleanup(func() { bridgeFarewellSamples, bridgeFarewellSampleGap = oldN, oldGap })

	newRun := func(states ...lockState) *bridgeRun {
		i := 0
		b := &bridgeRun{nick: "alice", thread: "t-1", cancel: func() {}, log: io.Discard}
		b.probe = func() lockState {
			s := states[i%len(states)]
			i++
			return s
		}
		return b
	}

	b := newRun(lockFree)
	b.seenHeld.Store(true)
	b.stop(exitEvicted)
	if got := b.farewell(); got != "" {
		t.Errorf("seen held, now free on every sample: expected no farewell, got %q", got)
	}

	b = newRun(lockFree, lockHeld, lockFree)
	b.seenHeld.Store(true)
	b.stop(exitEvicted)
	if got := b.farewell(); !strings.Contains(got, "inbox bridge stopped") {
		t.Errorf("one held sample means alive: expected a farewell, got %q", got)
	}

	b = newRun(lockMissing)
	b.stop(exitEvicted) // never seen held
	if got := b.farewell(); !strings.Contains(got, "inbox bridge stopped") {
		t.Errorf("lock never seen held says nothing: expected a farewell, got %q", got)
	}

	b = newRun(lockHeld)
	b.seenHeld.Store(true)
	b.stop(exitQuiet)
	if got := b.farewell(); got != "" {
		t.Errorf("quiet stop must not queue a farewell, got %q", got)
	}
}

// The farewell names the exact recovery command and says where to run it: a
// bridge started inside Codex's sandbox cannot deliver and dies with the tool
// call, and `agent-chat listen` there cannot deliver into the session either.
func TestCodexFarewellNamesThreadAndNick(t *testing.T) {
	msg := codexBridgeFarewell("alice", "thread-1")
	for _, want := range []string{
		"[agent-chat] inbox bridge stopped",
		"agent-chat codex-bridge --thread thread-1 --as alice",
		"OUTSIDE the Codex sandbox",
		"agent-chat history --to me --tail 20 --format text",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("farewell missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "<this session id>") || strings.Contains(msg, "<nick>") {
		t.Errorf("farewell still carries placeholders instead of the real ids:\n%s", msg)
	}
}

// The farewell bypasses the retry bookkeeping: a backoff left over from an
// earlier failed delivery must not swallow the last thing the bridge says.
func TestBridgeWriterDeliversNoticeDespiteBackoff(t *testing.T) {
	now := time.Unix(0, 0)
	var got []string
	fail := true
	w := &bridgeWriter{
		cancel: func() {},
		now:    func() time.Time { return now },
		deliver: func(msg string) error {
			if fail {
				return context.DeadlineExceeded
			}
			got = append(got, msg)
			return nil
		},
	}
	w.Write([]byte(`{"ts":1.000,"from":"bob","to":"@alice","text":"x"}` + "\n")) // fails, starts the backoff
	fail = false
	w.Write([]byte("[agent-chat] inbox bridge stopped\n"))
	if len(got) != 1 || got[0] != "[agent-chat] inbox bridge stopped" {
		t.Fatalf("notice not delivered inside the backoff window: %q", got)
	}
}

func TestBridgeArgvThread(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"/x/agent-chat", "codex-bridge", "--foreground", "--thread", "t-1", "--as", "alice"}, "t-1"},
		{[]string{"/x/agent-chat", "codex-bridge", "--thread=t-2", "--as", "alice"}, "t-2"},
		{[]string{"/x/agent-chat", "codex-bridge", "--thread"}, ""},
		{[]string{"/x/agent-chat", "listen", "--as", "alice"}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := bridgeArgvThread(c.argv); got != c.want {
			t.Errorf("bridgeArgvThread(%q) = %q, want %q", c.argv, got, c.want)
		}
	}
}

// bridgeAlive needs a pidfile whose pid is a codex-bridge for *this* thread;
// a missing pidfile, a foreign pid, or a bridge for another thread do not
// count — hook-start must not skip the spawn on stale state.
func TestBridgeAliveRejectsStaleAndForeignPids(t *testing.T) {
	withTempHome(t)
	if _, ok := bridgeAlive("alice", "t-1"); ok {
		t.Error("alive without any pidfile")
	}
	// Our own pid is alive but is not a codex-bridge process.
	if err := writeBridgePid("alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := bridgeAlive("alice", "t-1"); ok {
		t.Error("alive for a pid that is not a codex-bridge")
	}
}

// The pidfile is removed only while it still names this process, so a
// successor's pidfile survives a slow incumbent's exit.
func TestRemoveBridgePidIfOurs(t *testing.T) {
	withTempHome(t)
	if err := writeBridgePid("alice"); err != nil {
		t.Fatal(err)
	}
	removeBridgePidIfOurs("alice")
	if _, err := os.Stat(bridgePidPath("alice")); !os.IsNotExist(err) {
		t.Error("own pidfile not removed")
	}
	if err := os.MkdirAll(bridgePidPath("alice")[:len(bridgePidPath("alice"))-len("/codex-bridge.pid")], 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bridgePidPath("alice"), []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	removeBridgePidIfOurs("alice")
	if _, err := os.Stat(bridgePidPath("alice")); err != nil {
		t.Error("a successor's pidfile was removed")
	}
}

// The first stop reason wins: a takeover signal that lands after the watcher
// already gave up on the session must not turn a quiet exit into a farewell.
func TestBridgeRunFirstStopWins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	b := &bridgeRun{nick: "alice", thread: "t", cancel: cancel, log: io.Discard}
	b.stop(exitSessionGone)
	b.stop(exitEvicted)
	if got := bridgeExitReason(b.reason.Load()); got != exitSessionGone {
		t.Errorf("reason = %v, want the first recorded (%v)", got, exitSessionGone)
	}
	if ctx.Err() == nil {
		t.Error("stop must cancel the bridge context")
	}
}

// fakeServingBridge lays down what a live bridge leaves on disk: a pidfile
// and a lock file both naming pid, and a fresh heartbeat.
func fakeServingBridge(t *testing.T, nick string, pid int) {
	t.Helper()
	if err := os.MkdirAll(bridgePidPath(nick)[:len(bridgePidPath(nick))-len("/codex-bridge.pid")], 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := []byte(strconv.Itoa(pid) + "\n")
	if err := os.WriteFile(bridgePidPath(nick), stamp, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(listenerLockPath(nick), stamp, 0o644); err != nil {
		t.Fatal(err)
	}
	touchListenerHeartbeat(nick)
}

// bridgeServing is judged from the filesystem alone, and every piece must
// agree: a pidfile that does not match the lock holder (a Claude listener
// took the nick after the bridge was killed) or a stale heartbeat means no
// bridge is serving.
func TestBridgeServingNeedsMatchingLockHolderAndFreshHeartbeat(t *testing.T) {
	withTempHome(t)
	now := time.Now()
	if bridgeServing("alice", now) {
		t.Fatal("serving with nothing on disk")
	}
	fakeServingBridge(t, "alice", 4242)
	if !bridgeServing("alice", now) {
		t.Fatal("not serving with pidfile, matching lock holder, and fresh heartbeat")
	}
	if bridgeServing("alice", now.Add(bridgeStaleThreshold+time.Second)) {
		t.Error("serving on a stale heartbeat")
	}
	if err := os.WriteFile(listenerLockPath("alice"), []byte("4343\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if bridgeServing("alice", now) {
		t.Error("serving although another process holds the listener lock")
	}
}

// listen inside a Codex session: with no bridge it prints the restart
// command and exits 1 without starting anything; with a serving bridge it
// announces the attachment and idles until stopped (exit 0); a bridge that
// disappears for good ends the attachment with exit 1 — never a fallback
// listener. Short blips are ridden out.
func TestListenInsideCodexFollowsBridge(t *testing.T) {
	withTempHome(t)
	oldHB, oldMisses := listenHeartbeatInterval, bridgeAttachMisses
	listenHeartbeatInterval, bridgeAttachMisses = 10*time.Millisecond, 5
	t.Cleanup(func() { listenHeartbeatInterval, bridgeAttachMisses = oldHB, oldMisses })

	var out bytes.Buffer
	if rc := listenInsideCodex(context.Background(), "alice", "t-1", &out); rc != 1 {
		t.Fatalf("rc = %d without a bridge, want 1", rc)
	}
	if !strings.Contains(out.String(), "codex-bridge --thread t-1 --as alice") {
		t.Fatalf("no-bridge notice must name the restart command, got %q", out.String())
	}
	if _, err := os.Stat(listenerLockPath("alice")); !os.IsNotExist(err) {
		t.Fatal("listen inside Codex must not touch the listener lock")
	}

	out.Reset()
	fakeServingBridge(t, "alice", 4242)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- listenInsideCodex(ctx, "alice", "t-1", &out) }()
	time.Sleep(100 * time.Millisecond)
	select {
	case rc := <-done:
		t.Fatalf("attach returned %d while the bridge was serving", rc)
	default:
	}
	if !strings.Contains(out.String(), "[agent-chat] attached") {
		t.Errorf("attach notice not printed, got %q", out.String())
	}

	// A blip shorter than the miss window: pidfile briefly gone (handover).
	if err := os.Remove(bridgePidPath("alice")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	fakeServingBridge(t, "alice", 4242)
	time.Sleep(100 * time.Millisecond)
	select {
	case rc := <-done:
		t.Fatalf("attach returned %d on a short blip", rc)
	default:
	}

	cancel()
	select {
	case rc := <-done:
		if rc != 0 {
			t.Errorf("rc = %d after being stopped while attached, want 0", rc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attach did not return after ctx was cancelled")
	}

	// Bridge gone for good: exit 1 with the restart command, no listener.
	out.Reset()
	go func() { done <- listenInsideCodex(context.Background(), "alice", "t-1", &out) }()
	time.Sleep(30 * time.Millisecond)
	if err := os.Remove(bridgePidPath("alice")); err != nil {
		t.Fatal(err)
	}
	select {
	case rc := <-done:
		if rc != 1 {
			t.Errorf("rc = %d after the bridge went away, want 1", rc)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attach did not notice the bridge going away")
	}
	if !strings.Contains(out.String(), "the codex-bridge stopped") || !strings.Contains(out.String(), "codex-bridge --thread t-1 --as alice") {
		t.Errorf("hand-off notice must say the bridge stopped and name the restart command, got %q", out.String())
	}
}
