package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// SQL constants for the prune path
// ---------------------------------------------------------------------------

const (
	// sqlQueryPrunableSessions is the base query for finding prunable sessions.
	// Joins sessions + session_metrics + projects + host_slugs to gather display info.
	// session_metrics is LEFT JOINed because sessions may not have metrics yet.
	// V23+: host_slug is obtained via JOIN host_slugs on opaque_host_id; project display
	// name is canonical_cwd (or project_hash as fallback) since project_name no longer exists.
	// Column order: 0=session_id, 1=model_harness, 2=project_name, 3=git_remote, 4=start_ms, 5=turn_count, 6=host_slug, 7=project_hash, 8=project_path
	sqlQueryPrunableSessions = `SELECT
    s.session_id, s.model_harness,
    COALESCE(p.canonical_cwd, p.project_hash, ''), COALESCE(h.git_remote, ''),
    s.start_ms, COALESCE(m.turn_count, 0), h.host_slug,
    s.project_hash, COALESCE(NULLIF(s.git_worktree, ''), p.canonical_cwd, '')
FROM sessions s
LEFT JOIN session_metrics m ON s.session_id = m.session_id
LEFT JOIN projects p ON s.project_hash = p.project_hash
LEFT JOIN host_slugs h ON s.opaque_host_id = h.opaque_id`
)

