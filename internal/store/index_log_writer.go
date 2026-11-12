package store

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// Compile-time guard: Store must implement IndexLogger.
var _ ingest.IndexLogger = (*Store)(nil)

const sqlInsertIndexLog = `INSERT INTO index_log (
    session_id, provider, outcome, index_version,
    entries_count, source_path, original_root, reason,
    started_at, finished_at, error_message, index_format_version,
    parse_ms, write_ms, bytes
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

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
			entry.IndexerVersion,
			entry.EntriesCount,
			derefString(entry.SourcePath),
			derefString(entry.OriginalRoot),
			derefString(entry.Reason),
			entry.StartedAt,
			derefInt64(entry.FinishedAt),
			derefString(entry.ErrorMessage),
			derefInt(entry.IndexVersion),
			derefDurationMs(entry.ParseDuration),
			derefDurationMs(entry.WriteDuration),
			derefInt64(entry.Bytes),
		},
	}); err != nil {
		return fmt.Errorf("store: insert index_log: %w", err)
	}

	return nil
}

// derefDurationMs binds a measured duration as whole milliseconds, or NULL
// when it was not measured.
func derefDurationMs(d *time.Duration) any {
	if d == nil {
		return nil
	}
	return max(d.Milliseconds(), 0)
}

// IndexLogTiming is the stored measurement of one index attempt. A nil field
// means not measured: the row predates the timing columns, or the attempt did
// not parse.
type IndexLogTiming struct {
	Outcome       ingest.IndexOutcome
	Reason        string
	ParseDuration *time.Duration
	WriteDuration *time.Duration
	Bytes         *int64
}

// ListIndexLogTimings reads the index attempts of one session, oldest first,
// with their stored timings.
func (s *Store) ListIndexLogTimings(ctx context.Context, sessionID ingest.SessionID) ([]IndexLogTiming, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read index_log timings for session %s: %w; check database access and retry", sessionID, err)
	}
	defer s.pool.Put(conn)
	var timings []IndexLogTiming
	err = sqlitex.ExecuteTransient(conn, `SELECT outcome, COALESCE(reason, ''), parse_ms, write_ms, bytes FROM index_log WHERE session_id=? ORDER BY id`, &sqlitex.ExecOptions{Args: []any{string(sessionID)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		timing := IndexLogTiming{Outcome: ingest.IndexOutcome(stmt.ColumnText(0)), Reason: stmt.ColumnText(1)}
		if !timing.Outcome.IsValid() {
			return fmt.Errorf("stored index outcome %q is not one this build defines", stmt.ColumnText(0))
		}
		if stmt.ColumnType(2) != sqlite.TypeNull {
			parse := time.Duration(stmt.ColumnInt64(2)) * time.Millisecond
			timing.ParseDuration = &parse
		}
		if stmt.ColumnType(3) != sqlite.TypeNull {
			write := time.Duration(stmt.ColumnInt64(3)) * time.Millisecond
			timing.WriteDuration = &write
		}
		if stmt.ColumnType(4) != sqlite.TypeNull {
			bytes := stmt.ColumnInt64(4)
			timing.Bytes = &bytes
		}
		timings = append(timings, timing)
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store: read index_log timings for session %s: %w; check the stored audit data and retry", sessionID, err)
	}
	return timings, nil
}
