//go:build unix

package storetest

import (
	"errors"
	"syscall"
	"time"
)

// processAlive reports whether pid names a process that may still be running.
// Kill(pid, 0) performs no signalling: a nil error means the process exists,
// EPERM means it exists but belongs to another user, and ESRCH means it is
// definitely dead. Any other outcome is treated as alive — the dead-owner
// sweep must never reap a live owner's directory.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// reapableOwner is the conservative reap rule: only a provably dead owner
// past the age floor. A live owner is never reapable at any age; a dead
// owner younger than the floor is kept against PID reuse and slow starts.
func reapableOwner(pid int, modTime, now time.Time) bool {
	return !processAlive(pid) && now.Sub(modTime) >= ownerAgeFloor
}
