package store

import (
	"context"
	"errors"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
)

// sweepRetryAdvice names the repair for a sweep write that failed. A refused
// sweep deletes nothing and keeps its flag, so every class ends in a retry of
// harvest or reclaim.
func sweepRetryAdvice(err error) string { return writeRetryAdvice(err, "retry harvest or reclaim") }

// writeRetryAdvice names the repair for a store write that failed, by the
// class of its cause, and ends with retry, the step that repeats the write.
// Every class ends in that retry; what differs is what the user must change
// first. Lock contention and a full disk need opposite actions, so the advice
// never names one for the other, and a cancellation blames neither.
func writeRetryAdvice(err error, retry string) string {
	cancelled := "the operation was cancelled before it finished; nothing is wrong with the store, so " + retry
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return cancelled
	}
	switch sqlite.ErrCode(err).ToPrimary() {
	case sqlite.ResultBusy, sqlite.ResultLocked:
		return "another connection held the store's SQLite writer for longer than the " + defaults.SQLiteBusyTimeout.String() +
			" busy timeout, usually another peasant command writing to the same store; let it finish, then " + retry
	case sqlite.ResultFull:
		return "the disk that holds the store is full; free disk space, then " + retry
	case sqlite.ResultInterrupt:
		// A cancelled context reaches SQLite as an interrupt.
		return cancelled
	case sqlite.ResultIOErr:
		// The operating system can report a full disk as a write error, so an
		// I/O error names both causes.
		return "the operating system refused a read or write of the store's database file; the disk may be full or the file may not be writable, so free disk space and check the file's permissions, then " + retry
	default:
		return "check that the store's database file is readable and writable, then " + retry
	}
}
