//go:build windows

package store

import "golang.org/x/sys/windows"

const (
	lockShared    = 0
	lockExclusive = windows.LOCKFILE_EXCLUSIVE_LOCK
)

// flockAcquire attempts a non-blocking advisory lock on fd in the given mode
// (lockShared or lockExclusive) via LockFileEx. The zero-value Overlapped
// locks the same single byte range on every platform; peasant only ever
// needs whole-file mutual exclusion, never byte-range locking.
func flockAcquire(fd uintptr, mode int) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(windows.Handle(fd), uint32(mode)|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
}

func flockRelease(fd uintptr) error {
	ol := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(fd), 0, 1, 0, ol)
}

func flockWouldBlock(err error) bool {
	return err == windows.ERROR_LOCK_VIOLATION
}
