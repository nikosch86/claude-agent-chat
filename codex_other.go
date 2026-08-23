//go:build !unix

package main

import "syscall"

// detachAttr: no session detach on platforms without setsid; the bridge is
// still spawned with released handles and usually outlives the hook.
func detachAttr() *syscall.SysProcAttr {
	return nil
}

// terminateBridge is a no-op without a safe way to verify the pid is ours; a
// leftover bridge exits on its own after bridgeMaxDeliverFails failed queues.
func terminateBridge(int) {}

// probeThreadLock cannot inspect flock state here; the bridge then relies on
// the SessionEnd hook alone.
func probeThreadLock(string) lockState { return lockUnknown }
