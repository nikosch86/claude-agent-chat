//go:build unix

package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const bridgeFarewellMarker = "[agent-chat] inbox bridge stopped"

// codexTestEnv is the environment for the real-process codex tests: the fake
// codex binary records every `codex queue` argv into capture, and CODEX_HOME
// is a temp dir where the test plays Codex by holding thread writer locks.
// CODEX_THREAD_ID is cleared so a suite run from inside a Codex session does
// not put every child into attach mode.
func codexTestEnv(t *testing.T, home, codexHome, capture string) []string {
	t.Helper()
	return append(os.Environ(),
		"AGENT_CHAT_HOME="+home,
		"CODEX_HOME="+codexHome,
		"AGENT_CHAT_CODEX_BIN="+fakeCodex(t, capture),
		"AGENT_CHAT_CODEX_PROBE_MS=50",
		"CLAUDE_AGENT_CHAT_NICK=alice",
		"CLAUDE_AGENT_CHAT=",
		"CODEX_THREAD_ID=",
	)
}

func captureHas(t *testing.T, capture, want string) bool {
	t.Helper()
	b, _ := os.ReadFile(capture)
	return strings.Contains(string(b), want)
}

func hookStart(t *testing.T, cwd string, env []string, thread string) string {
	t.Helper()
	cmd := exec.Command(builtBinary, "hook-start", "--emit", "codex")
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdin = strings.NewReader(`{"session_id":"` + thread + `"}`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hook-start: %v\n%s", err, out)
	}
	return string(out)
}

func startBridge(t *testing.T, env []string, thread, nick string) int {
	t.Helper()
	cmd := exec.Command(builtBinary, "codex-bridge", "--thread", thread, "--as", nick)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("codex-bridge: %v\n%s", err, out)
	}
	home := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "AGENT_CHAT_HOME=") {
			home = strings.TrimPrefix(kv, "AGENT_CHAT_HOME=")
		}
	}
	// The pidfile appears before the bridge holds the listener lock; wait for
	// the "forwarding" line, which it logs once it does, so a test that then
	// starts a competing listener really is the newcomer.
	pidPath := filepath.Join(home, "agents", nick, "codex-bridge.pid")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(readBridgeLogFor(t, home, nick), "forwarding messages")
	}) {
		t.Fatalf("bridge never started serving; log:\n%s", readBridgeLogFor(t, home, nick))
	}
	pid := readPidfile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, syscall.Signal(0)) == nil }