// QueryPrunableSessions returns sessions matching the given filter.
func (s *Store) QueryPrunableSessions(ctx context.Context, filter ingest.PruneFilter) ([]ingest.PruneSessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.QueryPrunableSessions: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var conditions []string
	var args []any

	if !filter.All {
		if len(filter.SessionIDs) > 0 {
			placeholders := make([]string, len(filter.SessionIDs))
			for i, id := range filter.SessionIDs {
				placeholders[i] = "?"
				args = append(args, string(id))
			}
			conditions = append(conditions, "s.session_id IN ("+strings.Join(placeholders, ",")+")") //nolint:gocritic
		}
		if filter.ProjectHash != nil {
			conditions = append(conditions, "s.project_hash = ?")
			args = append(args, *filter.ProjectHash)
		}
		if filter.Harness != nil {
			conditions = append(conditions, "s.model_harness = ?")
			args = append(args, filter.Harness.String())
		}
		if filter.Before != nil {
			// Exclude sessions with start_ms=0 (unknown timestamp) from time-based filters.
			conditions = append(conditions, "s.start_ms > 0 AND s.start_ms < ?")
			args = append(args, *filter.Before)
		}
		if filter.After != nil {
			// Exclude sessions with start_ms=0 (unknown timestamp) from time-based filters.
			conditions = append(conditions, "s.start_ms > 0 AND s.start_ms >= ?")
			args = append(args, *filter.After)
		}
	}

	query := sqlQueryPrunableSessions
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY s.start_ms DESC"

	var rows []ingest.PruneSessionRow
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			// Provider is validated at write time by the sessions table CHECK constraint.
			// Column order: 0=session_id, 1=model_harness, 2=project_name, 3=git_remote,
			//               4=start_ms, 5=turn_count, 6=host_slug,
			//               7=project_hash, 8=project_path
			rows = append(rows, ingest.PruneSessionRow{
				SessionID:   ingest.SessionID(stmt.ColumnText(0)),
				Harness:     ingest.Harness(stmt.ColumnText(1)),
				ProjectName: stmt.ColumnText(2),
				GitRemote:   stmt.ColumnText(3),
				StartMs:     stmt.ColumnInt64(4),
				TurnCount:   stmt.ColumnInt(5),
				OutputPath:  stmt.ColumnText(6), // host_slug used to construct path later
				ProjectHash: stmt.ColumnText(7),
				ProjectPath: stmt.ColumnText(8),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store.QueryPrunableSessions: query: %w", err)
	}
	return rows, nil
}

// PruneSessions transactionally deletes all data for the given session IDs
// across all tables that reference session_id. Annotations targeting these
// sessions (or their entries) are also cleaned up.
func (s *Store) PruneSessions(ctx context.Context, sessionIDs []ingest.SessionID) (result ingest.PruneResult, err error) {
	if len(sessionIDs) == 0 {
		return ingest.PruneResult{}, nil
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return ingest.PruneResult{}, fmt.Errorf("store.PruneSessions: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	// Build IN clause for all queries.
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = string(id)
	}
	inClause := strings.Join(placeholders, ",")

	// Helper to execute a DELETE/UPDATE with the session ID args.
	execSQL := func(sql string) error {
		return sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args})
	}

	// Phase 1: Clean up annotations targeting these sessions or their entries.
	//
	// annotation_target_sessions and annotation_target_entries reference sessions
	// and session_entries respectively. The annotations table has a self-referencing
	// superseded_by FK (no CASCADE) and annotation_target_annotations references
	// annotations(id) (no CASCADE on target_annotation_id).
	//
	// Strategy:
	// 1. Collect annotation IDs targeting our sessions (direct, entry-level, and
	//    association-level)
	// 2. Clear superseded_by references to those annotations
	// 3. Delete meta-annotations targeting those annotations
	// 4. Delete the annotations themselves (CASCADE handles TPT children)

	// Collect annotation IDs from all target arms associated with these sessions.
	annQuery := fmt.Sprintf(
		`SELECT annotation_id FROM annotation_target_sessions WHERE session_id IN (%s)
			 UNION
			 SELECT annotation_id FROM annotation_target_entries WHERE session_id IN (%s)
			 UNION
			 SELECT ata.annotation_id
			 FROM annotation_target_associations ata
			 JOIN session_commit_associations sca ON sca.association_id = ata.association_id
			 WHERE sca.session_id IN (%s)`,
		inClause, inClause, inClause,
	)
	// Repeat the selected session IDs for each UNION arm.
	tripleArgs := make([]any, 0, len(args)*3)
	tripleArgs = append(tripleArgs, args...)
	tripleArgs = append(tripleArgs, args...)
	tripleArgs = append(tripleArgs, args...)

	var annIDs []string
	if err = sqlitex.ExecuteTransient(conn, annQuery, &sqlitex.ExecOptions{
		Args: tripleArgs,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			annIDs = append(annIDs, stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		return ingest.PruneResult{}, fmt.Errorf("store.PruneSessions: collect annotation IDs: %w", err)
	}

	if len(annIDs) > 0 {
		if err = deleteAnnotationClosure(conn, annIDs); err != nil {
			return ingest.PruneResult{}, fmt.Errorf("store.PruneSessions: %w", err)
		}
	}

	// Phase 2: Delete from tables with session_id in FK-safe order.
	// session_entries_ext has ON DELETE CASCADE from session_entries,
	// but we delete explicitly for robustness.
	deleteStmts := []struct {
		table string
		sql   string
	}{
		{"session_entries_ext", fmt.Sprintf("DELETE FROM session_entries_ext WHERE session_id IN (%s)", inClause)},
		{"session_entries", fmt.Sprintf("DELETE FROM session_entries WHERE session_id IN (%s)", inClause)},
		{"session_metrics", fmt.Sprintf("DELETE FROM session_metrics WHERE session_id IN (%s)", inClause)},
		{"session_commits", fmt.Sprintf("DELETE FROM session_commits WHERE session_id IN (%s)", inClause)},
		{"index_log", fmt.Sprintf("DELETE FROM index_log WHERE session_id IN (%s)", inClause)},
		{"pending_annotations", fmt.Sprintf("DELETE FROM pending_annotations WHERE session_id IN (%s)", inClause)},
		// sessions last — other tables have FKs pointing here.
		{"sessions", fmt.Sprintf("DELETE FROM sessions WHERE session_id IN (%s)", inClause)},
	}

	for _, d := range deleteStmts {
		if err = execSQL(d.sql); err != nil {
			return ingest.PruneResult{}, fmt.Errorf("store.PruneSessions: delete from %s: %w", d.table, err)
		}
	}

	// Use actual rows affected from the sessions DELETE (last statement).
	deleted := conn.Changes()
	return ingest.PruneResult{Deleted: deleted}, nil
}
