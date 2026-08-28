package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// Store-native row types for the read path
// ---------------------------------------------------------------------------

// SessionRow is a store-native session representation.
// Joins sessions + session_metrics + projects for common queries.
type SessionRow struct {
	SessionID       string
	ModelHarness    string
	ModelID         string
	HostSlug        string
	ProjectHash     string
	ProjectName     string
	StartMs         int64
	EndMs           int64
	GitBranch       *string
	ToolVersion     *string
	TurnCount       int
	ToolCalls       int
	InputTokens     int
	OutputTokens    int
	TokensTotal     int
	DurationMinutes float64

	// Quality-specific columns from session_metrics (nullable).
	Title                *string
	Outcome              *string
	Scope                *string
	FilesTouched         int
	LinesChanged         int
	RetryLoops           int
	RetryTokensWasted    int
	WithinSessionReverts int
	SignalDensity        float64
	SpecQualityScore     float64
	ExplorationRatio     float64
	ScopeBreadth         int
	DiscoveryTurns       int

	// ParentID is set for subagent sessions; nil for top-level sessions.
	ParentID *string

	// CanonicalRemote is the git remote URL from the projects table (nullable).
	CanonicalRemote *string
	// GitWorktree is the session's recorded worktree path. It is empty when the
	// session predates worktree capture or did not come from Git.
	GitWorktree string

	// IndexedAt is the Unix-ms time the session last completed an index pass,
	// or nil when it has never been indexed. A nil value means the session row
	// and its metrics exist but its entries are not yet populated, so the
	// transcript viewer would show no turns. Discovery uses it to withhold such
	// a session from lists until it is actually viewable; it is NOT an access
	// control (a deep link still resolves the session, see SessionSummariesByID).
	IndexedAt *int64

	// SessionOrigin is who drove the session (sessionorigin.User / Agent /
	// Unknown, stored as its wire token) — the sessions.session_origin column
	// written at ingest. Consumers that need the typed value parse it through
	// sessionorigin.Parse; the raw stored string always round-trips through
	// that parse, because the column CHECK admits no other token.
	SessionOrigin string
}

// DashboardRow holds global aggregate metrics from daily_summary.
type DashboardRow struct {
	TotalSessions int
	TotalTokens   int
	AvgDurationMs float64
	AvgTurns      float64
	ToolCallCount int
}

// DailySummaryRow is a single row from daily_summary.
type DailySummaryRow struct {
	DateUTC       string
	SessionCount  int
	TokensIn      int
	TokensOut     int
	TokensTotal   int
	AvgDurationMs float64
	AvgTurns      float64
	ToolCallCount int
}

// SessionDetailRow extends SessionRow with extra fields for the detail view.
// These come from additional JOINs that are only needed for the session_detail channel.
type SessionDetailRow struct {
	SessionRow          // all standard session fields
	GitRemote   *string // from host_slugs.git_remote (via opaque_id JOIN)
	PushedAt    *int64  // from sessions.pushed_at
	ProjectPath string  // from projects.canonical_cwd (V23+: was project_path)
}

// SessionFilter constrains which sessions FilteredSessions returns.
type SessionFilter struct {
	ModelHarness *string // nil = all harnesses
	ProjectHash  *string // nil = all projects
	// HostSlug filters by the human-readable host slug (h.host_slug via JOIN host_slugs).
	// V23+: sessions stores opaque_host_id FK, but callers still filter by slug string.
	HostSlug    *string
	StartFrom   *int64 // nil = no lower bound (unix ms, inclusive)
	StartBefore *int64 // nil = no upper bound (unix ms, exclusive)
}

// SessionListFilter extends SessionFilter with additional constraints for the
// sessions list command. All nil pointer fields mean "no constraint".
type SessionListFilter struct {
	SessionFilter                           // embed base filter (ModelHarness, ProjectHash, HostSlug, StartFrom, StartBefore)
	Tag           *string                   // filter by session tag (matches any tag in the JSON tags array)
	ProjectName   *string                   // filter by project: canonical_remote LIKE '%X%' first, fallback basename(canonical_cwd) = X
	SortField     defaults.SessionSortField // column to sort by (date, turns, tokens, project)
	SortDesc      bool                      // true = DESC (default), false = ASC
	Limit         int                       // 0 = no limit
}

// ---------------------------------------------------------------------------
// SQL constants for the read path
// ---------------------------------------------------------------------------

