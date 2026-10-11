package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

const migrationV63 = `CREATE TABLE session_source_unavailability (
 session_id TEXT PRIMARY KEY REFERENCES sessions(session_id) ON DELETE CASCADE,
 reason TEXT NOT NULL CHECK (reason IN ('original-source-unavailable-no-usable-saved-copy'))
) STRICT;`

var _ ingest.SourceAvailabilityStore = (*Store)(nil)

// RecordSourceUnavailable returns true only for a newly recorded transition.
// Absence of a row means no unavailability has been observed, not proof that
// the original source is currently readable.
func (s *Store) RecordSourceUnavailable(ctx context.Context, sid ingest.SessionID, reason ingest.SourceUnavailabilityReason) (bool, error) {
	validated, err := ingest.NewSourceUnavailabilityReason(string(reason))
	if err != nil {
		return false, err
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, err
	}
	defer s.pool.Put(conn)
	changed := false
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO session_source_unavailability (session_id, reason) VALUES (?, ?) ON CONFLICT(session_id) DO NOTHING RETURNING session_id`, &sqlitex.ExecOptions{
		Args: []any{string(sid), string(validated)}, ResultFunc: func(*sqlite.Stmt) error { changed = true; return nil },
	})
	if err != nil {
		return false, fmt.Errorf("record unavailable source for session %s during pair repair: %w; prior content remains stored; restore database access and rerun harvest", sid, err)
	}
	return changed, nil
}

func (s *Store) ReadSourceUnavailability(ctx context.Context, sid ingest.SessionID) (*ingest.SourceUnavailabilityReason, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	var reason *ingest.SourceUnavailabilityReason
	err = sqlitex.ExecuteTransient(conn, `SELECT reason FROM session_source_unavailability WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			value, err := ingest.NewSourceUnavailabilityReason(stmt.ColumnText(0))
			if err == nil {
				reason = &value
			}
			return err
		},
	})
	return reason, err
}