// Codex re-fires SessionStart for the same thread on resume and after every
// automatic compaction. hook-start must leave the bridge already serving that
// thread alone — respawning evicted it and made it announce its exit into a
// session that was still being served. And hook-stop, at SessionEnd, stops
// the bridge quietly: a farewell parked in an ended thread would be replayed
// as the first turn of a later resume.
func TestCodexHookStartLeavesLiveBridgeAloneAndHookStopIsQuiet(t *testing.T) {
	home := withTempHome(t)
	cwd := t.TempDir()
	codexHome := t.TempDir()
	capture := filepath.Join(t.TempDir(), "queue-calls.txt")
	env := codexTestEnv(t, home, codexHome, capture)
	release := holdFlock(t, filepath.Join(codexHome, "thread-writer-locks", "thread-c.lock"))
	defer release()

	hookStart(t, cwd, env, "thread-c")
	pidPath := filepath.Join(home, "agents", "alice", "codex-bridge.pid")
	if !waitFor(t, 5*time.Second, func() bool {
		return strings.Contains(readBridgeLog(t, home), "forwarding messages")
	}) {
		t.Fatalf("bridge never started serving; log:\n%s", readBridgeLog(t, home))
	}
	pid := readPidfile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	// Compaction: SessionStart again, same session_id.
	out := hookStart(t, cwd, env, "thread-c")
	if !strings.Contains(out, "already serving this thread") {
		t.Errorf("second hook-start should report the live bridge; got:\n%s", out)
	}
	time.Sleep(300 * time.Millisecond)
	if got := readPidfile(t, pidPath); got != pid {
		t.Errorf("bridge was respawned on the second SessionStart: pid %d -> %d", pid, got)
	}
	if !alive(pid) {
		t.Fatalf("original bridge died on the second SessionStart; log:\n%s", readBridgeLog(t, home))
	}
	if captureHas(t, capture, bridgeFarewellMarker) {
		t.Errorf("a farewell was queued into a session that is still served; capture:\n%s", readFile(t, capture))
	}

	// Delivery still works through the original bridge.
	send := exec.Command(builtBinary, "send", "--as", "bob", "@alice", "after compaction")
	send.Dir = cwd
	send.Env = env
	if out, err := send.CombinedOutput(); err != nil {
		t.Fatalf("send: %v\n%s", err, out)
	}
	if !waitFor(t, 5*time.Second, func() bool { return captureHas(t, capture, "after compaction") }) {
		t.Fatalf("message not delivered after the second SessionStart; log:\n%s", readBridgeLog(t, home))
	}

	// SessionEnd while the thread lock is still held (Codex runs the hook
	// synchronously before shutting down): the bridge must go, quietly.
	stop := exec.Command(builtBinary, "hook-stop")
	stop.Dir = cwd
	stop.Env = env
	stop.Stdin = strings.NewReader(`{"session_id":"thread-c"}`)
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("hook-stop: %v\n%s", err, out)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !alive(pid) }) {
		t.Fatalf("bridge still alive after hook-stop; log:\n%s", readBridgeLog(t, home))
	}
	if captureHas(t, capture, bridgeFarewellMarker) {
		t.Errorf("hook-stop must stop the bridge without queueing a farewell; capture:\n%s", readFile(t, capture))
	}
	if !strings.Contains(readBridgeLog(t, home), "stopping quietly") {
		t.Errorf("bridge log should record the quiet stop:\n%s", readBridgeLog(t, home))
	}
}

// A listener taking the nick while the thread is alive is a real eviction:
// the session is now deaf and must be told, with the exact restart command.
func TestCodexBridgeEvictedByListenerQueuesFarewell(t *testing.T) {
	home := withTempHome(t)
	codexHome := t.TempDir()
	capture := filepath.Join(t.TempDir(), "queue-calls.txt")
	env := codexTestEnv(t, home, codexHome, capture)
	release := holdFlock(t, filepath.Join(codexHome, "thread-writer-locks", "thread-e.lock"))
	defer release()

	pid := startBridge(t, env, "thread-e", "alice")

	listen := exec.Command(builtBinary, "listen", "--as", "alice")
	listen.Env = env
	if err := listen.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listen.Process.Kill(); _, _ = listen.Process.Wait() })

	if !waitFor(t, 5*time.Second, func() bool { return !alive(pid) }) {
		t.Fatalf("bridge survived a listener takeover; log:\n%s", readBridgeLog(t, home))
	}
	if !waitFor(t, 5*time.Second, func() bool { return captureHas(t, capture, bridgeFarewellMarker) }) {
		t.Fatalf("evicted bridge queued no farewell; log:\n%s\ncapture:\n%s", readBridgeLog(t, home), readFile(t, capture))
	}
	if !captureHas(t, capture, "codex-bridge --thread thread-e --as alice") {
		t.Errorf("farewell should name the restart command; capture:\n%s", readFile(t, capture))
	}
}

