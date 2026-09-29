// Package filelock provides a minimal exclusive-only advisory file lock with
// a deadline. It exists so test helpers (the storetest golden-template cache)
// can serialize a cross-process build without copying the session lock's
// retry loop or importing unix-only APIs outside a build-tagged file. The
// Windows port can extend this package with a shared mode for
// internal/store/session_lock.go or lift it into production; that reuse is the
// surface this package provides, and it is the only new shared package the
// cache adds.
package filelock

import (
	"errors"
	"fmt"
	"time"
)

// DefaultWait is the bounded wait AcquireWithDeadline grants when the caller
// passes a zero deadline: generous versus a migration pass, short enough that
// a wedged holder never stalls a suite forever.
const DefaultWait = 3 * time.Minute

// ReleaseFunc releases a lock acquired by Acquire. It closes the underlying
// lock-file description; on unix closing also releases the advisory lock, and
// the OS releases it on process exit even if ReleaseFunc never runs.
type ReleaseFunc func() error

// errDeadline reports an expired lock wait. It is a plain error (not
// context.DeadlineExceeded) so a deadline failure is distinguishable from a
// cancelled caller context at the call site.
var errDeadline = errors.New("filelock: deadline exceeded while waiting for the exclusive lock")

func errDeadlineExceeded(_ time.Time) error { return errDeadline }

// Acquire takes an exclusive advisory lock on the file at path, creating it
// if needed, and returns its release function. The wait is bounded by
// deadline: when the deadline passes before the lock is free, Acquire returns
// a non-nil error wrapping the deadline and takes no lock. A zero deadline
// means DefaultWait from now. The caller must call the release function when
// done; the lock is also released by the OS if the process exits first.
//
// Any Acquire error means "proceed without the lock" to the caller — the
// storetest cache builds privately in that case and never fails a test.
func Acquire(path string, deadline time.Time) (ReleaseFunc, error) {
	if deadline.IsZero() {
		deadline = time.Now().Add(DefaultWait)
	}
	release, err := acquire(path, deadline)
	if err != nil {
		return nil, fmt.Errorf("filelock: acquire exclusive lock on %s before %s: %w; no lock was taken; retry after the holder releases or proceed without the lock", path, deadline.Format(time.RFC3339), err)
	}
	return release, nil
}
