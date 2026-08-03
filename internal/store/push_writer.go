package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

const sqlInsertPushLog = `INSERT INTO push_log (
    started_at, finished_at,
    village_url,
    sessions_pushed, sessions_updated, sessions_skipped, sessions_failed,
    error_message, user_id, username
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// InsertPushLog records a completed push run in the push_log table.
// Best-effort: callers should not fail the pipeline on error.
func (s *Store) InsertPushLog(ctx context.Context, entry ingest.PushLogEntry) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: insert push log take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlInsertPushLog, &sqlitex.ExecOptions{
		Args: []any{
			entry.StartedAt,
			derefInt64(entry.FinishedAt),
			entry.VillageURL,
			entry.SessionsPushed,
			entry.SessionsUpdated,
			entry.SessionsSkipped,
			entry.SessionsFailed,
			derefString(entry.ErrorMessage),
			entry.UserID,
			entry.Username,
		},
	}); err != nil {
		return fmt.Errorf("store: insert push_log: %w", err)
	}

	return nil
}
