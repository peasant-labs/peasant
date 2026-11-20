package store

import (
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// beginWrite begins the IMMEDIATE transaction every store write runs in, so
// the transaction holds SQLite's writer before its first statement.
//
// A DEFERRED transaction takes a read snapshot at its first read and needs a
// lock upgrade at its first write. SQLite refuses that upgrade with BUSY at
// once, without waiting on busy_timeout, when another connection holds the
// writer or has committed since the snapshot began. Writers outside this
// process's write lane (staging, the after-commit sweep, a second peasant
// process) make both cases ordinary, so a transaction that may write begins
// IMMEDIATE and waits on the bounded busy timeout instead. A transaction that
// only reads stays deferred; the store's transaction-mode guard holds the
// reviewed list of those.
//
// operation completes "take the SQLite writer to ..." in the error, which
// names the cause-specific repair.
func beginWrite(conn *sqlite.Conn, operation string) (endFn func(*error), err error) {
	endFn, err = sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return endFn, fmt.Errorf("store: take the SQLite writer to %s: %w; nothing was written; %s", operation, err, writeRetryAdvice(err, "retry"))
	}
	return endFn, nil
}
