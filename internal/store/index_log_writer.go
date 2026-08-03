package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Compile-time guard: Store must implement IndexLogger.
var _ ingest.IndexLogger = (*Store)(nil)

const sqlInsertIndexLog = `INSERT INTO index_log (
    session_id, provider, outcome, index_version,
    entries_count, source_path, original_root, reason,
    started_at, finished_at, error_message
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// LogIndexEntry inserts a single index_log row.
// Best-effort: callers should not fail the pipeline on error.
func (s *Store) LogIndexEntry(ctx context.Context, entry ingest.IndexLogEntry) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlInsertIndexLog, &sqlitex.ExecOptions{
		Args: []any{
			string(entry.SessionID),
			string(entry.Harness),
			entry.Outcome.String(),
			entry.IndexVersion,
			entry.EntriesCount,
			derefString(entry.SourcePath),
			derefString(entry.OriginalRoot),
			derefString(entry.Reason),
			entry.StartedAt,
			derefInt64(entry.FinishedAt),
			derefString(entry.ErrorMessage),
		},
	}); err != nil {
		return fmt.Errorf("store: insert index_log: %w", err)
	}

	return nil
}
