package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// The Release N+1 retirement (design §7.1): once every file-backed native
// session is converted and the search index is consolidated, the three
// file-backed projection tables go away. The guard below refuses the drop
// until all five preconditions hold, and the open path refuses a database
// that a newer release already retired.
//
// The schema chain stays at Release N here: the drop runs as an explicit,
// guard-checked call whose statements the next release's schema migration
// reuses. Registering the drop in the chain now would remove the tables the
// conversion seeds and the migration command still read.

// retirementDropStatements are the exact statements the next release's
// schema migration runs behind the guard: the three file-backed projection
// tables go; session_entry_full_content and its chunks stay (they still
// serve non-native sessions until the later move). That migration must run
// them on its own migration connection, in its own transaction, after
// evaluating the guard on that same connection (CheckRetirementReady over
// evaluateRetirementPreconditionsOnConn): taking a second pooled checkout
// inside a migration risks deadlock on a pool of one, and a check on
// another connection would not share the migration's transaction.
const retirementDropStatements = `
DROP TABLE IF EXISTS session_projection_generations;
DROP TABLE IF EXISTS session_projection_entries;
DROP TABLE IF EXISTS session_projection_content;
`

// RetirementNotReadyError refuses the retired-table drop while any Release
// N+1 precondition fails. It names each failing precondition, its blocking
// row count, why it blocks the upgrade, and the fix, so the refusal tells
// the operator what to run before upgrading.
type RetirementNotReadyError struct {
	// Failed carries the failing precondition evaluations, each with its
	// row count and its unblock detail.
	Failed []RetirementPreconditionStatus
}

// Error renders the refusal: one clause per failing precondition.
func (e *RetirementNotReadyError) Error() string {
	parts := make([]string, 0, len(e.Failed))
	for _, status := range e.Failed {
		parts = append(parts, fmt.Sprintf("%s (%d rows): %s", status.Name, status.RowCount, status.Detail))
	}
	return fmt.Sprintf("store: the retired projection tables cannot be dropped: %d retirement precondition(s) fail: %s",
		len(e.Failed), strings.Join(parts, "; "))
}

// RetirementFailed returns the failing evaluations of a guard check, in
// AllRetirementPreconditions order.
func RetirementFailed(statuses []RetirementPreconditionStatus) []RetirementPreconditionStatus {
	var failed []RetirementPreconditionStatus
	for _, status := range statuses {
		if !status.Passed {
			failed = append(failed, status)
		}
	}
	return failed
}

// CheckRetirementReady refuses with a RetirementNotReadyError unless every
// evaluation passed. The migration command's Phase 4 report and the
// next release's schema migration share this check, so the refusal text is
// one shape everywhere.
func CheckRetirementReady(statuses []RetirementPreconditionStatus) error {
	if failed := RetirementFailed(statuses); len(failed) > 0 {
		return &RetirementNotReadyError{Failed: failed}
	}
	return nil
}

// DropRetiredProjectionTables removes the three file-backed projection
// tables behind the five Release N+1 preconditions (design §7.1). This is
// the command and test path: it checks out one connection, evaluates the
// guard on it, and runs the drop on that same connection, so no concurrent
// writer can invalidate a precondition between the check and the change.
// While any precondition fails it changes nothing and returns a
// RetirementNotReadyError naming the failing condition, its row count, why
// it blocks the upgrade, and the fix (run peasant migrate with this
// release, then upgrade). When all hold it drops the tables in one
// transaction; session_entry_full_content and its chunks stay, still
// serving non-native sessions. The drop is idempotent: a retired database
// whose tables are already gone evaluates to all-passed and the IF EXISTS
// statements change nothing.
func (s *Store) DropRetiredProjectionTables(ctx context.Context) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to drop the retired projection tables: %w; the tables are unchanged", err)
	}
	defer s.pool.Put(conn)
	statuses, err := evaluateRetirementPreconditionsOnConn(ctx, conn)
	if err != nil {
		return err
	}
	if err := CheckRetirementReady(statuses); err != nil {
		return err
	}
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	if err := sqlitex.ExecuteScript(conn, retirementDropStatements, nil); err != nil {
		txnErr = fmt.Errorf("store: drop the retired projection tables: %w; the tables are unchanged", err)
		return txnErr
	}
	if err := ctx.Err(); err != nil {
		txnErr = fmt.Errorf("store: drop of the retired projection tables interrupted: %w; the transaction rolled back and the tables are unchanged", err)
		return txnErr
	}
	return nil
}

// refuseNewerSchemaOnConn refuses a database whose user_version exceeds
// this build's schema (design §6.1). The migrator only ever moves forward,
// so without this check an open would silently serve a retired (or
// otherwise newer) database it cannot understand. The refusal is
// actionable: it names the database versions on both sides and tells the
// operator to install the newer release. It runs before any migration or
// backfill, and it changes nothing.
func refuseNewerSchemaOnConn(conn *sqlite.Conn, dbPath string) error {
	var version int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			version = int(stmt.ColumnInt64(0))
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: read the schema version of %s: %w; nothing was changed", dbPath, err)
	}
	if current := CurrentSchemaVersion(); version > current {
		return fmt.Errorf("store: open %s: database schema is version %d, but this build understands only up to version %d (the database was upgraded by a newer Peasant); nothing was changed; install that version or newer to open it",
			dbPath, version, current)
	}
	return nil
}