const (
	// sqlAllSessions is the base query joining sessions, session_metrics, projects, host_slugs.
	// Column order: session_id(0), model_harness(1), model_id(2), host_slug(3),
	// project_hash(4), project_display_name(5), start_ms(6), end_ms(7), git_branch(8),
	// tool_version(9), turn_count(10), tool_calls(11), input_tokens(12),
	// output_tokens(13), tokens_total(14), duration_minutes(15),
	// title(16), outcome(17), scope(18), files_touched(19), lines_changed(20),
	// retry_loops(21), retry_tokens_wasted(22), within_session_reverts(23),
	// signal_density(24), spec_quality_score(25), exploration_ratio(26),
	// scope_breadth(27), discovery_turns(28), parent_id(29), canonical_remote(30),
	// git_worktree(31), session_origin(32)
	//
	// V23+: sessions stores opaque_host_id FK, but we JOIN host_slugs and return
	// the human-readable h.host_slug at col 3 for backward-compatible SessionRow.HostSlug.
	// project_name removed; project display name is COALESCE(p.canonical_cwd, p.project_hash).
	//
	// tokens_total = peak context window usage (input_tokens stores MAX, not SUM).
	sqlAllSessions = `SELECT
    s.session_id, s.model_harness, s.model_id, COALESCE(h.host_slug, s.opaque_host_id),
    s.project_hash, COALESCE(p.canonical_cwd, p.project_hash), s.start_ms, s.end_ms,
    s.git_branch, s.tool_version,
    m.turn_count, m.tool_calls, m.input_tokens, m.output_tokens,
    COALESCE(m.input_tokens, 0) + COALESCE(m.output_tokens, 0), m.duration_minutes,
    m.title, m.outcome, m.scope,
    m.files_touched, m.lines_changed,
    m.retry_loops, m.retry_tokens_wasted, m.within_session_reverts,
    m.signal_density, m.spec_quality_score, m.exploration_ratio,
    m.scope_breadth, m.discovery_turns,
    s.parent_id,
    p.canonical_remote,
    COALESCE(s.git_worktree, ''),
    s.session_origin,
    s.indexed_at
FROM sessions s
JOIN session_metrics m ON s.session_id = m.session_id
JOIN projects p ON s.project_hash = p.project_hash
LEFT JOIN host_slugs h ON s.opaque_host_id = h.opaque_id`

	sqlSessionByID = sqlAllSessions + ` WHERE s.session_id = ?`

	// sqlSessionDetailByID extends sqlAllSessions with extra columns for the detail view.
	// Columns 0-33: same as sqlAllSessions (scanned by scanSessionRow), including
	//               column 33 s.indexed_at.
	// Column 34: h.git_remote (nullable, from host_slugs JOIN on opaque_id)
	// Column 35: s.pushed_at (nullable)
	// Column 36: p.canonical_cwd (project working dir, replaces project_path)
	sqlSessionDetailByID = `SELECT
    s.session_id, s.model_harness, s.model_id, COALESCE(h.host_slug, s.opaque_host_id),
    s.project_hash, COALESCE(p.canonical_cwd, p.project_hash), s.start_ms, s.end_ms,
    s.git_branch, s.tool_version,
    m.turn_count, m.tool_calls, m.input_tokens, m.output_tokens,
    COALESCE(m.input_tokens, 0) + COALESCE(m.output_tokens, 0), m.duration_minutes,
    m.title, m.outcome, m.scope,
    m.files_touched, m.lines_changed,
    m.retry_loops, m.retry_tokens_wasted, m.within_session_reverts,
    m.signal_density, m.spec_quality_score, m.exploration_ratio,
    m.scope_breadth, m.discovery_turns,
    s.parent_id,
    p.canonical_remote,
    COALESCE(s.git_worktree, ''),
    s.session_origin,
    s.indexed_at,
    h.git_remote, s.pushed_at, COALESCE(p.canonical_cwd, '')
FROM sessions s
JOIN session_metrics m ON s.session_id = m.session_id
JOIN projects p ON s.project_hash = p.project_hash
LEFT JOIN host_slugs h ON s.opaque_host_id = h.opaque_id
WHERE s.session_id = ?`

	sqlDashboardAggregates = `SELECT
    COALESCE(SUM(session_count), 0),
    COALESCE(SUM(tokens_total), 0),
    CASE WHEN COUNT(*) = 0 THEN 0.0
         ELSE SUM(avg_duration_ms * session_count) / SUM(session_count)
    END,
    CASE WHEN COUNT(*) = 0 THEN 0.0
         ELSE SUM(avg_turns * session_count) / SUM(session_count)
    END,
    COALESCE(SUM(tool_call_count), 0)
FROM daily_summary`

	sqlHarnessCounts = `SELECT model_harness, SUM(session_count)
FROM daily_summary_harness
GROUP BY model_harness`

	sqlDailySummaries = `SELECT
    date_utc, session_count, tokens_in, tokens_out, tokens_total,
    avg_duration_ms, avg_turns, tool_call_count
FROM daily_summary
ORDER BY date_utc DESC`

	sqlChildSessions = `SELECT s.session_id, s.start_ms, COALESCE(p.canonical_cwd, p.project_hash), COALESCE(p.canonical_remote, '')
FROM sessions s
JOIN projects p ON s.project_hash = p.project_hash
WHERE s.parent_id = ?
ORDER BY s.start_ms ASC`
)

