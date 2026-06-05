//go:build unix

package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadLockHolder covers the three readLockHolder outcomes: a missing file,
// a file whose contents are not an integer, and a well-formed pid stamp.
func TestReadLockHolder(t *testing.T) {
	withTempHome(t)
	p := listenerLockPath("alice")

	if got := readLockHolder(p); got != 0 {
		t.Errorf("readLockHolder(missing) = %d, want 0", got)
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readLockHolder(p); got != 0 {
		t.Errorf("readLockHolder(garbage) = %d, want 0", got)
	}

	if err := os.WriteFile(p, []byte(" 4321 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readLockHolder(p); got != 4321 {
		t.Errorf("readLockHolder(valid) = %d, want 4321", got)
	}
}

// TestRequestIncumbentStepDownGuards proves the no-op guards never signal: a
// non-positive pid, our own pid, and a live pid that is not one of our
// listeners (a sleep we spawned) all return without sending SIGTERM.
func TestRequestIncumbentStepDownGuards(t *testing.T) {
	// pid <= 0 and our own pid are pure early returns.
	requestIncumbentStepDown(0)
	requestIncumbentStepDown(-1)
	requestIncumbentStepDown(os.Getpid())

	// A process we own that is not a listener: the isOwnListener guard must
	// stop the SIGTERM, so the sleep survives until we kill it ourselves.
	sleep := exec.Command("sleep", "30")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sleep.Process.Kill()
		_, _ = sleep.Process.Wait()
	})

	requestIncumbentStepDown(sleep.Process.Pid)

	// Signal 0 probes liveness without delivering anything; the sleep must
	// still be alive, proving the guard suppressed the SIGTERM.
	if err := sleep.Process.Signal(syscall.Signal(0)); err != nil {
		t.Errorf("non-listener pid was killed by requestIncumbentStepDown: %v", err)
	}
}

// TestTryLockListenerFastPathStampsPid takes the free-lock fast path and
// verifies the holder pid was stamped into the lock file for handover.
func TestTryLockListenerFastPathStampsPid(t *testing.T) {
	withTempHome(t)

	release := tryLockListener("alice")
	defer release()

	if got := readLockHolder(listenerLockPath("alice")); got != os.Getpid() {
		t.Errorf("lock file pid = %d, want this process %d", got, os.Getpid())
	}
}

// TestTryLockListenerFailsOpenWhenNonListenerHoldsLock exercises the contended
// path in-process: a spawned non-listener holds the flock, so takeover cannot
// SIGTERM it, the bounded acquire expires, and tryLockListener fails open.
func TestTryLockListenerFailsOpenWhenNonListenerHoldsLock(t *testing.T) {
	withTempHome(t)
	prevGrace := listenerTakeoverGrace
	listenerTakeoverGrace = 150 * time.Millisecond
	t.Cleanup(func() { listenerTakeoverGrace = prevGrace })

	p := listenerLockPath("alice")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}

	// A spawned non-listener holds an exclusive flock on the file; its argv is
	// plainly not an agent-chat listener, so takeover may not SIGTERM it.
	holder := startFlockHolder(t, p)
	t.Cleanup(holder)

	if !waitFor(t, 5*time.Second, func() bool { return lockIsHeld(t, p) }) {
		t.Fatal("holder never acquired the lock")
	}

	start := time.Now()
	release := tryLockListener("alice")
	defer release()
	if elapsed := time.Since(start); elapsed < listenerTakeoverGrace {
		t.Errorf("tryLockListener returned in %s, expected to wait the full grace", elapsed)
	}
}

// flockHolderSrc is a standalone program that takes an exclusive flock on the
// path in os.Args[1], prints "locked" once held, then blocks until its stdin
// closes, so it cannot outlive the test even when SIGKILL is not forwarded.
const flockHolderSrc = `package main

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

func main() {
	f, err := os.OpenFile(os.Args[1], os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		os.Exit(1)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		os.Exit(1)
	}
	fmt.Println("locked")
	_, _ = io.Copy(io.Discard, os.Stdin)
}
`

// startFlockHolder spawns a child process holding an exclusive flock on path
// and returns a cleanup that kills it. It waits for the child's "locked"
// readiness line before returning.
func startFlockHolder(t *testing.T, path string) func() {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "holder.go")
	if err := os.WriteFile(src, []byte(flockHolderSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "run", src, path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		buf := make([]byte, len("locked\n"))
		_, _ = io.ReadFull(stdout, buf)
		close(ready)
	}()
	stop := func() {
		// Closing stdin ends the holder itself; killing go run alone would
		// orphan it, since SIGKILL is not forwarded to the child.
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		stop()
		t.Fatal("flock holder never signalled readiness")
	}
	return stop
}

// lockIsHeld reports whether the file at path currently holds an exclusive
// flock that a non-blocking acquire from this process cannot take.
func lockIsHeld(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	return true
}
