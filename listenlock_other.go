//go:build !unix

package main

// tryLockListener is a no-op on platforms without flock(2): listen always runs.
// On such platforms there is no singleton guarantee — concurrent listeners may
// coexist (duplicate notifications), but none is ever silenced.
func tryLockListener(string) func() {
	return func() {}
}

// tryLockListenerFor: same no-op for the codex bridge.
func tryLockListenerFor(string, string) func() {
	return func() {}
}

// procArgv cannot read another process's argv here.
func procArgv(int) []string { return nil }
