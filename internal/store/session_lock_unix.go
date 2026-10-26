//go:build unix

package store

import "golang.org/x/sys/unix"

const (
	lockShared    = unix.LOCK_SH
	lockExclusive = unix.LOCK_EX
)

// flockAcquire attempts a non-blocking advisory lock on fd in the given mode
// (lockShared or lockExclusive). It returns immediately with an error the
// caller recognizes via flockWouldBlock when the lock is already held.
func flockAcquire(fd uintptr, mode int) error {
	return unix.Flock(int(fd), mode|unix.LOCK_NB)
}

func flockRelease(fd uintptr) error {
	return unix.Flock(int(fd), unix.LOCK_UN)
}

func flockWouldBlock(err error) bool {
	return err == unix.EWOULDBLOCK || err == unix.EAGAIN
}
