//go:build !unix

package main

// tryLockListener is a no-op on platforms without flock(2): it always reports
// the lock as acquired so listen still runs. On such platforms the singleton
// guarantee falls back to the primer's heartbeat hint alone, as it did before
// the lock existed.
func tryLockListener(string) (func(), bool) {
	return func() {}, true
}
