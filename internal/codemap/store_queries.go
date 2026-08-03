package codemap

import (
	"context"
	"fmt"

	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// SQL for the codemap read path. These queries run over existing tables only
// without a migration; they live here, not in internal/store,
// because they are Map/Review-specific aggregation inputs.
const (
	// sqlProjectCwd resolves a project's canonical working directory and its
	// git remote (for the projectlabel display-name preference). No row =>
	// unknown projectHash (ErrProjectNotFound).
	sqlProjectCwd = `SELECT COALESCE(canonical_cwd, ''), COALESCE(canonical_remote, '') FROM projects WHERE project_hash = ?`

	// sqlAllProjects lists every project the store knows. The hash ordering is
	// a stable scan order only; ProjectSummaries re-sorts by display name.
	sqlAllProjects = `SELECT project_hash, COALESCE(canonical_cwd, ''), COALESCE(canonical_remote, '') FROM projects ORDER BY project_hash`

	// sqlSessionsForProject lists the project's sessions, newest first.
	sqlSessionsForProject = `SELECT
	s.session_id, s.start_ms, s.end_ms, COALESCE(s.git_branch, ''), s.model_harness,
    COALESCE(p.canonical_remote, ''), COALESCE(p.canonical_cwd, '')
FROM sessions s
LEFT JOIN projects p ON p.project_hash = s.project_hash
WHERE s.project_hash = ?
ORDER BY s.start_ms DESC, s.session_id`

	// sqlMetricsForProject reads the per-session metric columns the payloads
	// need: title, outcome, output tokens (the only summable token figure),
	// retry loops, and nullable total cost.
	sqlMetricsForProject = `SELECT
    m.session_id, COALESCE(m.title, ''), COALESCE(m.outcome, ''),
    m.output_tokens, m.retry_loops, m.cost_total_usd
FROM session_metrics m
JOIN sessions s ON s.session_id = m.session_id
WHERE s.project_hash = ?`

	// sqlCommitsForProject reads every commit linked to any of the project's
	// sessions (the ±72h email-filtered heuristic rows written at ingest), with
	// the producer-owned durable association ID for each current relationship.
	sqlCommitsForProject = `SELECT
    sc.session_id, sc.commit_hash, COALESCE(sc.message, ''), sc.commit_time,
    sca.association_id
FROM session_commits sc
JOIN session_commit_associations sca
  ON sca.session_id = sc.session_id
 AND sca.observed_commit_hash = sc.commit_hash
JOIN sessions s ON s.session_id = sc.session_id
WHERE s.project_hash = ?
ORDER BY sc.session_id, sc.commit_hash`

	// sqlAssociationLedgerForProject retains every observed relationship, even
	// when it no longer appears in the current session_commits projection after
	// a re-ingest. This append-only ledger is the source of timeline ghosts.
	sqlAssociationLedgerForProject = `SELECT
    sca.session_id, sca.observed_commit_hash, COALESCE(sca.subject, ''),
    sca.author_time, sca.association_id
FROM session_commit_associations sca
JOIN sessions s ON s.session_id = sca.session_id
WHERE s.project_hash = ?
ORDER BY sca.observed_commit_hash, sca.session_id`

	// sqlEffectiveLabelsForProject lists non-superseded session-level
	// annotations for the project, pre-ordered so the FIRST row per
	// (session, type) is the effective one — the same priority resolution as
	// the store's sqlGetEffectiveAnnotation (priority_override, else
	// human(3) > agent(2) > rule(1), most recent within a tier). The final
	// v.id tiebreak keeps equal-timestamp rows deterministic.
	sqlEffectiveLabelsForProject = `SELECT
    v.target_session_id, v.type_id, v.value
FROM annotations_with_target v
JOIN annotation_types t ON t.id = v.annotation_type_id
JOIN sessions s ON s.session_id = v.target_session_id
WHERE s.project_hash = ?
  AND v.target_session_id IS NOT NULL
  AND v.superseded_by IS NULL
ORDER BY v.target_session_id, v.type_id,
  COALESCE(t.priority_override,
    CASE v.annotator_kind
      WHEN 'human' THEN 3
      WHEN 'agent' THEN 2
      WHEN 'rule'  THEN 1
      ELSE 0
    END) DESC,
  v.created_at DESC, v.id`

	// sqlSearch is the FTS5 transcript search. Ranked by bm25 (lower is
	// better), then stable session and entry coordinates, and paged by the
	// bound LIMIT/OFFSET. snippet()
	// brackets the matched terms in the first indexed column group; -1 lets
	// FTS5 pick the best-matching column. The JOIN back to session_entries by
	// rowid (external-content) recovers the entry's role; the LEFT JOIN to
	// projects resolves a display name (raw canonical_cwd, else the hash).
	//
	// The NOT-EXISTS clause de-duplicates the message-text echo: full-depth
	// indexing stores a message's text on BOTH the depth=0 parent row
	// (content_preview) AND its depth=1 text child (the same block, same
	// truncation — claude_indexer/opencode_indexer), and the FTS triggers index
	// every row. Without this filter a single message returns two hits and the
	// LIMIT fills with duplicates. We drop a depth>0 row only when its
	// content_preview exactly equals its parent's (the echo): unique
	// multi-block depth>0 text (parent preview = first block only) and
	// tool_input/tool_output matches (content_preview NULL or distinct) survive.
	// The parent lookup is a PK point lookup on (session_id, entry_index).
	sqlSearch = `SELECT
    f.session_id,
    f.entry_index,
    e.role,
    COALESCE(p.canonical_cwd, s.project_hash, '') AS project,
    s.project_hash,
    snippet(session_entries_fts, -1, '[', ']', '…', 12) AS snippet,
    bm25(session_entries_fts) AS rank,
    s.model_harness,
    COALESCE(s.git_branch, ''),
    COALESCE(p.canonical_remote, ''),
    COALESCE(p.canonical_cwd, '')
FROM session_entries_fts f
JOIN session_entries e ON e.rowid = f.rowid
JOIN sessions s        ON s.session_id = f.session_id
LEFT JOIN projects p   ON p.project_hash = s.project_hash
WHERE session_entries_fts MATCH ?
  AND NOT (
    e.depth > 0
    AND e.content_preview IS NOT NULL
    AND e.content_preview = (
      SELECT pe.content_preview FROM session_entries pe
      WHERE pe.session_id = e.session_id AND pe.entry_index = e.parent_index
    )
  )
ORDER BY rank, f.session_id, f.entry_index
LIMIT ? OFFSET ?`
)

// projectRow is one projects row.
type projectRow struct {
	hash      schema.ProjectHash
	cwd       string // "" when NULL
	gitRemote string
}

// sessionRow is one sessions row of the project.
type sessionRow struct {
	id          string
	startMs     int64
	endMs       int64
	gitBranch   string // "" when NULL
	harness     string
	gitRemote   string
	projectName string
}

// metricRow is the session_metrics subset the payloads consume.
type metricRow struct {
	title        string
	outcome      string
	outputTokens *int
	retryLoops   *int
	costTotalUSD *float64
}

// commitRow is one session_commits row.
type commitRow struct {
	sessionID     string
	hash          string
	subject       string
	timeMs        *int64
	associationID schema.AssociationID
}

// searchRow is one FTS5 hit row (sqlSearch).
type searchRow struct {
	sessionID   string
	entryIndex  int
	role        string
	project     string // raw canonical_cwd, else project_hash, else ""
	hash        schema.ProjectHash
	snippet     string
	bm25        float64
	harness     string
	gitBranch   string
	gitRemote   string
	projectName string
}

// querySearch runs one deterministic page of the FTS5 MATCH query. match is
// the sanitized FTS5 string; limit is positive and already bounded, and offset
// counts raw ranked rows rather than visibility-filtered results. An empty
// result set is returned as a nil slice.
func (s *Service) querySearch(ctx context.Context, match string, limit, offset int) ([]searchRow, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	var rows []searchRow
	err = sqlitex.ExecuteTransient(conn, sqlSearch, &sqlitex.ExecOptions{
		Args: []any{match, limit, offset},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			projectHash, hashErr := schema.NewProjectHash(stmt.ColumnText(4))
			if hashErr != nil {
				return fmt.Errorf("codemap: search row %q has invalid stored project hash at raw offset %d: %w; run `peasant ingest verify` and repair the store before retrying", stmt.ColumnText(0), offset, hashErr)
			}
			rows = append(rows, searchRow{
				sessionID:   stmt.ColumnText(0),
				entryIndex:  stmt.ColumnInt(1),
				role:        stmt.ColumnText(2),
				project:     stmt.ColumnText(3),
				hash:        projectHash,
				snippet:     stmt.ColumnText(5),
				bm25:        stmt.ColumnFloat(6),
				harness:     stmt.ColumnText(7),
				gitBranch:   stmt.ColumnText(8),
				gitRemote:   stmt.ColumnText(9),
				projectName: stmt.ColumnText(10),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codemap: search %q at raw offset %d with page limit %d: %w", match, offset, limit, err)
	}
	return rows, nil
}

// queryProjectCwd returns (canonicalCwd, canonicalRemote, found). found=false
// means the projectHash is unknown to the store.
func (s *Service) queryProjectCwd(ctx context.Context, projectHash schema.ProjectHash) (string, string, bool, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	var cwd, gitRemote string
	var found bool
	err = sqlitex.ExecuteTransient(conn, sqlProjectCwd, &sqlitex.ExecOptions{
		Args: []any{projectHash.String()},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			cwd = stmt.ColumnText(0)
			gitRemote = stmt.ColumnText(1)
			found = true
			return nil
		},
	})
	if err != nil {
		return "", "", false, fmt.Errorf("codemap: project cwd for %s: %w", projectHash, err)
	}
	return cwd, gitRemote, found, nil
}

// queryProjects lists every project the store knows.
func (s *Service) queryProjects(ctx context.Context) ([]projectRow, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	var rows []projectRow
	err = sqlitex.ExecuteTransient(conn, sqlAllProjects, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			projectHash, hashErr := schema.NewProjectHash(stmt.ColumnText(0))
			if hashErr != nil {
				return fmt.Errorf("codemap: projects row has invalid stored project hash %q: %w; run `peasant ingest verify` and repair the store before retrying", stmt.ColumnText(0), hashErr)
			}
			rows = append(rows, projectRow{
				hash:      projectHash,
				cwd:       stmt.ColumnText(1),
				gitRemote: stmt.ColumnText(2),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codemap: list projects: %w", err)
	}
	return rows, nil
}

// querySessions lists the project's sessions, newest first.
func (s *Service) querySessions(ctx context.Context, projectHash schema.ProjectHash) ([]sessionRow, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	var rows []sessionRow
	err = sqlitex.ExecuteTransient(conn, sqlSessionsForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash.String()},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, sessionRow{
				id:          stmt.ColumnText(0),
				startMs:     stmt.ColumnInt64(1),
				endMs:       stmt.ColumnInt64(2),
				gitBranch:   stmt.ColumnText(3),
				harness:     stmt.ColumnText(4),
				gitRemote:   stmt.ColumnText(5),
				projectName: stmt.ColumnText(6),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codemap: sessions for %s: %w", projectHash, err)
	}
	return rows, nil
}

// queryMetrics maps sessionID -> metricRow for the project.
func (s *Service) queryMetrics(ctx context.Context, projectHash schema.ProjectHash) (map[string]metricRow, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	rows := make(map[string]metricRow)
	err = sqlitex.ExecuteTransient(conn, sqlMetricsForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash.String()},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			m := metricRow{
				title:   stmt.ColumnText(1),
				outcome: stmt.ColumnText(2),
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				v := stmt.ColumnInt(3)
				m.outputTokens = &v
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				v := stmt.ColumnInt(4)
				m.retryLoops = &v
			}
			if stmt.ColumnType(5) != sqlite.TypeNull {
				v := stmt.ColumnFloat(5)
				m.costTotalUSD = &v
			}
			rows[stmt.ColumnText(0)] = m
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codemap: metrics for %s: %w", projectHash, err)
	}
	return rows, nil
}

// queryCommits lists every session-linked commit of the project.
func (s *Service) queryCommits(ctx context.Context, projectHash schema.ProjectHash) ([]commitRow, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	var rows []commitRow
	err = sqlitex.ExecuteTransient(conn, sqlCommitsForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash.String()},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			associationID, associationErr := schema.NewAssociationID(stmt.ColumnText(4))
			if associationErr != nil {
				return fmt.Errorf("codemap: current commit association for session %q hash %q is invalid: %w; run `peasant ingest verify` and repair the store before rendering timeline data", stmt.ColumnText(0), stmt.ColumnText(1), associationErr)
			}
			r := commitRow{
				sessionID:     stmt.ColumnText(0),
				hash:          stmt.ColumnText(1),
				subject:       stmt.ColumnText(2),
				associationID: associationID,
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				v := stmt.ColumnInt64(3)
				r.timeMs = &v
			}
			rows = append(rows, r)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codemap: commits for %s: %w", projectHash, err)
	}
	return rows, nil
}

// queryAssociationLedger lists every historical observed relationship for the
// project. Its hash is never resolved or rewritten here; the review producer
// decides whether the original observation is live or unresolved against the
// displayed default-branch history.
func (s *Service) queryAssociationLedger(ctx context.Context, projectHash schema.ProjectHash) ([]commitRow, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	var rows []commitRow
	err = sqlitex.ExecuteTransient(conn, sqlAssociationLedgerForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash.String()},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			associationID, associationErr := schema.NewAssociationID(stmt.ColumnText(4))
			if associationErr != nil {
				return fmt.Errorf("codemap: association ledger row for session %q observed hash %q has invalid ID: %w; run `peasant ingest verify` and repair the store before rendering timeline data", stmt.ColumnText(0), stmt.ColumnText(1), associationErr)
			}
			row := commitRow{
				sessionID:     stmt.ColumnText(0),
				hash:          stmt.ColumnText(1),
				subject:       stmt.ColumnText(2),
				associationID: associationID,
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				value := stmt.ColumnInt64(3)
				row.timeMs = &value
			}
			rows = append(rows, row)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codemap: association ledger for %s: %w", projectHash, err)
	}
	return rows, nil
}

// queryEffectiveLabels maps sessionID -> effective annotation values, one
// per annotation type (first row per (session, type) wins; the SQL is
// pre-ordered by effective priority). Values are appended in type_id order,
// so label lists are deterministic.
func (s *Service) queryEffectiveLabels(ctx context.Context, projectHash schema.ProjectHash) (map[string][]string, error) {
	conn, err := s.store.Pool().Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("codemap: take connection: %w", err)
	}
	defer s.store.Pool().Put(conn)

	labels := make(map[string][]string)
	var lastSession, lastType string
	err = sqlitex.ExecuteTransient(conn, sqlEffectiveLabelsForProject, &sqlitex.ExecOptions{
		Args: []any{projectHash.String()},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sessionID := stmt.ColumnText(0)
			typeID := stmt.ColumnText(1)
			if sessionID == lastSession && typeID == lastType {
				return nil // lower-priority row for the same (session, type)
			}
			lastSession, lastType = sessionID, typeID
			labels[sessionID] = append(labels[sessionID], stmt.ColumnText(2))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("codemap: effective labels for %s: %w", projectHash, err)
	}
	return labels, nil
}

// listEntries streams a session's indexed entries via the store reader.
func (s *Service) listEntries(ctx context.Context, sessionID string) ([]schema.SessionEntry, error) {
	return s.store.ListEntries(ctx, schema.SessionID(sessionID))
}
