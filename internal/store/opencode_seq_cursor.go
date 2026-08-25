package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// The local store records the OpenCode change cursor, so a rescan re-ingests a
// session whose newest event sequence moved past the last ingested value even
// when no time column changed.
var _ ingest.OpenCodeSeqCursorStore = (*Store)(nil)

const (
	sqlSelectOpenCodeSeqCursor = `SELECT session_id, last_seq FROM opencode_session_seq_cursor WHERE session_id = ?`
	sqlUpsertOpenCodeSeqCursor = `INSERT OR REPLACE INTO opencode_session_seq_cursor (session_id, last_seq) VALUES (?, ?)`
)

// BulkLookupOpenCodeSeqCursors returns the last ingested event sequence for each
// given session that has a stored cursor. A session with no cursor is omitted,
// so its first sighting is treated as new rather than unchanged.
func (s *Store) BulkLookupOpenCodeSeqCursors(ctx context.Context, sessionIDs []ingest.SessionID) (map[ingest.SessionID]int64, error) {
	cursors := make(map[ingest.SessionID]int64, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return cursors, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.BulkLookupOpenCodeSeqCursors: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	for _, sessionID := range sessionIDs {
		err := sqlitex.ExecuteTransient(conn, sqlSelectOpenCodeSeqCursor, &sqlitex.ExecOptions{
			Args: []any{string(sessionID)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				cursors[ingest.SessionID(stmt.ColumnText(0))] = stmt.ColumnInt64(1)
				return nil
			},
		})
		if err != nil {
			return nil, fmt.Errorf("store.BulkLookupOpenCodeSeqCursors: read cursor for %q: %w", sessionID, err)
		}
	}
	return cursors, nil
}

// UpsertOpenCodeSeqCursor records the last ingested event sequence for a
// session, replacing any earlier value. A negative sequence is rejected by the
// table's CHECK constraint rather than stored.
func (s *Store) UpsertOpenCodeSeqCursor(ctx context.Context, sessionID ingest.SessionID, seq int64) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store.UpsertOpenCodeSeqCursor: take connection: %w", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, sqlUpsertOpenCodeSeqCursor, &sqlitex.ExecOptions{
		Args: []any{string(sessionID), seq},
	}); err != nil {
		return fmt.Errorf("store.UpsertOpenCodeSeqCursor: write cursor for %q: %w", sessionID, err)
	}
	return nil
}
