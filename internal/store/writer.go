package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// deriveOpaqueHostID computes HMAC-SHA256(salt, hashInput) for a host_slugs row.
// hashInput = NormalizeRemoteURL(gitRemote) when available, else hostSlug.
// Returns (opaqueID, canonicalRemote, error).
func (s *Store) deriveOpaqueHostID(gitRemote *string, hostSlug string) (opaqueID string, canonicalRemote string, err error) {
	var hashInput string
	if gitRemote != nil && *gitRemote != "" {
		canonical, normErr := ingest.NormalizeRemoteURL(*gitRemote)
		if normErr == nil {
			canonicalRemote = canonical
			hashInput = canonical
		} else {
			// Normalize failed: use host_slug as hash input.
			hashInput = hostSlug
		}
	} else {
		hashInput = hostSlug
	}
	ph, hashErr := s.salt.Hash(hashInput)
	if hashErr != nil {
		return "", "", fmt.Errorf(
			"store.deriveOpaqueHostID: hash %q: %w — "+
				"what: HMAC-SHA256 computation failed; "+
				"why: this should never happen for valid string input; "+
				"this is a bug — please report it",
			hashInput, hashErr,
		)
	}
	return string(ph), canonicalRemote, nil
}

// SQL statements for the write path. Defined as package-level constants for readability.
const (
	// sqlInsertProject upserts a project row (V23+: canonical_cwd, canonical_remote).
	// project_hash is HMAC-SHA256(salt, canonical_remote) — computed by DeriveProjectIdentifiers.
	//
	// On conflict, canonical_cwd is updated to the shorter path — the repo root is
	// always shorter than worktree subdirectories within the same project, so this
	// picks the most canonical (closest-to-root) working directory.
	// canonical_remote is updated if the new value is non-null (backfill).
	sqlInsertProject = `INSERT INTO projects (project_hash, canonical_cwd, canonical_remote)
VALUES (?, ?, ?)
ON CONFLICT(project_hash) DO UPDATE SET
  canonical_cwd = CASE
    WHEN excluded.canonical_cwd IS NOT NULL
     AND (projects.canonical_cwd IS NULL OR LENGTH(excluded.canonical_cwd) < LENGTH(projects.canonical_cwd))
    THEN excluded.canonical_cwd
    ELSE projects.canonical_cwd
  END,
  canonical_remote = COALESCE(excluded.canonical_remote, projects.canonical_remote)`

	// sqlInsertHostSlug inserts a host_slugs row (V23+: opaque_id PK).
	// opaque_id is HMAC-SHA256(salt, canonical_remote) — computed by caller.
	sqlInsertHostSlug = `INSERT OR IGNORE INTO host_slugs (opaque_id, host_slug, git_remote, canonical_remote)
VALUES (?, ?, ?, ?)`

	// sqlInsertSession inserts a session row (V23+: opaque_host_id replaces host_slug FK).
	sqlInsertSession = `INSERT OR REPLACE INTO sessions (
    session_id, parent_id, model_harness, model_id, opaque_host_id, project_hash,
    start_ms, end_ms, ingested_ms, source_path, source_format,
    schema_version, git_branch, git_worktree, git_tracking, tool_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlSessionExists = `SELECT 1 FROM sessions WHERE session_id = ?`

	sqlInsertSessionMetrics = `INSERT OR REPLACE INTO session_metrics (
    session_id, turn_count, subagent_count,
    input_tokens, output_tokens, tool_calls, duration_minutes
) VALUES (?, ?, ?, ?, ?, ?, ?)`

	sqlUpsertDailySummary = `INSERT OR REPLACE INTO daily_summary (
    date_utc, session_count, tokens_in, tokens_out, tokens_total,
    avg_duration_ms, avg_turns, tool_call_count,
    total_cost_usd, avg_cost_per_session_usd, acceptance_rate
)
SELECT
    date(s.start_ms / 1000, 'unixepoch') AS date_utc,
    COUNT(*)                              AS session_count,
    COALESCE(SUM(m.input_tokens), 0)     AS tokens_in,
    COALESCE(SUM(m.output_tokens), 0)    AS tokens_out,
    COALESCE(SUM(m.input_tokens + m.output_tokens), 0) AS tokens_total,
    COALESCE(AVG(m.duration_minutes) * 60000.0, 0) AS avg_duration_ms,
    COALESCE(AVG(m.turn_count), 0)       AS avg_turns,
    COALESCE(SUM(m.tool_calls), 0)       AS tool_call_count,
    SUM(m.cost_total_usd)                AS total_cost_usd,
    SUM(m.cost_total_usd) / NULLIF(COUNT(*), 0) AS avg_cost_per_session_usd,
    CAST(SUM(CASE WHEN m.outcome = 'resolved' THEN 1 ELSE 0 END) AS REAL)
      / NULLIF(COUNT(*), 0)              AS acceptance_rate
FROM sessions s
JOIN session_metrics m ON s.session_id = m.session_id
WHERE date(s.start_ms / 1000, 'unixepoch') = ?
GROUP BY date_utc`

	sqlDeleteDailySummaryHarness = `DELETE FROM daily_summary_harness WHERE date_utc = ?`

	sqlInsertDailySummaryHarness = `INSERT INTO daily_summary_harness (
    date_utc, model_harness, session_count, tokens_in, tokens_out, tokens_total,
    avg_duration_ms, avg_turns, tool_call_count
)
SELECT
    date(s.start_ms / 1000, 'unixepoch') AS date_utc,
    s.model_harness,
    COUNT(*)                              AS session_count,
    COALESCE(SUM(m.input_tokens), 0),
    COALESCE(SUM(m.output_tokens), 0),
    COALESCE(SUM(m.input_tokens + m.output_tokens), 0),
    COALESCE(AVG(m.duration_minutes) * 60000.0, 0),
    COALESCE(AVG(m.turn_count), 0),
    COALESCE(SUM(m.tool_calls), 0)
FROM sessions s
JOIN session_metrics m ON s.session_id = m.session_id
WHERE date(s.start_ms / 1000, 'unixepoch') = ?
GROUP BY date_utc, s.model_harness`

	sqlDeleteDailySummaryByProject = `DELETE FROM daily_summary_by_project WHERE date_utc = ?`

	// sqlInsertDailySummaryByProject recomputes per-project daily aggregates.
	// V23+: project_path removed (projects table no longer has project_path column).
	// project_path column in daily_summary_by_project is preserved for display purposes
	// but set to canonical_cwd from the projects table.
	sqlInsertDailySummaryByProject = `INSERT INTO daily_summary_by_project (
    date_utc, project_hash, project_path, session_count, total_tokens,
    total_tool_calls, total_duration_minutes, resolved_count, failed_count,
    partial_count, avg_spec_quality, acceptance_rate, total_cost_usd
)
SELECT
    date(s.start_ms / 1000, 'unixepoch') AS date_utc,
    s.project_hash,
    MAX(p.canonical_cwd)                                     AS project_path,
    COUNT(*)                                                  AS session_count,
    COALESCE(SUM(m.input_tokens + m.output_tokens), 0)       AS total_tokens,
    COALESCE(SUM(m.tool_calls), 0)                           AS total_tool_calls,
    COALESCE(SUM(m.duration_minutes), 0)                     AS total_duration_minutes,
    SUM(CASE WHEN m.outcome = 'resolved' THEN 1 ELSE 0 END) AS resolved_count,
    SUM(CASE WHEN m.outcome = 'failed'   THEN 1 ELSE 0 END) AS failed_count,
    SUM(CASE WHEN m.outcome = 'partial'  THEN 1 ELSE 0 END) AS partial_count,
    AVG(m.spec_quality_score)                                AS avg_spec_quality,
    CAST(SUM(CASE WHEN m.outcome = 'resolved' THEN 1 ELSE 0 END) AS REAL)
      / NULLIF(COUNT(*), 0)                                  AS acceptance_rate,
    SUM(m.cost_total_usd)                                    AS total_cost_usd
FROM sessions s
JOIN session_metrics m ON s.session_id = m.session_id
LEFT JOIN projects p ON s.project_hash = p.project_hash
WHERE date(s.start_ms / 1000, 'unixepoch') = ?
GROUP BY date_utc, s.project_hash`
)

// InsertSessions inserts a batch of sessions + metrics within a single transaction.
// Daily summary recomputation is decoupled; call UpdateDailySummary separately.
//
// Transaction structure:
//  1. INSERT OR IGNORE INTO projects (deduplicate project_hash)
//  2. Compute opaque_host_id = HMAC-SHA256(salt, canonical_remote or host_slug)
//  3. INSERT OR IGNORE INTO host_slugs with opaque_id PK
//  4. INSERT OR REPLACE INTO sessions with opaque_host_id FK
//  5. INSERT OR REPLACE INTO session_metrics (upsert)
//  6. COMMIT
//
// Entries with nil Metadata are silently skipped — this occurs when extraction
// fails for a session but the pipeline continues with partial results.
func (s *Store) InsertSessions(ctx context.Context, entries []ingest.StoreEntry) (err error) {
	if len(entries) == 0 {
		return nil
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	// Topological sort: parents before children to satisfy the FK constraint
	// sessions.parent_id REFERENCES sessions(session_id). When processing
	// thousands of sessions in one transaction, a child may appear before its
	// parent in the input slice, causing a FOREIGN KEY constraint failure.
	// Partitioning into roots-first is sufficient because the schema only has
	// one level of parent→child nesting (subagent sessions).
	//
	// Build a batch ID set so we can check whether a child's parent is in
	// this batch or already in the DB from a prior ingest.
	batchIDs := make(map[ingest.SessionID]bool, len(entries))
	sorted := make([]ingest.StoreEntry, 0, len(entries))
	for i := range entries {
		if entries[i].Metadata != nil {
			batchIDs[entries[i].Metadata.SessionID] = true
			if entries[i].Metadata.ParentUUID == nil {
				sorted = append(sorted, entries[i])
			}
		}
	}
	for i := range entries {
		if entries[i].Metadata != nil && entries[i].Metadata.ParentUUID != nil {
			sorted = append(sorted, entries[i])
		}
	}
	// Entries with nil Metadata are already skipped in the loop below.

	for i := range sorted {
		m := sorted[i].Metadata
		if m == nil {
			continue
		}

		// 1. Insert project dimension.
		// V23+: canonical_cwd = m.Project.FilePath, canonical_remote derived from git remote.
		var projectCanonicalRemote any
		if m.Git.Remote != nil && *m.Git.Remote != "" {
			if canonical, normErr := ingest.NormalizeRemoteURL(*m.Git.Remote); normErr == nil {
				projectCanonicalRemote = canonical
			}
		}
		if err = sqlitex.ExecuteTransient(conn, sqlInsertProject, &sqlitex.ExecOptions{
			Args: []any{
				string(m.Project.Hash),
				m.Project.FilePath, // canonical_cwd
				projectCanonicalRemote,
			},
		}); err != nil {
			return fmt.Errorf("store: insert project %s: %w", m.Project.Hash, err)
		}

		// 2. Compute opaque_host_id for this host_slug.
		// opaque_id = HMAC-SHA256(salt, NormalizeRemoteURL(git_remote)) or HMAC-SHA256(salt, host_slug).
		opaqueHostID, canonicalRemote, opaqueErr := s.deriveOpaqueHostID(m.Git.Remote, string(m.HostSlug))
		if opaqueErr != nil {
			return fmt.Errorf("store: derive opaque host id for session %s: %w", m.SessionID, opaqueErr)
		}

		// 3. Insert host_slug dimension (V23+: opaque_id PK).
		var canonicalRemoteVal any
		if canonicalRemote != "" {
			canonicalRemoteVal = canonicalRemote
		}
		if err = sqlitex.ExecuteTransient(conn, sqlInsertHostSlug, &sqlitex.ExecOptions{
			Args: []any{
				opaqueHostID,
				string(m.HostSlug),
				derefString(m.Git.Remote),
				canonicalRemoteVal,
			},
		}); err != nil {
			return fmt.Errorf("store: insert host_slug %s: %w", m.HostSlug, err)
		}

		// 4. Insert session fact.
		// For children whose parent is not in this batch, verify the parent
		// exists in the DB from a prior ingest. If the parent is nowhere,
		// skip this child to avoid an FK constraint failure that would roll
		// back the entire transaction.
		if m.ParentUUID != nil && !batchIDs[*m.ParentUUID] {
			var parentExists bool
			if err = sqlitex.ExecuteTransient(conn, sqlSessionExists, &sqlitex.ExecOptions{
				Args:       []any{string(*m.ParentUUID)},
				ResultFunc: func(stmt *sqlite.Stmt) error { parentExists = true; return nil },
			}); err != nil {
				return fmt.Errorf("store: check parent %s: %w", *m.ParentUUID, err)
			}
			if !parentExists {
				continue // parent not in batch or DB; skip orphan child
			}
		}
		// V23+: opaque_host_id replaces host_slug in sessions FK.
		if err = sqlitex.ExecuteTransient(conn, sqlInsertSession, &sqlitex.ExecOptions{
			Args: []any{
				string(m.SessionID),
				derefSessionID(m.ParentUUID),
				m.ModelHarness.String(),
				string(m.Model),
				opaqueHostID, // opaque_host_id (FK → host_slugs.opaque_id)
				string(m.Project.Hash),
				m.Timestamp.Start,
				m.Timestamp.End,
				derefInt64(m.Timestamp.Ingested),
				m.Source.FilePath,
				string(m.Source.Format),
				m.SchemaVersion,
				derefString(m.Git.Branch),
				derefString(m.Git.Worktree),
				derefString(m.Git.Tracking),
				m.Version,
			},
		}); err != nil {
			return fmt.Errorf("store: insert session %s: %w", m.SessionID, err)
		}

		// 5. Insert session metrics.
		if err = sqlitex.ExecuteTransient(conn, sqlInsertSessionMetrics, &sqlitex.ExecOptions{
			Args: []any{
				string(m.SessionID),
				m.Stats.TurnCount,
				m.Stats.SubagentCount,
				m.Stats.TokensIn,
				m.Stats.TokensOut,
				m.Stats.ToolCallCount,
				float64(m.Stats.DurationMs) / 60000.0,
			},
		}); err != nil {
			return fmt.Errorf("store: insert session_metrics %s: %w", m.SessionID, err)
		}

	}

	return nil
}

// UpdateDailySummary recomputes daily_summary, daily_summary_harness, and
// daily_summary_by_project for the given days.
// Called by the metrics Engine via ComputeInsights after session_metrics are computed.
//
// For each day:
// 1. INSERT OR REPLACE INTO daily_summary (recompute from sessions + session_metrics)
// 2. DELETE + INSERT INTO daily_summary_harness (recompute per model_harness)
// 3. DELETE + INSERT INTO daily_summary_by_project (recompute per project_hash)
func (s *Store) UpdateDailySummary(ctx context.Context, days []string) (err error) {
	if len(days) == 0 {
		return nil
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	for _, day := range days {
		// 1. Recompute daily_summary for this day.
		if err = sqlitex.ExecuteTransient(conn, sqlUpsertDailySummary, &sqlitex.ExecOptions{
			Args: []any{day},
		}); err != nil {
			return fmt.Errorf("store: upsert daily_summary for %s: %w", day, err)
		}

		// 2. Delete existing harness rows for this day, then recompute.
		if err = sqlitex.ExecuteTransient(conn, sqlDeleteDailySummaryHarness, &sqlitex.ExecOptions{
			Args: []any{day},
		}); err != nil {
			return fmt.Errorf("store: delete daily_summary_harness for %s: %w", day, err)
		}

		if err = sqlitex.ExecuteTransient(conn, sqlInsertDailySummaryHarness, &sqlitex.ExecOptions{
			Args: []any{day},
		}); err != nil {
			return fmt.Errorf("store: insert daily_summary_harness for %s: %w", day, err)
		}

		// 3. Delete existing per-project rows for this day, then recompute.
		if err = sqlitex.ExecuteTransient(conn, sqlDeleteDailySummaryByProject, &sqlitex.ExecOptions{
			Args: []any{day},
		}); err != nil {
			return fmt.Errorf("store: delete daily_summary_by_project for %s: %w", day, err)
		}

		if err = sqlitex.ExecuteTransient(conn, sqlInsertDailySummaryByProject, &sqlitex.ExecOptions{
			Args: []any{day},
		}); err != nil {
			return fmt.Errorf("store: insert daily_summary_by_project for %s: %w", day, err)
		}
	}

	return nil
}

const (
	// sqlDeleteOrphanDailySummaryByProject removes daily_summary_by_project rows
	// for project_hash values that no longer appear in the sessions table.
	sqlDeleteOrphanDailySummaryByProject = `DELETE FROM daily_summary_by_project
WHERE project_hash NOT IN (SELECT DISTINCT project_hash FROM sessions)`

	// sqlDeleteOrphanProjects removes projects rows for project_hash values that
	// no longer appear in the sessions table.
	sqlDeleteOrphanProjects = `DELETE FROM projects
WHERE project_hash NOT IN (SELECT DISTINCT project_hash FROM sessions)`
)

// CleanupOrphanProjects deletes projects and related daily_summary_by_project
// rows that have no matching session. Best-effort: non-fatal in the pipeline.
//
// Deletion order matters: daily_summary_by_project references projects via
// project_hash; the child rows are deleted first to avoid FK constraint errors.
func (s *Store) CleanupOrphanProjects(ctx context.Context) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf(
			"store.CleanupOrphanProjects: acquire connection: %w — "+
				"what: failed to obtain a DB connection from the pool; "+
				"why: pool may be exhausted or context cancelled; "+
				"how to fix: check pool size and context deadline",
			err,
		)
	}
	defer s.pool.Put(conn)

	// Delete child rows first (daily_summary_by_project → projects).
	if err := sqlitex.ExecuteTransient(conn, sqlDeleteOrphanDailySummaryByProject, nil); err != nil {
		return fmt.Errorf(
			"store.CleanupOrphanProjects: delete orphan daily_summary_by_project: %w — "+
				"what: DELETE failed on daily_summary_by_project; "+
				"why: unexpected SQLite error; "+
				"how to fix: re-run peasant ingest; if persistent, delete the DB at ~/.local/share/peasant/peasant.db and re-ingest",
			err,
		)
	}

	if err := sqlitex.ExecuteTransient(conn, sqlDeleteOrphanProjects, nil); err != nil {
		return fmt.Errorf(
			"store.CleanupOrphanProjects: delete orphan projects: %w — "+
				"what: DELETE failed on projects; "+
				"why: unexpected SQLite error; "+
				"how to fix: re-run peasant ingest; if persistent, delete the DB at ~/.local/share/peasant/peasant.db and re-ingest",
			err,
		)
	}

	return nil
}

// derefString returns nil (SQL NULL) for nil pointers, or the dereferenced string value.
func derefString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// derefSessionID returns nil (SQL NULL) for nil pointers, or the string representation.
func derefSessionID(p *ingest.SessionID) any {
	if p == nil {
		return nil
	}
	return string(*p)
}
