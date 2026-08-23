//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// detachAttr detaches the spawned bridge into its own session, so it survives
// the hook process (and the codex process tree) exiting.
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// terminateBridge SIGTERMs pid only when it still looks like one of our own
// codex-bridge processes, so a recycled pid can never take out an unrelated
// process. The bridge handles SIGTERM by exiting cleanly (quitting the listen
// loop and removing its pidfile).
func terminateBridge(pid int) {
	if !isOwnAgentChatProc(pid, "codex-bridge") {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
}

// probeThreadLock reports whether Codex currently holds its exclusive flock
// on the thread writer lock file. Rust's File::try_lock is flock(2) on Linux
// and macOS, so a non-blocking LOCK_EX attempt that succeeds means nobody has
// it; EWOULDBLOCK means the session is alive. The probe releases immediately
// and never creates the file.
func probeThreadLock(path string) lockState {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return lockMissing
		}
		return lockUnknown
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return lockFree
	case errors.Is(err, syscall.EWOULDBLOCK):
		return lockHeld
	default:
		return lockUnknown
	}
}
