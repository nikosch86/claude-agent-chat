//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeCodex writes an executable stand-in for the codex binary that appends
// each invocation's argv (one arg per line, NUL-free) plus a blank separator
// to capturePath, and returns its path.
func fakeCodex(t *testing.T, capturePath string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + capturePath + "\nprintf '\\n' >> " + capturePath + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCodexEndToEnd drives the full codex wiring with real processes and a
// fake codex binary: hook-start --emit codex joins and spawns the detached
// bridge, a send from a peer is forwarded via `codex queue --thread ...`, and
// hook-stop terminates the bridge.
func TestCodexEndToEnd(t *testing.T) {
	home := withTempHome(t)
	cwd := t.TempDir()
	capture := filepath.Join(t.TempDir(), "queue-calls.txt")
	codex := fakeCodex(t, capture)

	env := append(os.Environ(),
		"AGENT_CHAT_HOME="+home,
		"AGENT_CHAT_CODEX_BIN="+codex,
		"CLAUDE_AGENT_CHAT_NICK=alice",
		"CLAUDE_AGENT_CHAT=",
	)

	// Join. stdin carries the codex hook payload; session_id doubles as the
	// queue thread id.
	start := exec.Command(builtBinary, "hook-start", "--emit", "codex")
	start.Dir = cwd
	start.Env = env
	start.Stdin = strings.NewReader(`{"session_id":"thread-e2e"}`)
	out, err := start.CombinedOutput()
	if err != nil {
		t.Fatalf("hook-start: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "additionalContext") {
		t.Fatalf("hook-start output missing additionalContext:\n%s", out)
	}

	pidPath := filepath.Join(home, "agents", "alice", "codex-bridge.pid")
	if !waitFor(t, 5*time.Second, func() bool {
		_, err := os.ReadFile(pidPath)
		return err == nil
	}) {
		t.Fatalf("bridge pidfile never appeared; bridge log:\n%s", readBridgeLog(t, home))
	}
	pid := readPidfile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) }) // belt and braces if the test fails early

	// A peer's message must land in the fake codex as one queue call.
	send := exec.Command(builtBinary, "send", "--as", "bob", "@alice", "hi from bob")
	send.Dir = cwd
	send.Env = env
	if out, err := send.CombinedOutput(); err != nil {
		t.Fatalf("send: %v\n%s", err, out)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		b, err := os.ReadFile(capture)
		return err == nil && strings.Contains(string(b), "hi from bob")
	}) {
		t.Fatalf("message never reached the fake codex; bridge log:\n%s", readBridgeLog(t, home))
	}
	b, _ := os.ReadFile(capture)
	args := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(args) < 5 || args[0] != "queue" || args[1] != "--thread" || args[2] != "thread-e2e" || args[3] != "--message" {
		t.Fatalf("unexpected codex argv: %q", args)
	}
	if want := "New agent-chat message:"; args[4] != want {
		t.Errorf("message must start with %q, got %q", want, args[4])
	}
	if len(args) < 6 || args[5] != "@bob: hi from bob" {
		t.Errorf("rendered body missing: %q", args)
	}

	// SessionEnd: hook-stop must terminate the bridge and release the claim.
	stop := exec.Command(builtBinary, "hook-stop")
	stop.Dir = cwd
	stop.Env = env
	stop.Stdin = strings.NewReader(`{"session_id":"thread-e2e"}`)
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("hook-stop: %v\n%s", err, out)
	}
	if !waitFor(t, 5*time.Second, func() bool {
		// Signal 0 probes liveness; ESRCH means the bridge is gone.
		return syscall.Kill(pid, syscall.Signal(0)) != nil
	}) {
		t.Errorf("bridge pid %d still alive after hook-stop; bridge log:\n%s", pid, readBridgeLog(t, home))
	}
	if !waitFor(t, 2*time.Second, func() bool {
		_, err := os.Stat(pidPath)
		return os.IsNotExist(err)
	}) {
		t.Errorf("pidfile still present after hook-stop")
	}
}

// stopCodexBridge must never signal a recycled pid that is not one of our
// bridges: the pidfile is removed but the process survives.
func TestStopCodexBridgeSparesForeignProcess(t *testing.T) {
	home := withTempHome(t)

	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleep.Process.Kill()
		_, _ = sleep.Process.Wait()
	})

	pidPath := filepath.Join(home, "agents", "alice", "codex-bridge.pid")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(sleep.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stopCodexBridge("alice")

	if err := sleep.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("foreign pid was killed by stopCodexBridge: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale pidfile should be removed even when the pid is foreign")
	}
}

func readPidfile(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("pidfile contents %q: %v", b, err)
	}
	return pid
}

