//go:build unix

package main

import "syscall"

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