// The same eviction after the thread ended (writer lock seen held, then
// released — /new, or an unclean exit the watcher has not reaped yet) queues
// nothing: there is nobody to tell, and Codex would replay it on resume.
func TestCodexBridgeEvictedFromDeadThreadStaysQuiet(t *testing.T) {
	home := withTempHome(t)
	codexHome := t.TempDir()
	capture := filepath.Join(t.TempDir(), "queue-calls.txt")
	env := codexTestEnv(t, home, codexHome, capture)
	release := holdFlock(t, filepath.Join(codexHome, "thread-writer-locks", "thread-d.lock"))

	pid := startBridge(t, env, "thread-d", "alice")
	time.Sleep(300 * time.Millisecond) // several probes see the lock held
	release()                          // Codex goes away

	listen := exec.Command(builtBinary, "listen", "--as", "alice")
	listen.Env = env
	if err := listen.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listen.Process.Kill(); _, _ = listen.Process.Wait() })

	if !waitFor(t, 5*time.Second, func() bool { return !alive(pid) }) {
		t.Fatalf("bridge survived a listener takeover; log:\n%s", readBridgeLog(t, home))
	}
	time.Sleep(300 * time.Millisecond)
	if captureHas(t, capture, bridgeFarewellMarker) {
		t.Errorf("farewell queued into a thread whose session is gone; capture:\n%s", readFile(t, capture))
	}
	if !strings.Contains(readBridgeLog(t, home), "session gone") {
		t.Errorf("bridge log should explain the quiet exit:\n%s", readBridgeLog(t, home))
	}
}

// A lock that was never seen held (remote app-server thread, unusual
// CODEX_HOME) says nothing about the session, so an eviction still tells it.
func TestCodexBridgeEvictedWithLockNeverSeenStillTells(t *testing.T) {
	home := withTempHome(t)
	codexHome := t.TempDir() // no thread-writer-locks at all
	capture := filepath.Join(t.TempDir(), "queue-calls.txt")
	env := codexTestEnv(t, home, codexHome, capture)

	pid := startBridge(t, env, "thread-n", "alice")

	listen := exec.Command(builtBinary, "listen", "--as", "alice")
	listen.Env = env
	if err := listen.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listen.Process.Kill(); _, _ = listen.Process.Wait() })

	if !waitFor(t, 5*time.Second, func() bool { return !alive(pid) }) {
		t.Fatalf("bridge survived a listener takeover; log:\n%s", readBridgeLog(t, home))
	}
	if !waitFor(t, 5*time.Second, func() bool { return captureHas(t, capture, bridgeFarewellMarker) }) {
		t.Fatalf("eviction with an unknown thread state must still queue a farewell; log:\n%s", readBridgeLog(t, home))
	}
}

// `agent-chat listen` inside a Codex session with no bridge serving does not
// start a listener (it could never deliver into the session, and inside the
// sandbox it could not be evicted later): it prints the restart command and
// exits 1, leaving no listener lock behind.
func TestListenInsideCodexWithoutBridgeExits(t *testing.T) {
	home := withTempHome(t)
	env := codexTestEnv(t, home, t.TempDir(), filepath.Join(t.TempDir(), "queue-calls.txt"))

	listen := exec.Command(builtBinary, "listen", "--as", "alice")
	listen.Env = append(env, "CODEX_THREAD_ID=thread-x")
	out, err := listen.CombinedOutput()
	if listen.ProcessState == nil || listen.ProcessState.ExitCode() != 1 {
		t.Fatalf("exit = %v (%v), want 1; output:\n%s", listen.ProcessState, err, out)
	}
	if !strings.Contains(string(out), "codex-bridge --thread thread-x --as alice") {
		t.Errorf("output must name the restart command:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "agents", "alice", "listener.lock")); !os.IsNotExist(err) {
		t.Error("listen inside Codex must not take the listener lock")
	}
}

// A newer bridge for the same thread replaces the old one quietly: delivery
// continues from the new process, so the session has nothing to learn.
func TestCodexBridgeSameThreadHandoverIsQuiet(t *testing.T) {
	home := withTempHome(t)
	codexHome := t.TempDir()
	capture := filepath.Join(t.TempDir(), "queue-calls.txt")
	env := codexTestEnv(t, home, codexHome, capture)
	release := holdFlock(t, filepath.Join(codexHome, "thread-writer-locks", "thread-h.lock"))
	defer release()

	first := startBridge(t, env, "thread-h", "alice")

	again := exec.Command(builtBinary, "codex-bridge", "--thread", "thread-h", "--as", "alice")
	again.Env = env
	if out, err := again.CombinedOutput(); err != nil {
		t.Fatalf("second codex-bridge: %v\n%s", err, out)
	}
	if !waitFor(t, 5*time.Second, func() bool { return !alive(first) }) {
		t.Fatalf("first bridge survived the handover; log:\n%s", readBridgeLog(t, home))
	}
	pidPath := filepath.Join(home, "agents", "alice", "codex-bridge.pid")
	if !waitFor(t, 5*time.Second, func() bool {
		b, err := os.ReadFile(pidPath)
		return err == nil && strings.TrimSpace(string(b)) != ""
	}) {
		t.Fatal("second bridge left no pidfile")
	}
	second := readPidfile(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(second, syscall.SIGKILL) })
	if second == first || !alive(second) {
		t.Fatalf("second bridge not running (pid %d, first %d)", second, first)
	}

	send := exec.Command(builtBinary, "send", "--as", "bob", "@alice", "after handover")
	send.Env = env
	if out, err := send.CombinedOutput(); err != nil {
		t.Fatalf("send: %v\n%s", err, out)
	}
	if !waitFor(t, 5*time.Second, func() bool { return captureHas(t, capture, "after handover") }) {
		t.Fatalf("second bridge did not deliver; log:\n%s", readBridgeLog(t, home))
	}
	if captureHas(t, capture, bridgeFarewellMarker) {
		t.Errorf("same-thread handover must not queue a farewell; capture:\n%s", readFile(t, capture))
	}
}