// ChildSessionRow is a lightweight row for child (subagent) session references.
type ChildSessionRow struct {
	SessionID       string
	StartMs         int64
	ProjectName     string
	CanonicalRemote string // "" when the project has no recorded git remote
}

// ---------------------------------------------------------------------------
// Read methods
// ---------------------------------------------------------------------------

// AllSessions returns all sessions ordered by start_ms DESC.
func (s *Store) AllSessions(ctx context.Context) ([]SessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: all sessions take connection: %w", err)
	}
	defer s.pool.Put(conn)

	query := sqlAllSessions + " ORDER BY s.start_ms DESC"

	var rows []SessionRow
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanSessionRow(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: all sessions query: %w", err)
	}
	return rows, nil
}

// ChildSessionsForParent returns child (subagent) sessions for a given parent session ID.
func (s *Store) ChildSessionsForParent(ctx context.Context, parentID string) ([]ChildSessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: child sessions take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []ChildSessionRow
	err = sqlitex.ExecuteTransient(conn, sqlChildSessions, &sqlitex.ExecOptions{
		Args: []any{parentID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, ChildSessionRow{
				SessionID:       stmt.ColumnText(0),
				StartMs:         stmt.ColumnInt64(1),
				ProjectName:     stmt.ColumnText(2),
				CanonicalRemote: stmt.ColumnText(3),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: child sessions query: %w", err)
	}
	return rows, nil
}

// AllSessionIDs returns all session IDs from the sessions table.
// Unlike AllSessions, this does NOT join on session_metrics, so it returns
// sessions that have not yet been computed — exactly what metrics compute needs.
func (s *Store) AllSessionIDs(ctx context.Context) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: all session ids take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var ids []string
	err = sqlitex.ExecuteTransient(conn, `SELECT session_id FROM sessions ORDER BY start_ms DESC`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ids = append(ids, stmt.ColumnText(0))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: all session ids query: %w", err)
	}
	return ids, nil
}

// IngestedSessionRow is the store-recorded view of one ingested session: the
// display and identity values a re-scan or selection projection can reuse
// instead of resolving them from git again, plus the two columns the diff stage
// classifies a discovered source against.
type IngestedSessionRow struct {
	SessionID     string
	Harness       string // sessions.model_harness
	ProjectHash   string // sessions.project_hash
	GitRemote     string // host_slugs.git_remote as ingest resolved it ("" when the project has no remote)
	Branch        string // sessions.git_branch ("" when unknown or non-git)
	GitWorktree   string // sessions.git_worktree ("" when unknown or non-git)
	CanonicalCwd  string // projects.canonical_cwd ("" when unknown)
	Title         string // session_metrics.title ("" when metrics have not been computed)
	IngestedMs    int64
	SchemaVersion int
}

// sqlAllIngestedSessions reads every session with the dimension values a
// re-scan reuses. The metrics join is LEFT because a session can outlive its
// metrics row (prune deletes the metrics row before the session row), and a
// session dropped here is one a caller reading this as "everything the store
// holds" would treat as new and pay to resolve again. The dimension joins are
// LEFT to match the sibling session queries; a foreign key already guarantees
// the host slug row exists.
const sqlAllIngestedSessions = `SELECT
    s.session_id,
    s.model_harness,
    s.project_hash,
    COALESCE(h.git_remote, ''),
    COALESCE(s.git_branch, ''),
    COALESCE(s.git_worktree, ''),
    COALESCE(p.canonical_cwd, ''),
    COALESCE(m.title, ''),
    s.ingested_ms,
    s.schema_version
FROM sessions s
LEFT JOIN host_slugs h ON s.opaque_host_id = h.opaque_id
LEFT JOIN projects p ON s.project_hash = p.project_hash
LEFT JOIN session_metrics m ON s.session_id = m.session_id
ORDER BY s.start_ms DESC`

// AllIngestedSessions returns the recorded harness, stable project hash, remote,
// branch, worktree, canonical project directory, and title for every session in
// the store, with the ingest timestamp and metadata schema version the diff
// stage needs. It is the read a re-scan uses to answer "what do I already know
// about this session" without walking git again.
func (s *Store) AllIngestedSessions(ctx context.Context) ([]IngestedSessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: all ingested sessions take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []IngestedSessionRow
	err = sqlitex.ExecuteTransient(conn, sqlAllIngestedSessions, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, IngestedSessionRow{
				SessionID:     stmt.ColumnText(0),
				Harness:       stmt.ColumnText(1),
				ProjectHash:   stmt.ColumnText(2),
				GitRemote:     stmt.ColumnText(3),
				Branch:        stmt.ColumnText(4),
				GitWorktree:   stmt.ColumnText(5),
				CanonicalCwd:  stmt.ColumnText(6),
				Title:         stmt.ColumnText(7),
				IngestedMs:    stmt.ColumnInt64(8),
				SchemaVersion: int(stmt.ColumnInt64(9)),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: all ingested sessions query: %w", err)
	}
	return rows, nil
}

// SessionByID returns a single session by ID, or nil if not found.
func (s *Store) SessionByID(ctx context.Context, sessionID string) (*SessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: session by id take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var row *SessionRow
	err = sqlitex.ExecuteTransient(conn, sqlSessionByID, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			r := scanSessionRow(stmt)
			row = &r
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: session by id query: %w", err)
	}
	return row, nil
}

// SessionDetailByID returns an extended session row with extra fields for the
// detail view (git_remote, pushed_at, canonical_cwd). Returns nil if not found.
func (s *Store) SessionDetailByID(ctx context.Context, sessionID string) (*SessionDetailRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: session detail by id take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var row *SessionDetailRow
	err = sqlitex.ExecuteTransient(conn, sqlSessionDetailByID, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			r := scanSessionDetailRow(stmt)
			row = &r
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: session detail by id query: %w", err)
	}
	return row, nil
}

// DashboardAggregates returns global aggregate metrics from daily_summary.
// When daily_summary has no rows, returns a zeroed DashboardRow (not nil, not error).
func (s *Store) DashboardAggregates(ctx context.Context) (*DashboardRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: dashboard aggregates take connection: %w", err)
	}
	defer s.pool.Put(conn)

	row := &DashboardRow{}
	err = sqlitex.ExecuteTransient(conn, sqlDashboardAggregates, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row.TotalSessions = stmt.ColumnInt(0)
			row.TotalTokens = stmt.ColumnInt(1)
			row.AvgDurationMs = stmt.ColumnFloat(2)
			row.AvgTurns = stmt.ColumnFloat(3)
			row.ToolCallCount = stmt.ColumnInt(4)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: dashboard aggregates query: %w", err)
	}
	return row, nil
}

// HarnessCounts returns session counts grouped by model_harness from daily_summary_harness.
func (s *Store) HarnessCounts(ctx context.Context) (map[string]int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: harness counts take connection: %w", err)
	}
	defer s.pool.Put(conn)

	counts := make(map[string]int)
	err = sqlitex.ExecuteTransient(conn, sqlHarnessCounts, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			counts[stmt.ColumnText(0)] = stmt.ColumnInt(1)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: harness counts query: %w", err)
	}
	return counts, nil
}

// DailySummaries returns all daily_summary rows ordered by date_utc DESC.
func (s *Store) DailySummaries(ctx context.Context) ([]DailySummaryRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: daily summaries take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var rows []DailySummaryRow
	err = sqlitex.ExecuteTransient(conn, sqlDailySummaries, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, DailySummaryRow{
				DateUTC:       stmt.ColumnText(0),
				SessionCount:  stmt.ColumnInt(1),
				TokensIn:      stmt.ColumnInt(2),
				TokensOut:     stmt.ColumnInt(3),
				TokensTotal:   stmt.ColumnInt(4),
				AvgDurationMs: stmt.ColumnFloat(5),
				AvgTurns:      stmt.ColumnFloat(6),
				ToolCallCount: stmt.ColumnInt(7),
			})
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: daily summaries query: %w", err)
	}
	return rows, nil
}

