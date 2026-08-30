package store

import (
	"context"
	"fmt"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	sqlGetCurrentSessionEntriesHash = `SELECT session_entries_hash FROM sessions WHERE session_id = ?`
	sqlGetAnnotationRunState        = `SELECT session_id, session_entries_hash, compute_version, classifier_version, annotated_at FROM annotation_run_state WHERE session_id = ?`
	sqlGetAnnotationRunInputs       = `SELECT
    s.session_id,
    s.session_entries_hash,
    sm.compute_version,
    ars.session_id,
    ars.session_entries_hash,
    ars.compute_version,
    ars.classifier_version,
    ars.annotated_at
FROM sessions s
LEFT JOIN session_metrics sm ON sm.session_id = s.session_id
LEFT JOIN annotation_run_state ars ON ars.session_id = s.session_id
WHERE s.session_id = ?`
	sqlSaveAnnotationRunState = `INSERT INTO annotation_run_state (session_id, session_entries_hash, compute_version, classifier_version, annotated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(session_id) DO UPDATE SET
    session_entries_hash = excluded.session_entries_hash,
    compute_version = excluded.compute_version,
    classifier_version = excluded.classifier_version,
    annotated_at = excluded.annotated_at`
)

func (s *Store) GetAnnotationRunInputs(ctx context.Context, sessionID ingest.SessionID) (*ingest.AnnotationRunInputs, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for annotation run input lookup: %w", err)
	}
	defer s.pool.Put(conn)

	var inputs *ingest.AnnotationRunInputs
	err = sqlitex.ExecuteTransient(conn, sqlGetAnnotationRunInputs, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			inputs = &ingest.AnnotationRunInputs{
				SessionID: ingest.SessionID(stmt.ColumnText(0)),
			}
			if stmt.ColumnType(1) != sqlite.TypeNull {
				inputs.SessionEntriesHash = stmt.ColumnText(1)
				inputs.HasSessionEntriesHash = inputs.SessionEntriesHash != ""
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				inputs.ComputeVersion = stmt.ColumnInt(2)
				inputs.HasComputeVersion = true
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				inputs.State = &ingest.AnnotationRunState{
					SessionID:          ingest.SessionID(stmt.ColumnText(3)),
					SessionEntriesHash: stmt.ColumnText(4),
					ComputeVersion:     stmt.ColumnInt(5),
					ClassifierVersion:  stmt.ColumnInt(6),
					AnnotatedAt:        time.UnixMilli(stmt.ColumnInt64(7)),
				}
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get annotation run inputs for %s: %w", sessionID, err)
	}
	return inputs, nil
}

func (s *Store) GetCurrentSessionEntriesHash(ctx context.Context, sessionID ingest.SessionID) (string, bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", false, fmt.Errorf("store: take connection for session_entries_hash lookup: %w", err)
	}
	defer s.pool.Put(conn)

	var hash string
	var ok bool
	err = sqlitex.ExecuteTransient(conn, sqlGetCurrentSessionEntriesHash, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) == sqlite.TypeNull {
				return nil
			}
			hash = stmt.ColumnText(0)
			ok = hash != ""
			return nil
		},
	})
	if err != nil {
		return "", false, fmt.Errorf("store: get session_entries_hash for %s: %w", sessionID, err)
	}
	return hash, ok, nil
}

func (s *Store) GetAnnotationRunState(ctx context.Context, sessionID ingest.SessionID) (*ingest.AnnotationRunState, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection for annotation_run_state lookup: %w", err)
	}
	defer s.pool.Put(conn)

	var state *ingest.AnnotationRunState
	err = sqlitex.ExecuteTransient(conn, sqlGetAnnotationRunState, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			state = &ingest.AnnotationRunState{
				SessionID:          ingest.SessionID(stmt.ColumnText(0)),
				SessionEntriesHash: stmt.ColumnText(1),
				ComputeVersion:     stmt.ColumnInt(2),
				ClassifierVersion:  stmt.ColumnInt(3),
				AnnotatedAt:        time.UnixMilli(stmt.ColumnInt64(4)),
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get annotation_run_state for %s: %w", sessionID, err)
	}
	return state, nil
}

func (s *Store) SaveAnnotationRunState(ctx context.Context, state ingest.AnnotationRunState) error {
	s.annotationWriteMu.Lock()
	defer s.annotationWriteMu.Unlock()

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection for annotation_run_state save: %w", err)
	}
	defer s.pool.Put(conn)

	if err := saveAnnotationRunStateOnConn(conn, state); err != nil {
		return fmt.Errorf("store: save annotation_run_state for %s: %w", state.SessionID, err)
	}
	return nil
}

func saveAnnotationRunStateOnConn(conn *sqlite.Conn, state ingest.AnnotationRunState) error {
	annotatedAt := state.AnnotatedAt
	if annotatedAt.IsZero() {
		annotatedAt = time.Now()
	}
	if err := sqlitex.ExecuteTransient(conn, sqlSaveAnnotationRunState, &sqlitex.ExecOptions{
		Args: []any{string(state.SessionID), state.SessionEntriesHash, state.ComputeVersion, state.ClassifierVersion, annotatedAt.UnixMilli()},
	}); err != nil {
		return fmt.Errorf("execute annotation_run_state upsert: %w", err)
	}
	return nil
}
