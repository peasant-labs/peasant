package store

import (
	"fmt"
	"sync/atomic"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// baselineApplications counts the fresh databases this process created from the
// committed baseline snapshot. It increments only after the snapshot has been
// applied and committed. Tests read it to prove the fresh path was actually
// taken; it is unexported and never surfaced to production callers.
var baselineApplications atomic.Int64

// baselineApplicationTotal reports how many times the baseline snapshot has
// been applied in this process.
func baselineApplicationTotal() int64 { return baselineApplications.Load() }

// errBaselineNotFresh marks the transactional re-check's disagreement. It is
// internal control flow, never returned to a caller.
var errBaselineNotFresh = fmt.Errorf("store: baseline re-check found the database is no longer fresh")

// databaseIsFresh reports whether conn points at a brand-new database: version
// zero, an empty schema catalog, and the same application_id acceptance rule
// the migration library applies. It is a read-only probe and is explicitly an
// optimization: the transactional re-check in applyBaselineIfStillFresh is what
// authorizes a write.
func databaseIsFresh(conn *sqlite.Conn) (bool, error) {
	version, err := readPragmaInt(conn, "PRAGMA user_version")
	if err != nil {
		return false, err
	}
	var objects int64
	if err := sqlitex.ExecuteTransient(conn, "SELECT COUNT(*) FROM sqlite_master", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			objects = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		return false, fmt.Errorf("store: count schema objects while probing freshness: %w; the probe reads the catalog only", err)
	}
	appID, err := readPragmaInt(conn, "PRAGMA application_id")
	if err != nil {
		return false, err
	}
	// The library accepts a database whose application_id already matches, or a
	// schemaless database with application_id zero. A fresh database must satisfy
	// the same rule before any baseline write.
	wantAppID := int64(dbSchema.AppID)
	accepted := appID == wantAppID || (appID == 0 && objects == 0)
	return version == 0 && objects == 0 && accepted, nil
}

// applyBaselineIfStillFresh applies the committed baseline snapshot to a fresh
// database inside one immediate transaction. It re-checks the full freshness
// predicate inside the write transaction (the read-only probe is only an
// optimization), disables foreign keys before BEGIN and restores them after the
// commit or rollback, and rolls back on every error. It reports false — with no
// writes — when the database stopped being fresh, so the caller falls through
// to the migration chain.
func applyBaselineIfStillFresh(conn *sqlite.Conn) (applied bool, err error) {
	if fkErr := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = OFF", nil); fkErr != nil {
		return false, fmt.Errorf("store: disable foreign keys before applying the baseline schema: %w; the snapshot needs foreign-key enforcement off while it creates its objects", fkErr)
	}
	defer func() {
		if fkErr := sqlitex.ExecuteTransient(conn, "PRAGMA foreign_keys = ON", nil); fkErr != nil && err == nil {
			err = fmt.Errorf("store: restore foreign keys after the baseline schema: %w; the connection must leave Open with foreign-key enforcement on", fkErr)
		}
	}()

	end, beginErr := sqlitex.ImmediateTransaction(conn)
	if beginErr != nil {
		return false, fmt.Errorf("store: begin an immediate transaction for the baseline schema: %w; another migrator may hold the database", beginErr)
	}
	txnErr := error(nil)

	fresh, probeErr := databaseIsFresh(conn)
	if probeErr != nil {
		txnErr = probeErr
		end(&txnErr)
		return false, fmt.Errorf("store: re-check whether the database is fresh: %w", probeErr)
	}
	if !fresh {
		txnErr = errBaselineNotFresh
		end(&txnErr)
		return false, nil
	}
	if applyErr := sqlitex.ExecuteScript(conn, baselineSchemaSQL, nil); applyErr != nil {
		txnErr = applyErr
		end(&txnErr)
		return false, fmt.Errorf("store: apply the baseline schema: %w; regenerate it with `go generate ./internal/store` if a migration changed the head schema", applyErr)
	}
	end(&txnErr)
	if txnErr != nil {
		return false, fmt.Errorf("store: commit the baseline schema: %w", txnErr)
	}
	baselineApplications.Add(1)
	return true, nil
}