// FilteredSessions returns sessions matching the given filter, ordered by start_ms DESC.
// Nil filter fields are ignored (no constraint applied for that field).
func (s *Store) FilteredSessions(ctx context.Context, f SessionFilter) ([]SessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: filtered sessions take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var conditions []string
	var args []any
	if f.ModelHarness != nil {
		conditions = append(conditions, "s.model_harness = ?")
		args = append(args, *f.ModelHarness)
	}
	if f.ProjectHash != nil {
		conditions = append(conditions, "s.project_hash = ?")
		args = append(args, *f.ProjectHash)
	}
	if f.HostSlug != nil {
		// V23+: sessions stores opaque_host_id; sqlAllSessions already JOINs host_slugs h.
		// Filter on the human-readable slug column from the JOIN.
		conditions = append(conditions, "h.host_slug = ?")
		args = append(args, *f.HostSlug)
	}
	if f.StartFrom != nil {
		conditions = append(conditions, "s.start_ms >= ?")
		args = append(args, *f.StartFrom)
	}
	if f.StartBefore != nil {
		conditions = append(conditions, "s.start_ms < ?")
		args = append(args, *f.StartBefore)
	}

	query := sqlAllSessions
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY s.start_ms DESC"

	var rows []SessionRow
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanSessionRow(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: filtered sessions query: %w", err)
	}
	return rows, nil
}

