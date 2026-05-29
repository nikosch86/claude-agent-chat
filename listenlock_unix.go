//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// tryLockListener takes the per-nick singleton lock with a non-blocking
// flock(2). It returns (release, true) when this process now holds the lock —
// the caller must defer release. It returns (nil, false) when another live
// process already holds it, so the caller must not start a second listener.
//
// The lock lives on an open file description, so the kernel releases it the
// instant this process exits — clean shutdown, SIGTERM, or SIGKILL — with no
// stale PID file to reap and no liveness probe to get wrong.
//
// On any non-contention problem (cannot create the lock dir/file, or an
// unexpected flock error) it FAILS OPEN: returns (no-op, true) and warns,
// because being unable to lock must never silence the listener outright — a
// rare duplicate is a far better failure than going deaf.
func tryLockListener(nick string) (func(), bool) {
	p := listenerLockPath(nick)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "listen: warning: lock dir unavailable (%v); continuing without singleton guard\n", err)
		return func() {}, true
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: warning: lock file unavailable (%v); continuing without singleton guard\n", err)
		return func() {}, true
	}
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
	case err == nil:
		return func() {
			// Best effort; the kernel also drops the lock on process exit.
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}, true
	case errors.Is(err, syscall.EWOULDBLOCK):
		// Held by another live listener — the one case where we refuse to run.
		_ = f.Close()
		return nil, false
	default:
		fmt.Fprintf(os.Stderr, "listen: warning: flock failed (%v); continuing without singleton guard\n", err)
		_ = f.Close()
		return func() {}, true
	}
}