func readBridgeLog(t *testing.T, home string) string {
	t.Helper()
	return readBridgeLogFor(t, home, "alice")
}

func readBridgeLogFor(t *testing.T, home, nick string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, "agents", nick, "codex-bridge.log"))
	return string(b)
}

// holdFlock takes Codex's role: it creates the thread writer lock file and
// holds an exclusive flock on it until the returned release func is called.
func holdFlock(t *testing.T, path string) (release func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}

func TestProbeThreadLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-writer-locks", "t.lock")
	if got := probeThreadLock(path); got != lockMissing {
		t.Errorf("missing file: got %v, want lockMissing", got)
	}
	release := holdFlock(t, path)
	if got := probeThreadLock(path); got != lockHeld {
		t.Errorf("held lock: got %v, want lockHeld", got)
	}
	release()
	if got := probeThreadLock(path); got != lockFree {
		t.Errorf("released lock: got %v, want lockFree", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("probe must not remove the lock file: %v", err)
	}
}

// The watcher ends the bridge only after the lock was seen held and then
// observed free for deadProbes consecutive probes; a lock that never appears
// (remote thread, slow start) never trips it.
func TestWatchCodexThreadExitsWhenLockReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-writer-locks", "t.lock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log, _ := os.CreateTemp(t.TempDir(), "log")
	defer log.Close()

	done := make(chan struct{})
	go func() {
		watchCodexThread(ctx, cancel, path, 10*time.Millisecond, 3, log)
		close(done)
	}()

	// Never held: stays alive through many probes.
	time.Sleep(100 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("watcher exited although the lock was never seen held")
	default:
	}

	release := holdFlock(t, path)
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("watcher exited while the lock was held")
	default:
	}

	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not exit after the lock was released")
	}
	if ctx.Err() == nil {
		t.Error("watcher must cancel the bridge context")
	}
	b, _ := os.ReadFile(log.Name())
	if !strings.Contains(string(b), "no longer held") {
		t.Errorf("watcher should log why it exited, got: %q", b)
	}
}

// A lock deleted outright (Codex removes it on thread close) counts as gone too.
func TestWatchCodexThreadExitsWhenLockDeleted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "thread-writer-locks", "t.lock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := holdFlock(t, path)
	done := make(chan struct{})
	go func() {
		watchCodexThread(ctx, cancel, path, 10*time.Millisecond, 3, os.Stderr)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	release()
	os.Remove(path)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not exit after the lock file was deleted")
	}
}

// End to end: a bridge whose codex thread dies uncleanly (no SessionEnd hook)
// exits on its own via the writer-lock probe and leaves no pidfile behind.
func TestCodexBridgeExitsWhenThreadDies(t *testing.T) {
	home := withTempHome(t)
	codexHome := t.TempDir()
	lock := filepath.Join(codexHome, "thread-writer-locks", "thread-dead.lock")
	release := holdFlock(t, lock)

	env := append(os.Environ(),
		"AGENT_CHAT_HOME="+home,
		"CODEX_HOME="+codexHome,
		"AGENT_CHAT_CODEX_BIN="+fakeCodex(t, filepath.Join(t.TempDir(), "calls")),
		"AGENT_CHAT_CODEX_PROBE_MS=20",
	)
	bridge := exec.Command(builtBinary, "codex-bridge", "--thread", "thread-dead", "--as", "carol")
	bridge.Env = env
	if out, err := bridge.CombinedOutput(); err != nil {
		t.Fatalf("codex-bridge: %v\n%s", err, out)
	}
	pidPath := filepath.Join(home, "agents", "carol", "codex-bridge.pid")
	if !waitFor(t, 5*time.Second, func() bool { _, err := os.ReadFile(pidPath); return err == nil }) {
		t.Fatalf("bridge pidfile never appeared; log:\n%s", readBridgeLogFor(t, home, "carol"))
	}
	pid := readPidfile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	time.Sleep(200 * time.Millisecond) // several held probes
	if syscall.Kill(pid, syscall.Signal(0)) != nil {
		t.Fatalf("bridge died while the thread lock was held; log:\n%s", readBridgeLogFor(t, home, "carol"))
	}
	release() // codex dies without running SessionEnd
	if !waitFor(t, 5*time.Second, func() bool { return syscall.Kill(pid, syscall.Signal(0)) != nil }) {
		t.Fatalf("bridge outlived its codex thread; log:\n%s", readBridgeLogFor(t, home, "carol"))
	}
	if !waitFor(t, 2*time.Second, func() bool { _, err := os.Stat(pidPath); return os.IsNotExist(err) }) {
		t.Errorf("pidfile still present after the bridge exited on its own")
	}
}
