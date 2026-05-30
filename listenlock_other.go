//go:build !unix

package main

// tryLockListener is a no-op on platforms without flock(2): listen always runs.
// On such platforms there is no singleton guarantee — concurrent listeners may
// coexist (duplicate notifications), but none is ever silenced.
func tryLockListener(string) func() {
	return func() {}
}
