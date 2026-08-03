package store

import (
	"context"
	"fmt"
	"time"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// createPendingAnnotations is the DDL for the pending_annotations table (V21).
// Pending annotations are TUI-local drafts created before being committed to
// the backend via HTTP POST. No FK on session_id: the TUI may reference sessions
// not yet ingested into the local DB, and pending rows are ephemeral by design.
const createPendingAnnotations = `CREATE TABLE IF NOT EXISTS pending_annotations (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL,
    type_id     TEXT NOT NULL,
    value       TEXT NOT NULL,
    entry_index INTEGER,
    end_index   INTEGER,
    created_at  INTEGER NOT NULL
) STRICT`

// PendingAnnotationRecord is a single row from the pending_annotations table.
// Pending annotations are created by the TUI user and committed to the backend
// via HTTP POST (POST /api/v1/annotations) when the user presses 'c'. They are
// stored locally in SQLite until successfully flushed to the server.
type PendingAnnotationRecord struct {
	ID         string // UUID primary key
	SessionID  string // session identifier (not FK-constrained — may not exist in sessions table)
	TypeID     string // annotation type_id (e.g. "quality.session_outcome")
	Value      string // annotation value string
	EntryIndex *int   // nil for session-level; set for entry-level annotations
	EndIndex   *int   // nil unless range selection (half-open [EntryIndex, EndIndex))
	CreatedAt  int64  // Unix timestamp in seconds
}

// CreatePendingAnnotation inserts a new pending annotation into the local SQLite store.
// Returns an error describing what went wrong if the insert fails.
func (s *Store) CreatePendingAnnotation(ctx context.Context, rec PendingAnnotationRecord) error {
	if rec.ID == "" {
		return fmt.Errorf(
			"store.CreatePendingAnnotation (pending_annotations.go): id is required — " +
				"provide a non-empty UUID string for PendingAnnotationRecord.ID",
		)
	}
	if rec.SessionID == "" {
		return fmt.Errorf(
			"store.CreatePendingAnnotation (pending_annotations.go): session_id is required — " +
				"provide a non-empty session ID for PendingAnnotationRecord.SessionID",
		)
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = time.Now().Unix()
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf(
			"store.CreatePendingAnnotation (pending_annotations.go): failed to take DB connection — "+
				"context may be cancelled or pool exhausted: %w", err,
		)
	}
	defer s.pool.Put(conn)

	// Convert *int to nil (SQL NULL) or int (the actual value) for STRICT table compatibility.
	var entryIdx, endIdx any
	if rec.EntryIndex != nil {
		entryIdx = *rec.EntryIndex
	}
	if rec.EndIndex != nil {
		endIdx = *rec.EndIndex
	}

	return sqlitex.ExecuteTransient(conn,
		`INSERT INTO pending_annotations (id, session_id, type_id, value, entry_index, end_index, created_at)
         VALUES (?, ?, ?, ?, ?, ?, ?)`,
		&sqlitex.ExecOptions{
			Args: []any{rec.ID, rec.SessionID, rec.TypeID, rec.Value, entryIdx, endIdx, rec.CreatedAt},
		})
}

// ListPendingBySession returns all pending annotations for the given session,
// ordered by created_at ASC. Returns an empty slice (not nil) when none exist.
func (s *Store) ListPendingBySession(ctx context.Context, sessionID string) ([]PendingAnnotationRecord, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf(
			"store.ListPendingBySession (pending_annotations.go): failed to take DB connection: %w", err,
		)
	}
	defer s.pool.Put(conn)

	var recs []PendingAnnotationRecord
	err = sqlitex.ExecuteTransient(conn,
		`SELECT id, session_id, type_id, value, entry_index, end_index, created_at
         FROM pending_annotations
         WHERE session_id = ?
         ORDER BY created_at ASC`,
		&sqlitex.ExecOptions{
			Args: []any{sessionID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				rec := PendingAnnotationRecord{
					ID:        stmt.ColumnText(0),
					SessionID: stmt.ColumnText(1),
					TypeID:    stmt.ColumnText(2),
					Value:     stmt.ColumnText(3),
					CreatedAt: stmt.ColumnInt64(6),
				}
				if !stmt.ColumnIsNull(4) {
					v := int(stmt.ColumnInt64(4))
					rec.EntryIndex = &v
				}
				if !stmt.ColumnIsNull(5) {
					v := int(stmt.ColumnInt64(5))
					rec.EndIndex = &v
				}
				recs = append(recs, rec)
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf(
			"store.ListPendingBySession (pending_annotations.go): query failed for session_id=%q: %w",
			sessionID, err,
		)
	}
	if recs == nil {
		recs = []PendingAnnotationRecord{}
	}
	return recs, nil
}

// DeletePendingByID deletes a single pending annotation by its UUID primary key.
// Returns nil if the row did not exist (idempotent).
func (s *Store) DeletePendingByID(ctx context.Context, id string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf(
			"store.DeletePendingByID (pending_annotations.go): failed to take DB connection: %w", err,
		)
	}
	defer s.pool.Put(conn)

	return sqlitex.ExecuteTransient(conn,
		`DELETE FROM pending_annotations WHERE id = ?`,
		&sqlitex.ExecOptions{Args: []any{id}})
}

// DeleteAllPendingBySession deletes all pending annotations for the given session.
// Returns nil if no rows existed (idempotent).
func (s *Store) DeleteAllPendingBySession(ctx context.Context, sessionID string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf(
			"store.DeleteAllPendingBySession (pending_annotations.go): failed to take DB connection: %w", err,
		)
	}
	defer s.pool.Put(conn)

	return sqlitex.ExecuteTransient(conn,
		`DELETE FROM pending_annotations WHERE session_id = ?`,
		&sqlitex.ExecOptions{Args: []any{sessionID}})
}
