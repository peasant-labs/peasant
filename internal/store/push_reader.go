package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// SQL constants for the push read path
// ---------------------------------------------------------------------------

// sqlPushSessionsBase is the base SELECT for push queries.
// Column order:
//
//	 0: session_id
//	 1: model_harness
//	 2: model_id
//	 3: host_slug      (from host_slugs JOIN — V23+: no longer a sessions column)
//	 4: project_hash
//	 5: project_name   (COALESCE(canonical_cwd, project_hash) — V23+: project_name removed)
//	 6: start_ms
//	 7: end_ms
//	 8: ingested_ms
//	 9: pushed_at      (nullable INTEGER)
//	10: source_path
//	11: source_format
//	12: git_branch     (nullable TEXT)
//	13: tool_version   (nullable TEXT)
//	14: turn_count
//	15: tool_calls
//	16: input_tokens
//	17: output_tokens
//	18: tokens_total   (computed: COALESCE(input)+COALESCE(output))
//	19: duration_ms    (CAST(duration_minutes * 60000 AS INTEGER))
//	20: git_remote     (from host_slugs JOIN; COALESCE to '' when unknown)
//	21: parent_id      (nullable FK; COALESCE to '' — empty for root sessions)
//
// Uses JOIN session_metrics (not LEFT JOIN) — sessions without metrics are
// excluded. This is intentional: sessions held back from push until
// peasant ingest --index completes their metrics.
// V23+: host_slug obtained via JOIN host_slugs on opaque_host_id.
const sqlPushSessionsBase = `SELECT
    s.session_id, s.model_harness, s.model_id, h.host_slug,
    s.project_hash, COALESCE(p.canonical_cwd, p.project_hash, ''), s.start_ms, s.end_ms,
    s.ingested_ms, s.pushed_at,
    s.source_path, s.source_format,
    s.git_branch, s.tool_version,
    m.turn_count, m.tool_calls,
    m.input_tokens, m.output_tokens,
    COALESCE(m.input_tokens, 0) + COALESCE(m.output_tokens, 0),
    CAST(m.duration_minutes * 60000 AS INTEGER),
    COALESCE(h.git_remote, ''),
    COALESCE(s.parent_id, '')
FROM sessions s
JOIN session_metrics m ON s.session_id = m.session_id
LEFT JOIN projects p ON s.project_hash = p.project_hash
LEFT JOIN host_slugs h ON s.opaque_host_id = h.opaque_id`

const (
	// sqlUnpushedSessions returns sessions with NULL pushed_at or re-ingested after push.
	sqlUnpushedSessions = sqlPushSessionsBase + `
WHERE s.pushed_at IS NULL OR s.ingested_ms > s.pushed_at
ORDER BY s.start_ms DESC`

	// sqlUnpushedSessionsByProvider adds a provider filter to the unpushed query.
	sqlUnpushedSessionsByProvider = sqlPushSessionsBase + `
WHERE (s.pushed_at IS NULL OR s.ingested_ms > s.pushed_at)
  AND s.model_harness = ?
ORDER BY s.start_ms DESC`

	// sqlAllPushableSessions returns all sessions regardless of pushed_at.
	// Used by --force flag to re-push everything.
	sqlAllPushableSessions = sqlPushSessionsBase + `
ORDER BY s.start_ms DESC`

	// sqlSessionsWithoutMetrics returns the sessions that have no session_metrics
	// row, with the project identity each carries so the caller can report only
	// the ones belonging to the repository it is pushing. These are sessions held
	// back from push (ingest must complete first).
	sqlSessionsWithoutMetrics = `SELECT s.session_id, s.project_hash
FROM sessions s
LEFT JOIN session_metrics m ON s.session_id = m.session_id
WHERE m.session_id IS NULL
ORDER BY s.start_ms DESC`

	// sqlRecordedProjects returns every project identity together with the
	// working directory recorded for it. Ordered by path so a caller's report is
	// stable run to run.
	sqlRecordedProjects = `SELECT project_hash, COALESCE(canonical_cwd, '')
FROM projects
ORDER BY canonical_cwd`
)

// ---------------------------------------------------------------------------
// Push read methods
// ---------------------------------------------------------------------------

