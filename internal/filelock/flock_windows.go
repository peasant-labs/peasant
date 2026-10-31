//go:build windows

package filelock

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// acquire takes an exclusive lock with LockFileEx and LOCKFILE_FAIL_IMMEDIATELY
// so the wait stays non-blocking and honors the deadline, mirroring the unix
// backend's retry loop. The lock covers the whole file and is released with
// UnlockFileEx; the OS also releases it if the process exits first.
func acquire(path string, deadline time.Time) (ReleaseFunc, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = file.Close()
		}
	}()
	handle := windows.Handle(file.Fd())
	var overlapped windows.Overlapped
	for {
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			failed = false
			return func() error {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
				return file.Close()
			}, nil
		}
		// Only a conflicting lock means "still held"; any other failure
		// (a bad handle, an I/O error) is returned instead of retried.
		if err != windows.ERROR_LOCK_VIOLATION && err != windows.ERROR_IO_PENDING {
			return nil, err
		}
		if wait := time.Until(deadline); wait <= 0 {
			return nil, errDeadline
		} else if wait < 2*time.Millisecond {
			time.Sleep(wait)
		} else {
			time.Sleep(2 * time.Millisecond)
		}
	}
}
