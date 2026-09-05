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

// bridgeSignals lists what a running bridge listens for. SIGTERM and SIGINT
// mean "you are being evicted" (another listener or bridge took the nick, or
// someone killed us) and warrant a farewell into the session; SIGUSR1 is the
// quiet stop hook-stop and a same-thread handover use, where a farewell would
// only be noise (see bridgeFarewellFor).
func bridgeSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGUSR1}
}

func isQuietSignal(s os.Signal) bool { return s == syscall.SIGUSR1 }

// terminateBridge asks pid to stop quietly, but only when it still looks like
// one of our own codex-bridge processes, so a recycled pid can never take out
// an unrelated process. Called by hook-stop: the session is ending, so the
// bridge must not queue a farewell into it — Codex's queue is durable, and a
// farewell parked in an ended thread is replayed as the first turn when that
// thread is resumed, telling the resumed session its (freshly started) bridge
// is gone. A bridge built before SIGUSR1 handling existed just dies on it,
// which is the same outcome minus the pidfile cleanup hook-stop does anyway.
func terminateBridge(pid int) {
	if !isOwnAgentChatProc(pid, "codex-bridge") {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGUSR1)
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

// codexHomeWritable reports whether this process can write under Codex's home
// — which `codex queue` must, to persist into the queue database. Inside
// Codex's workspace-write sandbox the home directory is bind-mounted read-only
// (EROFS), so a bridge started from a sandboxed tool call could never deliver
// anything and would also die with the sandbox's PID namespace; refusing up
// front with a clear message beats a bridge that silently fails for a minute.
func codexHomeWritable() error {
	const wOK = 2
	err := syscall.Access(codexHome(), wOK)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EACCES) {
		return err
	}
	return nil
}