// UnpushedSessions returns sessions where pushed_at is NULL or ingested_ms > pushed_at.
// Sessions without session_metrics are excluded (JOIN, not LEFT JOIN).
func (s *Store) UnpushedSessions(ctx context.Context) ([]ingest.PushSessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: unpushed sessions take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []ingest.PushSessionRow
	err = sqlitex.ExecuteTransient(conn, sqlUnpushedSessions, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanPushSessionRow(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: unpushed sessions query: %w", err)
	}
	return rows, nil
}

// UnpushedSessionsByProvider returns unpushed sessions filtered by model_harness.
func (s *Store) UnpushedSessionsByProvider(ctx context.Context, provider string) ([]ingest.PushSessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: unpushed sessions by provider take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []ingest.PushSessionRow
	err = sqlitex.ExecuteTransient(conn, sqlUnpushedSessionsByProvider, &sqlitex.ExecOptions{
		Args: []any{provider},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanPushSessionRow(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: unpushed sessions by provider query: %w", err)
	}
	return rows, nil
}

// AllPushableSessions returns all sessions regardless of pushed_at.
// Used by --force to re-push everything.
func (s *Store) AllPushableSessions(ctx context.Context) ([]ingest.PushSessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: all pushable sessions take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []ingest.PushSessionRow
	err = sqlitex.ExecuteTransient(conn, sqlAllPushableSessions, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanPushSessionRow(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: all pushable sessions query: %w", err)
	}
	return rows, nil
}

// SessionsWithoutMetrics returns the sessions with no session_metrics row, each
// with the project identity it carries. These are held back from push until
// peasant ingest --index completes.
func (s *Store) SessionsWithoutMetrics(ctx context.Context) ([]ingest.HeldSession, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: sessions without metrics take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var held []ingest.HeldSession
	err = sqlitex.ExecuteTransient(conn, sqlSessionsWithoutMetrics, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			held = append(held, ingest.HeldSession{
				SessionID:   stmt.ColumnText(0),
				ProjectHash: stmt.ColumnText(1),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: sessions without metrics query: %w", err)
	}
	return held, nil
}

// RecordedProjects returns every project ingestion has stamped sessions with,
// paired with the working directory recorded for it.
//
// Scoping a push to one repository needs this: a repository with no origin
// remote is identified by the directory each session was recorded in, so work
// done in a subdirectory carries an identity the repository root never derives.
// The rows are raw evidence — deciding which of them belong to a given
// repository is the caller's job.
func (s *Store) RecordedProjects(ctx context.Context) ([]ingest.RecordedProject, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: recorded projects take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var projects []ingest.RecordedProject
	err = sqlitex.ExecuteTransient(conn, sqlRecordedProjects, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			projects = append(projects, ingest.RecordedProject{
				Hash:         stmt.ColumnText(0),
				CanonicalCwd: stmt.ColumnText(1),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: recorded projects query: %w", err)
	}
	return projects, nil
}

// GetQualityMetrics returns the schema-level QualityMetrics for a session.
// Returns nil if no metrics have been computed for the session.
// The push mapper separately sanitizes TitleGenerated with the canonical title
// pipeline before applying runtime document rules to every other string field.
func (s *Store) GetQualityMetrics(ctx context.Context, sessionID ingest.SessionID) (*schema.QualityMetrics, error) {
	metrics, err := s.GetMetrics(ctx, sessionID)
	if err != nil || metrics == nil {
		return nil, err
	}
	quality := metrics.QualityMetrics
	return &quality, nil
}

// GetTitleContext returns the session harness and exact worktree when present,
// otherwise the project's canonical working directory.
func (s *Store) GetTitleContext(ctx context.Context, sessionID ingest.SessionID) (schema.Harness, string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", "", fmt.Errorf("store: title context take connection for session %s: %w", sessionID, err)
	}
	defer s.pool.Put(conn)

	var harness schema.Harness
	var projectPath string
	found := false
	err = sqlitex.ExecuteTransient(conn, `SELECT s.model_harness,
COALESCE(NULLIF(s.git_worktree, ''), p.canonical_cwd, '')
FROM sessions s LEFT JOIN projects p ON p.project_hash = s.project_hash
WHERE s.session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			harness = schema.Harness(stmt.ColumnText(0))
			projectPath = stmt.ColumnText(1)
			found = true
			return nil
		},
	})
	if err != nil {
		return "", "", fmt.Errorf("store: query title context for session %s during metric computation: %w", sessionID, err)
	}
	if !found {
		return "", "", fmt.Errorf("store: title context for session %s was not found during metric computation; the generated title was omitted; re-ingest the session and retry", sessionID)
	}
	return harness, projectPath, nil
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// scanPushSessionRow reads a PushSessionRow from the current statement position.
// Column order must match sqlPushSessionsBase.
// Nullable columns: pushed_at (col 9), git_branch (col 12), tool_version (col 13).
func scanPushSessionRow(stmt *sqlite.Stmt) ingest.PushSessionRow {
	row := ingest.PushSessionRow{
		SessionID:    stmt.ColumnText(0),
		ModelHarness: stmt.ColumnText(1),
		ParentID:     stmt.ColumnText(21), // COALESCE'd to '' — non-null; empty for root sessions
		ModelID:      stmt.ColumnText(2),
		HostSlug:     stmt.ColumnText(3),
		ProjectHash:  stmt.ColumnText(4),
		ProjectName:  stmt.ColumnText(5),
		StartMs:      stmt.ColumnInt64(6),
		EndMs:        stmt.ColumnInt64(7),
		IngestedMs:   stmt.ColumnInt64(8),
		// col 9: pushed_at — handled below (nullable)
		SourceFilePath: stmt.ColumnText(10),
		SourceFormat:   stmt.ColumnText(11),
		// col 12: git_branch — handled below (nullable)
		// col 13: tool_version — handled below (nullable)
		TurnCount:    stmt.ColumnInt(14),
		ToolCalls:    stmt.ColumnInt(15),
		InputTokens:  stmt.ColumnInt(16),
		OutputTokens: stmt.ColumnInt(17),
		TokensTotal:  stmt.ColumnInt(18),
		DurationMs:   stmt.ColumnInt64(19),
		GitRemote:    stmt.ColumnText(20), // COALESCE'd to '' — non-null
	}

	// Nullable: pushed_at (col 9)
	if stmt.ColumnType(9) != sqlite.TypeNull {
		v := stmt.ColumnInt64(9)
		row.PushedAt = &v
	}
	// Nullable: git_branch (col 12)
	if stmt.ColumnType(12) != sqlite.TypeNull {
		v := stmt.ColumnText(12)
		row.GitBranch = &v
	}
	// Nullable: tool_version (col 13)
	if stmt.ColumnType(13) != sqlite.TypeNull {
		v := stmt.ColumnText(13)
		row.ToolVersion = &v
	}

	return row
}
