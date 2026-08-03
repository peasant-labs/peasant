package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Compile-time guard: Store must implement IngestLogger.
var _ ingest.IngestLogger = (*Store)(nil)

const sqlInsertIngestLog = `INSERT INTO ingest_log (
    started_at, finished_at,
    sessions_new, sessions_updated, sessions_unchanged, sessions_error,
    indexed_count, computed_count,
    error_message, source_path
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// LogIngestRun inserts a single ingest_log row.
// Best-effort: callers should not fail the pipeline on error.
func (s *Store) LogIngestRun(ctx context.Context, entry ingest.IngestLogEntry) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlInsertIngestLog, &sqlitex.ExecOptions{
		Args: []any{
			entry.StartedAt,
			derefInt64(entry.FinishedAt),
			entry.SessionsNew,
			entry.SessionsUpdated,
			entry.SessionsUnchanged,
			entry.SessionsError,
			entry.IndexedCount,
			entry.ComputedCount,
			derefString(entry.ErrorMessage),
			derefString(entry.SourcePath),
		},
	}); err != nil {
		return fmt.Errorf("store: insert ingest_log: %w", err)
	}

	return nil
}