// `agent-chat listen` run from inside the Codex session (CODEX_THREAD_ID set)
// attaches to the serving bridge instead of evicting it: the bridge stays,
// keeps delivering, and no farewell is queued.
func TestListenInsideCodexAttachesToBridge(t *testing.T) {
	home := withTempHome(t)
	codexHome := t.TempDir()
	capture := filepath.Join(t.TempDir(), "queue-calls.txt")
	env := codexTestEnv(t, home, codexHome, capture)
	release := holdFlock(t, filepath.Join(codexHome, "thread-writer-locks", "thread-a.lock"))
	defer release()

	pid := startBridge(t, env, "thread-a", "alice")

	listen := exec.Command(builtBinary, "listen", "--as", "alice")
	listen.Env = append(env, "CODEX_THREAD_ID=thread-a")
	stdout, err := listen.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := listen.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listen.Process.Kill(); _, _ = listen.Process.Wait() })

	lines := make(chan string, 8)
	go func() {
		s := bufio.NewScanner(stdout)
		for s.Scan() {
			lines <- s.Text()
		}
		close(lines)
	}()
	select {
	case line := <-lines:
		if !strings.Contains(line, "[agent-chat] attached") {
			t.Fatalf("first listen line = %q, want the attach notice", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("listen printed nothing; bridge log:\n%s", readBridgeLog(t, home))
	}

	time.Sleep(300 * time.Millisecond)
	if !alive(pid) {
		t.Fatalf("attached listen evicted the bridge; log:\n%s", readBridgeLog(t, home))
	}

	send := exec.Command(builtBinary, "send", "--as", "bob", "@alice", "while attached")
	send.Env = env
	if out, err := send.CombinedOutput(); err != nil {
		t.Fatalf("send: %v\n%s", err, out)
	}
	if !waitFor(t, 5*time.Second, func() bool { return captureHas(t, capture, "while attached") }) {
		t.Fatalf("bridge stopped delivering while a listen was attached; log:\n%s", readBridgeLog(t, home))
	}
	select {
	case line, ok := <-lines:
		if ok {
			t.Errorf("attached listen must not stream messages itself, got %q", line)
		}
	default:
	}

	_ = listen.Process.Signal(syscall.SIGTERM)
	if err := listen.Wait(); err != nil {
		t.Errorf("attached listen should exit 0 on SIGTERM: %v", err)
	}
	if !alive(pid) {
		t.Errorf("bridge died when the attached listen exited")
	}
	if captureHas(t, capture, bridgeFarewellMarker) {
		t.Errorf("a farewell was queued although the bridge never stopped; capture:\n%s", readFile(t, capture))
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}
