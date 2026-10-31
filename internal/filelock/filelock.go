// Package filelock provides a minimal exclusive-only advisory file lock with
// a deadline. Two kinds of caller use it, and they treat a failure differently:
// test helpers (the storetest golden-template cache) serialize a cross-process
// build and proceed without the lock on any error, while the ingest run lock
// serializes writing harvests in production and fails closed. The Windows port
// can extend this package with a shared mode for internal/store/session_lock.go.
package filelock

import (
	"errors"
	"fmt"
	"time"
)

// DefaultWait is the bounded wait Acquire grants when the caller passes a zero
// deadline: generous versus a migration pass, short enough that a wedged
// holder never stalls a suite forever.
const DefaultWait = 3 * time.Minute

// ReleaseFunc releases a lock acquired by Acquire. It closes the underlying
// lock-file description; on unix closing also releases the advisory lock, and
// the OS releases it on process exit even if ReleaseFunc never runs.
type ReleaseFunc func() error

// errDeadline reports an expired lock wait. It is a plain error (not
// context.DeadlineExceeded) so a deadline failure is distinguishable from a
// cancelled caller context at the call site.
var errDeadline = errors.New("filelock: deadline exceeded while waiting for the exclusive lock")

// Acquire takes an exclusive advisory lock on the file at path, creating it
// if needed, and returns its release function. The wait is bounded by
// deadline: when the deadline passes before the lock is free, Acquire returns
// a non-nil error wrapping the deadline and takes no lock. A zero deadline
// means DefaultWait from now. The caller must call the release function when
// done; the lock is also released by the OS if the process exits first.
//
// An Acquire error means no lock was taken; the caller decides what follows.
// The storetest cache builds privately and never fails a test, and the ingest
// run lock refuses to run. IsDeadline tells a held lock from any other failure.
func Acquire(path string, deadline time.Time) (ReleaseFunc, error) {
	if deadline.IsZero() {
		deadline = time.Now().Add(DefaultWait)
	}
	release, err := acquire(path, deadline)
	if err != nil {
		return nil, fmt.Errorf("filelock: acquire exclusive lock on %s before %s: %w; no lock was taken; retry after the holder releases", path, deadline.Format(time.RFC3339), err)
	}
	return release, nil
}

// IsDeadline reports whether err is Acquire giving up because the lock stayed
// held until the deadline, as opposed to failing to open the lock file.
func IsDeadline(err error) bool { return errors.Is(err, errDeadline) }
