//go:build unix

package filelock

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// acquire mirrors internal/store/session_lock.go's non-blocking retry: the
// lock file is opened (created) once, then LOCK_EX|LOCK_NB is retried with a
// 2 ms backoff so an expired deadline is honored instead of blocking forever
// on a held exclusive lock.
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
	for {
		if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
			failed = false
			return func() error { return file.Close() }, nil
		} else if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
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
