package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// Compile-time guard: Store must implement IngestLogger.
var _ ingest.IngestLogger = (*Store)(nil)

const sqlInsertIngestLog = `INSERT INTO ingest_log (
    started_at, finished_at,
    sessions_new, sessions_updated, sessions_unchanged, sessions_error,
    indexed_count, computed_count,
    error_message, source_path
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// LogIngestRun writes one run's audit. The ingest_log row, its stage rows and
// the stage retention delete commit in one transaction: a fault in any of
// them leaves none. The outcome rows follow in bounded batches, each its own
// transaction, so a large run never holds the writer for long; a set of
// outcome rows may be partial and never certifies a complete run.
// Best-effort: callers should not fail the pipeline on error.
func (s *Store) LogIngestRun(ctx context.Context, entry ingest.IngestLogEntry) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	runID, err := insertIngestRun(conn, entry)
	if err != nil {
		return err
	}
	return s.writeIngestRunOutcomes(ctx, conn, runID, entry.Outcomes)
}

func insertIngestRun(conn *sqlite.Conn, entry ingest.IngestLogEntry) (runID int64, err error) {
	defer sqlitex.Transaction(conn)(&err)
	if err := sqlitex.Execute(conn, sqlInsertIngestLog, &sqlitex.ExecOptions{
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
		return 0, fmt.Errorf("store: insert ingest_log: %w", err)
	}
	// Read the parent identity on the same connection; another run may insert
	// concurrently, so MAX(id) is not a safe way to associate child rows.
	runID = conn.LastInsertRowID()
	if err := writeIngestRunStages(conn, runID, entry.Stages); err != nil {
		return 0, fmt.Errorf("%w; the run row and every stage row were rolled back", err)
	}
	return runID, nil
}
