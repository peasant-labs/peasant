package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const sqlUpdateIndexState = `UPDATE sessions SET index_version = ?, indexed_at = ? WHERE session_id = ?`

// UpdateIndexState sets the index_version and indexed_at for a session after
// successful indexing.
func (s *Store) UpdateIndexState(ctx context.Context, sessionID ingest.SessionID, version int, indexedAtMs int64) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlUpdateIndexState, &sqlitex.ExecOptions{
		Args: []any{version, indexedAtMs, string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: update index state for %s: %w", sessionID, err)
	}
	return nil
}

const sqlListStaleIndexSessions = `SELECT session_id FROM sessions WHERE index_version < ?`

// ListStaleIndexSessions returns session IDs where index_version < currentVersion.
// Used by the post-FILTER auto-detect step to find sessions needing re-indexing.
func (s *Store) ListStaleIndexSessions(ctx context.Context, currentVersion int) ([]ingest.SessionID, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var sessions []ingest.SessionID
	if err := sqlitex.ExecuteTransient(conn, sqlListStaleIndexSessions, &sqlitex.ExecOptions{
		Args: []any{currentVersion},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			raw := stmt.ColumnText(0)
			sid, err := ingest.NewSessionID(raw)
			if err != nil {
				// Skip invalid session IDs (should not happen in practice).
				return nil
			}
			sessions = append(sessions, sid)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list stale index sessions: %w", err)
	}
	return sessions, nil
}
