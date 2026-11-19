package store

import (
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
)

// sweepRetryAdvice names the repair for a sweep write that SQLite refused, by
// the class of its result code. A refused sweep deletes nothing and keeps its
// flag, so every class ends in a retry; what differs is what the user must
// change first. Lock contention and a full disk need opposite actions, so the
// advice never names one for the other.
func sweepRetryAdvice(err error) string {
	switch sqlite.ErrCode(err).ToPrimary() {
	case sqlite.ResultBusy, sqlite.ResultLocked:
		return "another connection held the store's SQLite writer for longer than the " + defaults.SQLiteBusyTimeout.String() +
			" busy timeout, usually another peasant command writing to the same store; let it finish, then retry harvest or reclaim"
	case sqlite.ResultFull:
		return "the disk that holds the store is full; free disk space, then retry harvest or reclaim"
	default:
		return "check that the store's database file is readable and writable, then retry harvest or reclaim"
	}
}
