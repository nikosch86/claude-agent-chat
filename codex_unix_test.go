//go:build unix

package main

import (
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
	b, _ := os.ReadFile(filepath.Join(home, "agents", "alice", "codex-bridge.log"))
	return string(b)
}
