// Package codexstate reads the native Codex state database read-only.
//
// The ingest package keeps every SQLite call site inside a fixed, bounded
// OpenCode executor so its private SQL inventory stays statically attributable.
// Native Codex current-rollout lookup is a distinct concern, so it lives here:
// the database is opened read-only, exactly one fixed SELECT runs, and the
// database is never written. Errors are sanitized at this boundary and never
// carry a raw private path.
package codexstate

import (
	"context"
	"fmt"
	"strings"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// PointerRecord is the native current-rollout pointer storage record for one
// stable thread: the authoritative current rollout path and the raw history
// mode the native database records beside it.
type PointerRecord struct {
	Pointer     string
	HistoryMode string
}

// PointerStore reads the native current-rollout pointer from the read-only
// native Codex state database. It is the production pointer authority; it is
// never a plaintext side document.
type PointerStore struct {
	path string
}

// NewPointerStore creates a read-only native state-database pointer store over
// the given database path.
func NewPointerStore(path string) *PointerStore {
	return &PointerStore{path: path}
}

// CurrentRollout queries the native threads table read-only for the one current
// rollout path of a stable thread. found is false when the native store holds
// no usable row for the thread, which is the only case that permits
// detached-file fallback.
func (s *PointerStore) CurrentRollout(ctx context.Context, stableThreadID string) (PointerRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return PointerRecord{}, false, fmt.Errorf("codexstate.PointerStore.CurrentRollout: the current-rollout read for thread %q was cancelled before the native state database could be queried; no source was read and no state changed; retry the harvest", stableThreadID)
	}
	conn, err := sqlite.OpenConn(s.path, sqlite.OpenReadOnly)
	if err != nil {
		return PointerRecord{}, false, fmt.Errorf("codexstate.PointerStore.CurrentRollout: the native Codex state database could not be opened read-only because %s; the caller falls back to detached-file authority and no older rollout is preferred", sanitizedSQLiteCause())
	}
	defer conn.Close()

	record := PointerRecord{}
	found := false
	queryErr := sqlitex.ExecuteTransient(conn, "SELECT rollout_path, history_mode FROM threads WHERE id = ? LIMIT 1", &sqlitex.ExecOptions{
		Args: []any{stableThreadID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnCount() > 0 {
				record.Pointer = strings.TrimSpace(stmt.ColumnText(0))
			}
			if stmt.ColumnCount() > 1 {
				record.HistoryMode = strings.TrimSpace(stmt.ColumnText(1))
			}
			found = record.Pointer != ""
			return nil
		},
	})
	if queryErr != nil {
		return PointerRecord{}, false, fmt.Errorf("codexstate.PointerStore.CurrentRollout: the native Codex state database could not be queried read-only for thread %q because %s; the caller falls back to detached-file authority and no older rollout is preferred", stableThreadID, sanitizedSQLiteCause())
	}
	return record, found, nil
}

// sanitizedSQLiteCause is the fixed sanitized reason for a native state
// database failure. It deliberately ignores the underlying error text, which
// can carry a raw private path.
func sanitizedSQLiteCause() string {
	return "the native state database could not be read"
}
