package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

var errSweepNeedsBatches = errors.New("sweep requires resumable bounded batches")

// sweepBoundedSession consolidates an ordinary sweep in one bounded commit.
// The total catalog + body + blob work must fit SweepRows. Larger sessions,
// filesystem cleanup, and corrupt deletes retain the resumable batch path.
func (s *Store) sweepBoundedSession(ctx context.Context, sid schema.SessionID, active string, limit int) (result SweepResult, applied bool, err error) {
	dirs, err := s.generationArtifacts.ListGenerationDirectories(ctx, sid)
	if err != nil {
		return result, false, err
	}
	if len(dirs) != 0 {
		return result, false, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return result, false, fmt.Errorf("store: take connection for bounded sweep of session %s: %w; the sweep flag remains set; retry harvest", sid, err)
	}
	defer s.pool.Put(conn)
	err = sweepBoundedOnConn(ctx, conn, sid, active, limit, &result)
	if errors.Is(err, errSweepNeedsBatches) {
		return SweepResult{}, false, nil
	}
	if err != nil {
		return SweepResult{}, true, fmt.Errorf("store: bounded sweep of session %s rolled back: %w; no rows were deleted and the sweep flag remains set; free disk space if full and retry harvest or reclaim", sid, err)
	}
	return result, true, nil
}

func sweepBoundedOnConn(ctx context.Context, conn *sqlite.Conn, sid schema.SessionID, active string, limit int, result *SweepResult) (err error) {
	end := sqlitex.Transaction(conn)
	defer end(&err)
	var work int64
	for _, table := range reclaimTableNames {
		statement := `SELECT COUNT(*) FROM (SELECT 1 FROM ` + table + ` WHERE session_id = ? AND generation_id IS NOT ? LIMIT ?)`
		if err := sqlitex.Execute(conn, statement, &sqlitex.ExecOptions{
			Args: []any{string(sid), active, int64(limit) + 1 - work},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				work += stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			return err
		}
		if work > int64(limit) {
			return errSweepNeedsBatches
		}
	}
	// These predicates include objects referenced only by the superseded
	// catalog rows that this transaction will remove, not active content.
	statements := []string{
		`SELECT COUNT(*) FROM (SELECT 1 FROM session_entry_bodies b WHERE b.session_id = ? AND NOT EXISTS (SELECT 1 FROM session_generation_entries m WHERE m.session_id = b.session_id AND m.body_digest = b.body_digest AND m.generation_id = ?) LIMIT ?)`,
		`SELECT COUNT(*) FROM (SELECT 1 FROM session_content c WHERE c.session_id = ? AND NOT EXISTS (SELECT 1 FROM session_generation_content d WHERE d.session_id = c.session_id AND d.digest = c.digest AND d.generation_id = ?) LIMIT ?)`,
	}
	for _, statement := range statements {
		if err := sqlitex.Execute(conn, statement, &sqlitex.ExecOptions{
			Args: []any{string(sid), active, int64(limit) + 1 - work},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				work += stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			return err
		}
		if work > int64(limit) {
			return errSweepNeedsBatches
		}
	}
	var needsRebuild bool
	if err := sqlitex.Execute(conn, `SELECT needs_rebuild FROM session_search_state WHERE id = 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			needsRebuild = stmt.ColumnInt64(0) != 0
			return nil
		},
	}); err != nil {
		return err
	}
	if needsRebuild {
		return errSweepNeedsBatches
	}
	if err := reportContentSweepSeam(contentSweepSeamFirstWrite); err != nil {
		return err
	}
	for _, table := range reclaimTableNames {
		if err := ctx.Err(); err != nil {
			return err
		}
		statement := `DELETE FROM ` + table + ` WHERE session_id = ? AND generation_id IS NOT ?`
		if err := sqlitex.Execute(conn, statement, &sqlitex.ExecOptions{Args: []any{string(sid), active}}); err != nil {
			return err
		}
		result.RowsDeleted += int64(conn.Changes())
	}
	var digests []string
	if err := sqlitex.Execute(conn, `SELECT b.body_digest FROM session_entry_bodies b WHERE b.session_id = ? AND NOT EXISTS (SELECT 1 FROM session_generation_entries m WHERE m.session_id = b.session_id AND m.body_digest = b.body_digest)`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			digests = append(digests, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		return err
	}
	for _, digest := range digests {
		if err := ctx.Err(); err != nil {
			return err
		}
		trusted, err := sweepVerifyBodyForDelete(conn, sid, digest)
		if err != nil {
			return err
		}
		if !trusted {
			return errSweepNeedsBatches
		}
		if err := sqlitex.Execute(conn, `DELETE FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{Args: []any{string(sid), digest}}); err != nil {
			return err
		}
		result.BodiesDeleted += int64(conn.Changes())
	}
	if err := sqlitex.Execute(conn, `DELETE FROM session_content WHERE session_id = ? AND NOT EXISTS (SELECT 1 FROM session_generation_content d WHERE d.session_id = session_content.session_id AND d.digest = session_content.digest)`, &sqlitex.ExecOptions{Args: []any{string(sid)}}); err != nil {
		return err
	}
	result.BlobsDeleted = int64(conn.Changes())
	return sqlitex.Execute(conn, `UPDATE sessions SET content_sweep_pending = 0 WHERE session_id = ? AND content_sweep_pending = 1`, &sqlitex.ExecOptions{Args: []any{string(sid)}})
}
