package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// This file is the origin half of the row-version watermark idiom this store
// already uses for re-indexing (index_state_writer.go): one query listing the
// rows a newer rule has not judged yet, and one update that records a verdict
// and moves the row past the line. The two files are deliberately shaped the
// same, because they answer the same question about different work.

// The store is the resolve pass's persistence. Declaring it here means a rename
// or a signature change on either side is a build failure rather than a silently
// skipped pass at the pipeline's type assertion.
var _ ingest.OriginResolverStore = (*Store)(nil)

const sqlListStaleOriginSessions = `SELECT session_id, COALESCE(parent_id, ''), model_harness, source_path, session_origin, origin_version
FROM sessions WHERE origin_version < ?`

// ListStaleOriginSessions returns every session whose origin_version is below
// currentVersion, together with the facts the resolver judges it on. It is the
// origin twin of ListStaleIndexSessions.
//
// The rows come back with their stored verdict AND the version that verdict was
// written at, so a caller can tell a verdict it would only rewrite unchanged
// from one it must actually persist, and can leave a retryable row's watermark
// exactly where it found it. A row is
// skipped only when its session_id is unreadable as an identifier, which no
// row this store wrote can be.
func (s *Store) ListStaleOriginSessions(ctx context.Context, currentVersion int) ([]ingest.StoredOriginRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []ingest.StoredOriginRow
	if err := sqlitex.ExecuteTransient(conn, sqlListStaleOriginSessions, &sqlitex.ExecOptions{
		Args: []any{currentVersion},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sid, err := ingest.NewSessionID(stmt.ColumnText(0))
			if err != nil {
				// Skip an identifier this build cannot read rather than failing
				// the whole pass: one corrupt row must not stop every other row
				// from getting a verdict.
				return nil
			}
			rows = append(rows, ingest.StoredOriginRow{
				SessionID:     sid,
				ParentID:      stmt.ColumnText(1),
				Harness:       ingest.Harness(stmt.ColumnText(2)),
				SourcePath:    ingest.ResolvedPath(stmt.ColumnText(3)),
				StoredOrigin:  stmt.ColumnText(4),
				OriginVersion: int(stmt.ColumnInt64(5)),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list sessions below origin version %d: %w", currentVersion, err)
	}
	return rows, nil
}

const sqlUpdateOriginState = `UPDATE sessions SET session_origin = ?, origin_version = ? WHERE session_id = ?`

// UpdateOriginState records a session's origin verdict and the rule version that
// produced it. It is the origin twin of UpdateIndexState.
//
// Both columns move in ONE statement, which is what makes the pass crash-safe:
// a row can never be found above the version line carrying a verdict that was
// not committed with it, so an interrupted pass resumes at the first row it did
// not reach instead of restarting on half-written state.
//
// A version BELOW the current rule version is how the caller keeps a row
// retryable while still giving it a verdict; the column carries no CHECK for
// exactly that reason.
func (s *Store) UpdateOriginState(ctx context.Context, sessionID ingest.SessionID, origin string, version int) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlUpdateOriginState, &sqlitex.ExecOptions{
		Args: []any{origin, version, string(sessionID)},
	}); err != nil {
		return fmt.Errorf("store: update origin state for %s to %q at version %d: %w", sessionID, origin, version, err)
	}
	return nil
}