// SessionSourceInfo holds the source file location and provider for a session transcript.
type SessionSourceInfo struct {
	SourcePath   string
	SourceFormat schema.SourceFormat
	Harness      string // model_harness value (e.g. "claude-code", "opencode")
}

// SessionSourceInfo returns the source file path, format, and provider for a session.
// Returns nil, nil if the session ID does not exist in the store.
func (s *Store) SessionSourceInfo(ctx context.Context, sessionID string) (*SessionSourceInfo, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.SessionSourceInfo: take connection for session %q: %w", sessionID, err)
	}
	defer s.pool.Put(conn)

	var info *SessionSourceInfo
	err = sqlitex.ExecuteTransient(conn,
		`SELECT source_path, source_format, model_harness FROM sessions WHERE session_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{sessionID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				sf := schema.SourceFormat(stmt.ColumnText(1))
				if !sf.IsValid() {
					slog.Warn("store: invalid source_format in DB", "session_id", sessionID, "value", stmt.ColumnText(1))
				}
				info = &SessionSourceInfo{
					SourcePath:   stmt.ColumnText(0),
					SourceFormat: sf,
					Harness:      stmt.ColumnText(2),
				}
				return nil
			},
		})
	if err != nil {
		return nil, fmt.Errorf("store.SessionSourceInfo: query for session %q: %w", sessionID, err)
	}
	return info, nil
}

// BulkLookupSessionLocations returns host_slug and parent_id for all given
// session IDs in a single query. Sessions not found in the DB are omitted
// from the returned map (not an error).
func (s *Store) BulkLookupSessionLocations(ctx context.Context, sessionIDs []ingest.SessionID) (map[ingest.SessionID]ingest.SessionLocation, error) {
	if len(sessionIDs) == 0 {
		return map[ingest.SessionID]ingest.SessionLocation{}, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("bulk lookup session locations: take conn: %w", err)
	}
	defer s.pool.Put(conn)

	// Build: SELECT s.session_id, h.host_slug, COALESCE(s.parent_id,''), s.ingested_ms, s.schema_version
	//        FROM sessions s JOIN host_slugs h ON s.opaque_host_id = h.opaque_id
	//        WHERE s.session_id IN (?,?,?,...)
	// V23+: host_slug is no longer a column on sessions; must JOIN host_slugs.
	// V8 diff: ingested_ms and schema_version are also fetched for DB-first diff stage.
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = string(id)
	}
	q := `SELECT s.session_id, h.host_slug, COALESCE(s.parent_id,''), s.ingested_ms, s.schema_version
FROM sessions s
JOIN host_slugs h ON s.opaque_host_id = h.opaque_id
WHERE s.session_id IN (` +
		strings.Join(placeholders, ",") + ")"

	result := make(map[ingest.SessionID]ingest.SessionLocation, len(sessionIDs))
	err = sqlitex.ExecuteTransient(conn, q, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			id := schema.SessionID(stmt.ColumnText(0))
			ingestedMs := stmt.ColumnInt64(3)
			schemaVersion := int(stmt.ColumnInt64(4))
			result[id] = ingest.SessionLocation{
				HostSlug:      stmt.ColumnText(1),
				ParentID:      stmt.ColumnText(2),
				IngestedMs:    &ingestedMs,
				SchemaVersion: schemaVersion,
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("bulk lookup session locations: %w", err)
	}
	return result, nil
}

// buildSessionListFilterWhere constructs the WHERE conditions and bind arguments for a
// SessionListFilter. It is the single source of truth for filter → SQL mapping shared by
// ListSessionsFiltered and CountSessionsFiltered; keeping them in sync is guaranteed by
// delegation — neither re-implements the predicate independently.
//
// Project name matching (f.ProjectName) uses a two-step priority:
//  1. canonical_remote LIKE '%X%'  (preferred: git remote URL contains name)
//  2. fallback: basename(canonical_cwd) = X when canonical_remote is NULL or no match
//
// Tag filtering (f.Tag) uses SQLite json_each() over the sessions.tags JSON array.
func buildSessionListFilterWhere(f SessionListFilter) (conditions []string, args []any) {
	// Base SessionFilter conditions.
	if f.ModelHarness != nil {
		conditions = append(conditions, "s.model_harness = ?")
		args = append(args, *f.ModelHarness)
	}
	if f.ProjectHash != nil {
		conditions = append(conditions, "s.project_hash = ?")
		args = append(args, *f.ProjectHash)
	}
	if f.HostSlug != nil {
		conditions = append(conditions, "h.host_slug = ?")
		args = append(args, *f.HostSlug)
	}
	if f.StartFrom != nil {
		conditions = append(conditions, "s.start_ms >= ?")
		args = append(args, *f.StartFrom)
	}
	if f.StartBefore != nil {
		conditions = append(conditions, "s.start_ms < ?")
		args = append(args, *f.StartBefore)
	}

	// Tag filter: use json_each over the sessions.tags JSON array.
	// tags is a JSON array like ["bugfix","wip"]. json_each() treats s.tags as a virtual table.
	if f.Tag != nil {
		conditions = append(conditions,
			"EXISTS (SELECT 1 FROM json_each(s.tags) WHERE json_each.value = ?)")
		args = append(args, *f.Tag)
	}

	// ProjectName filter: canonical_remote LIKE '%X%' first, fallback to basename(canonical_cwd).
	// The fallback fires when canonical_remote IS NULL OR when it exists but does NOT contain X.
	// SUBSTR trick: extracts the last N characters of canonical_cwd where N = LENGTH(pname),
	// then compares case-insensitively. This avoids SQLite's lack of a basename() function.
	if f.ProjectName != nil {
		pname := *f.ProjectName
		conditions = append(conditions,
			"(p.canonical_remote LIKE '%' || ? || '%'"+
				" OR ("+
				"   (p.canonical_remote IS NULL OR p.canonical_remote NOT LIKE '%' || ? || '%')"+
				"   AND p.canonical_cwd IS NOT NULL"+
				"   AND LOWER(?) = LOWER(SUBSTR(p.canonical_cwd, LENGTH(p.canonical_cwd) - LENGTH(?) + 1))"+
				" ))",
		)
		args = append(args, pname, pname, pname, pname)
	}
	return conditions, args
}

// ListSessionsFiltered returns sessions matching the given SessionListFilter,
// ordered per f.SortField and f.SortDesc. All nil/zero filter fields are ignored.
func (s *Store) ListSessionsFiltered(ctx context.Context, f SessionListFilter) ([]SessionRow, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: ListSessionsFiltered take connection: %w", err)
	}
	defer s.pool.Put(conn)

	conditions, args := buildSessionListFilterWhere(f)

	// Build ORDER BY clause.
	sortCol := sortFieldToColumn(f.SortField)
	dir := "DESC"
	if !f.SortDesc {
		dir = "ASC"
	}

	query := sqlAllSessions
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY " + sortCol + " " + dir

	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}

	var rows []SessionRow
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, scanSessionRow(stmt))
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: ListSessionsFiltered query: %w", err)
	}
	return rows, nil
}

// CountSessionsFiltered returns the number of sessions that match f (ignoring Limit).
// It uses the same WHERE predicate as ListSessionsFiltered via the shared
// buildSessionListFilterWhere helper, so the two counts can never drift.
func (s *Store) CountSessionsFiltered(ctx context.Context, f SessionListFilter) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: CountSessionsFiltered take connection: %w", err)
	}
	defer s.pool.Put(conn)

	conditions, args := buildSessionListFilterWhere(f)

	// COUNT(*) over the same JOIN as sqlAllSessions. We omit session_metrics columns
	// because COUNT only needs the FK join for WHERE predicates that reference metric cols.
	// The base FROM+JOIN is identical so the WHERE clause applies identically.
	query := `SELECT COUNT(*) FROM sessions s
JOIN session_metrics m ON s.session_id = m.session_id
JOIN projects p ON s.project_hash = p.project_hash
LEFT JOIN host_slugs h ON s.opaque_host_id = h.opaque_id`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}

	var count int
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		return 0, fmt.Errorf("store: CountSessionsFiltered query: %w", err)
	}
	return count, nil
}

// sortFieldToColumn maps a SessionSortField to the SQL column expression used for ORDER BY.
func sortFieldToColumn(f defaults.SessionSortField) string {
	switch f {
	case defaults.SessionSortTurns:
		return "m.turn_count"
	case defaults.SessionSortTokens:
		// tokens_total in sqlAllSessions is COALESCE(m.input_tokens, 0); use m.input_tokens for sort.
		return "COALESCE(m.input_tokens, 0)"
	case defaults.SessionSortProject:
		return "COALESCE(p.canonical_cwd, p.project_hash)"
	default: // SessionSortDate and any unknown value
		return "s.start_ms"
	}
}

// FirstUserMessage returns the content_preview of the first user message (depth=0)
// in a session, truncated to SessionPreviewMaxChars Unicode runes. Returns "" if
// no user entries exist or the session does not exist.
func (s *Store) FirstUserMessage(ctx context.Context, sessionID string) (string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", fmt.Errorf("store: FirstUserMessage take connection: %w", err)
	}
	defer s.pool.Put(conn)

	const q = `SELECT content_preview FROM session_entries
WHERE session_id = ? AND role = 'user' AND depth = 0
ORDER BY entry_index ASC LIMIT 1`

	var preview string
	err = sqlitex.ExecuteTransient(conn, q, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				preview = stmt.ColumnText(0)
			}
			return nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("store: FirstUserMessage query for session %q: %w", sessionID, err)
	}

	return TruncateToRunes(preview, defaults.SessionPreviewMaxChars), nil
}

// FirstUserMessageBulk returns the content_preview of the first user message (depth=0)
// for each session in sessionIDs, using a single IN(...) query. Sessions that have no
// user entry are OMITTED from the returned map (the caller should treat a missing key as
// an empty preview — this matches the current behavior of FirstUserMessage returning "").
//
// All previews are truncated to SessionPreviewMaxChars runes for parity with
// the single-row FirstUserMessage.
func (s *Store) FirstUserMessageBulk(ctx context.Context, sessionIDs []string) (map[string]string, error) {
	if len(sessionIDs) == 0 {
		return map[string]string{}, nil
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: FirstUserMessageBulk take connection: %w", err)
	}
	defer s.pool.Put(conn)

	// Build a single IN(...) query so we pay one round-trip regardless of session count.
	// The subquery per-session MIN(entry_index) picks the first user message per session.
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := `SELECT session_id, content_preview FROM session_entries
WHERE role = 'user' AND depth = 0
  AND (session_id, entry_index) IN (
    SELECT session_id, MIN(entry_index)
    FROM session_entries
    WHERE session_id IN (` + strings.Join(placeholders, ", ") + `) AND role = 'user' AND depth = 0
    GROUP BY session_id
  )`

	result := make(map[string]string, len(sessionIDs))
	err = sqlitex.ExecuteTransient(conn, q, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			sid := stmt.ColumnText(0)
			var preview string
			if stmt.ColumnType(1) != sqlite.TypeNull {
				preview = stmt.ColumnText(1)
			}
			result[sid] = TruncateToRunes(preview, defaults.SessionPreviewMaxChars)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: FirstUserMessageBulk query: %w", err)
	}
	return result, nil
}

// TruncateToRunes truncates s to at most maxRunes Unicode code points.
// It always returns a valid UTF-8 string (no split multi-byte sequences).
// Exported so that other packages, including CLI formatting, can reuse the
// canonical rune-safe truncator without spawning a second implementation.
func TruncateToRunes(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	i := 0
	count := 0
	for j := range s {
		if count == maxRunes {
			i = j
			break
		}
		count++
	}
	return s[:i]
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// scanSessionDetailRow reads a SessionDetailRow from the current statement.
// Columns 0-33 are scanned by scanSessionRow (33 = s.indexed_at); columns 34-36
// are the extra detail fields.
func scanSessionDetailRow(stmt *sqlite.Stmt) SessionDetailRow {
	row := SessionDetailRow{
		SessionRow: scanSessionRow(stmt),
	}
	// Column 34: h.git_remote (nullable)
	if stmt.ColumnType(34) != sqlite.TypeNull {
		v := stmt.ColumnText(34)
		row.GitRemote = &v
	}
	// Column 35: s.pushed_at (nullable)
	if stmt.ColumnType(35) != sqlite.TypeNull {
		v := stmt.ColumnInt64(35)
		row.PushedAt = &v
	}
	// Column 36: p.canonical_cwd (V23+: was project_path)
	row.ProjectPath = stmt.ColumnText(36)
	return row
}

// scanSessionRow reads a SessionRow from the current statement position.
// Column order must match sqlAllSessions.
func scanSessionRow(stmt *sqlite.Stmt) SessionRow {
	row := SessionRow{
		SessionID:       stmt.ColumnText(0),
		ModelHarness:    stmt.ColumnText(1),
		ModelID:         stmt.ColumnText(2),
		HostSlug:        stmt.ColumnText(3),
		ProjectHash:     stmt.ColumnText(4),
		ProjectName:     stmt.ColumnText(5),
		StartMs:         stmt.ColumnInt64(6),
		EndMs:           stmt.ColumnInt64(7),
		TurnCount:       stmt.ColumnInt(10),
		ToolCalls:       stmt.ColumnInt(11),
		InputTokens:     stmt.ColumnInt(12),
		OutputTokens:    stmt.ColumnInt(13),
		TokensTotal:     stmt.ColumnInt(14),
		DurationMinutes: stmt.ColumnFloat(15),
	}

	// Nullable columns: git_branch (col 8), tool_version (col 9)
	if stmt.ColumnType(8) != sqlite.TypeNull {
		v := stmt.ColumnText(8)
		row.GitBranch = &v
	}
	if stmt.ColumnType(9) != sqlite.TypeNull {
		v := stmt.ColumnText(9)
		row.ToolVersion = &v
	}

	// Quality columns from session_metrics (cols 16–28, all nullable).
	if stmt.ColumnType(16) != sqlite.TypeNull {
		v := stmt.ColumnText(16)
		row.Title = &v
	}
	if stmt.ColumnType(17) != sqlite.TypeNull {
		v := stmt.ColumnText(17)
		row.Outcome = &v
	}
	if stmt.ColumnType(18) != sqlite.TypeNull {
		v := stmt.ColumnText(18)
		row.Scope = &v
	}
	if stmt.ColumnType(19) != sqlite.TypeNull {
		row.FilesTouched = stmt.ColumnInt(19)
	}
	if stmt.ColumnType(20) != sqlite.TypeNull {
		row.LinesChanged = stmt.ColumnInt(20)
	}
	if stmt.ColumnType(21) != sqlite.TypeNull {
		row.RetryLoops = stmt.ColumnInt(21)
	}
	if stmt.ColumnType(22) != sqlite.TypeNull {
		row.RetryTokensWasted = stmt.ColumnInt(22)
	}
	if stmt.ColumnType(23) != sqlite.TypeNull {
		row.WithinSessionReverts = stmt.ColumnInt(23)
	}
	if stmt.ColumnType(24) != sqlite.TypeNull {
		row.SignalDensity = stmt.ColumnFloat(24)
	}
	if stmt.ColumnType(25) != sqlite.TypeNull {
		row.SpecQualityScore = stmt.ColumnFloat(25)
	}
	if stmt.ColumnType(26) != sqlite.TypeNull {
		row.ExplorationRatio = stmt.ColumnFloat(26)
	}
	if stmt.ColumnType(27) != sqlite.TypeNull {
		row.ScopeBreadth = stmt.ColumnInt(27)
	}
	if stmt.ColumnType(28) != sqlite.TypeNull {
		row.DiscoveryTurns = stmt.ColumnInt(28)
	}

	// Column 29: s.parent_id (nullable — set for subagent sessions)
	if stmt.ColumnType(29) != sqlite.TypeNull {
		v := stmt.ColumnText(29)
		row.ParentID = &v
	}

	// Column 30: p.canonical_remote (nullable — git remote URL from projects table)
	if stmt.ColumnType(30) != sqlite.TypeNull {
		v := stmt.ColumnText(30)
		row.CanonicalRemote = &v
	}

	// Column 31: s.git_worktree (COALESCE'd to an empty string)
	row.GitWorktree = stmt.ColumnText(31)

	// Column 32: s.session_origin. NOT NULL with a CHECK over the three menu
	// tokens, so it always arrives as a value sessionorigin.Parse accepts.
	row.SessionOrigin = stmt.ColumnText(32)

	// Column 33: s.indexed_at (nullable). NULL until the session completes an
	// index pass; a populated value means its entries exist and it is viewable.
	if stmt.ColumnType(33) != sqlite.TypeNull {
		v := stmt.ColumnInt64(33)
		row.IndexedAt = &v
	}

	return row
}
