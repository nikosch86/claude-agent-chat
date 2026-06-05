//go:build unix

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// listenerTakeoverGrace bounds how long a starting listener waits for the
// incumbent holder to release the lock after being asked to step down. If it
// expires we fail open and run anyway — a transient duplicate beats silence.
var listenerTakeoverGrace = 3 * time.Second

// tryLockListener acquires the per-nick singleton lock, evicting any existing
// holder so the newest listener always wins. It returns a release func the
// caller must defer; listen then always runs.
//
// Takeover, not refusal. An flock(2) is pinned to an open file description, so
// a stale or detached holder — e.g. a listener orphaned when its session died —
// keeps the lock until it exits. The previous design *refused to start* when
// the lock was held, which made a fresh Monitor's `listen` exit silently while
// the orphan ate this nick's messages and advanced the shared cursor (so they
// did not even resurface as "missed" later). Instead we identify the incumbent,
// ask it to step down (SIGTERM, which listen handles by exiting cleanly and
// releasing), then take the lock with a bounded acquire. The most recently
// started listener is by definition the one wired to the session the user is
// looking at, so newest-wins is exactly the behaviour we want.
//
// It FAILS OPEN on every non-contention problem — cannot create the lock
// dir/file, an unexpected flock error, or the grace period expiring — by
// returning a no-op release and warning. Being unable to lock must never
// silence the listener.
func tryLockListener(nick string) func() {
	p := listenerLockPath(nick)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "listen: warning: lock dir unavailable (%v); continuing without singleton guard\n", err)
		return func() {}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: warning: lock file unavailable (%v); continuing without singleton guard\n", err)
		return func() {}
	}
	release := func() {
		// Best effort; the kernel also drops the lock on process exit.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}

	// Fast path: the lock is free.
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		recordLockHolder(f)
		return release
	case errors.Is(err, syscall.EWOULDBLOCK):
		// Contended — take over below.
	default:
		fmt.Fprintf(os.Stderr, "listen: warning: flock failed (%v); continuing without singleton guard\n", err)
		_ = f.Close()
		return func() {}
	}

	// Ask the incumbent to step down, then poll for the lock until it releases.
	requestIncumbentStepDown(readLockHolder(p))
	deadline := time.Now().Add(listenerTakeoverGrace)
	for {
		switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
		case err == nil:
			recordLockHolder(f)
			return release
		case errors.Is(err, syscall.EWOULDBLOCK) && time.Now().Before(deadline):
			time.Sleep(50 * time.Millisecond)
		default:
			fmt.Fprintf(os.Stderr, "listen: warning: could not take over the listener lock for %q within %s; continuing without singleton guard\n", nick, listenerTakeoverGrace)
			_ = f.Close()
			return func() {}
		}
	}
}

// requestIncumbentStepDown SIGTERMs the current lock holder so it exits cleanly
// and releases. It only signals a pid that still looks like one of our own
// listeners, so a recycled pid (the holder may have died between our read and
// now) can never take out an unrelated process. A pid of 0, our own pid, or one
// that no longer looks like a listener is a no-op: the bounded acquire loop
// then either wins (the holder is already gone) or fails open.
func requestIncumbentStepDown(pid int) {
	if pid <= 0 || pid == os.Getpid() || !isOwnListener(pid) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
}

// isOwnListener reports whether pid is one of our own `agent-chat listen`
// processes, read from /proc/<pid>/cmdline where available (Linux), else from
// `ps -o command=` (macOS and other no-/proc unixes). Unreadable means false.
func isOwnListener(pid int) bool {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		// argv is NUL-separated.
		return isListenArgv(strings.Split(string(b), "\x00"))
	}
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		// ps exits non-zero when the pid no longer exists.
		return false
	}
	// ps space-joins argv into one line, so fields of a spaced argument
	// match as individual tokens here.
	return isListenArgv(strings.Fields(string(out)))
}

// isListenArgv applies the listener heuristic to an argv: some argument names
// the binary and a bare "listen" verb appears alongside it.
func isListenArgv(argv []string) bool {
	var sawBinary, sawListen bool
	for _, a := range argv {
		if strings.Contains(a, "agent-chat") {
			sawBinary = true
		}
		if a == "listen" {
			sawListen = true
		}
	}
	return sawBinary && sawListen
}

// recordLockHolder stamps this process's pid into the lock file so a later
// listener taking over can find and signal it. The flock, not the contents, is
// the actual lock; the pid is only a rendezvous for a clean handover.
func recordLockHolder(f *os.File) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		return
	}
	_ = f.Sync()
}

func readLockHolder(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0
	}
	return pid
}
