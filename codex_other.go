//go:build !unix

package main

import (
	"os"
	"syscall"
)

// detachAttr: no session detach on platforms without setsid; the bridge is
// still spawned with released handles and usually outlives the hook.
func detachAttr() *syscall.SysProcAttr {
	return nil
}

// bridgeSignals: no SIGUSR1 here, so every stop counts as an eviction.
func bridgeSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }

func isQuietSignal(os.Signal) bool { return false }

// terminateBridge is a no-op without a safe way to verify the pid is ours; a
// leftover bridge exits on its own once `codex queue` has failed for
// bridgeDeliverGrace.
func terminateBridge(int) {}

// probeThreadLock cannot inspect flock state here; the bridge then relies on
// the SessionEnd hook alone, and an eviction always queues the farewell.
func probeThreadLock(string) lockState { return lockUnknown }

// codexHomeWritable: no sandbox detection here.
func codexHomeWritable() error { return nil }
