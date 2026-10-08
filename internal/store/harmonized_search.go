package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// SearchState is the store-global search index health (design §3.5): exactly
// one row. It is set when a delete used untrustworthy FTS values; search
// refuses while it is set; the whole-index rebuild clears it.
type SearchState struct {
	NeedsRebuild bool
}

// SearchNeedsOptimizeRatio is the size-ratio threshold harvest verify
// --content reports: a needs-optimize state above 1.5x a fresh build.
const SearchNeedsOptimizeRatio = 1.5

// searchFreshBuildFactor is the measured fresh-build cost: about 0.39x the
// indexed text bytes on the live store.
const searchFreshBuildFactor = 0.39

// searchStateReadOnConn reports the store-global search index health on the
// caller's connection.
func searchStateReadOnConn(conn *sqlite.Conn) (SearchState, error) {
	var state SearchState
	if err := sqlitex.ExecuteTransient(conn, `SELECT needs_rebuild FROM session_search_state WHERE id = 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			state.NeedsRebuild = stmt.ColumnInt64(0) == 1
			return nil
		},
	}); err != nil {
		return SearchState{}, fmt.Errorf("store: read the search index health: %w; run `peasant harvest verify --content` to inspect the index", err)
	}
	return state, nil
}

// SearchStateSetNeedsRebuild flags the store-global search index for a
// whole-index rebuild on the caller's connection: a deleting transaction
// calls it when the deleted row failed the serializeEntry digest check, so
// its indexed values cannot be trusted for the BEFORE DELETE un-indexing.
// (The contract sketch names tx *sqlitex.Tx; the vendored sqlite fork has no
// Tx type — every store helper takes *sqlite.Conn — so the seam takes the
// connection.)
func SearchStateSetNeedsRebuild(ctx context.Context, conn *sqlite.Conn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if conn == nil {
		return fmt.Errorf("store: flag the search index for rebuild with no connection: pass the deleting transaction's connection; nothing was flagged; retry with the caller's *sqlite.Conn")
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild = 1 WHERE id = 1`, nil); err != nil {
		return fmt.Errorf("store: flag the search index for rebuild: %w; the stale postings are still indexed and search refuses until the flag is set; retry the delete", err)
	}
	return nil
}

// verifyBodyForDelete reports whether a body row's stored columns are exactly
// what the FTS insert indexed: serializeEntry(row) must hash to body_digest.
// A mismatch — or a row that fails to hash at all — counts as untrusted, and
// the deleting transaction must set needs_rebuild instead of trusting the
// BEFORE DELETE trigger's values.
func verifyBodyForDelete(row EntryRecord) bool {
	if row.BodyDigest == "" {
		return false
	}
	data, err := json.Marshal(entryFromRow(row))
	if err != nil {
		return false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) == row.BodyDigest
}

// trustBodyForDelete owns missing-row and digest semantics for every delete
// path. Missing or unserializable bodies are untrusted, never assumed safe.
func trustBodyForDelete(conn *sqlite.Conn, sid schema.SessionID, digest string) (found, trusted bool, err error) {
	err = sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id=? AND body_digest=?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), digest}, ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			trusted = verifyBodyForDelete(scanEntryRecord(stmt))
			return nil
		},
	})
	if err != nil {
		return false, false, fmt.Errorf("store: verify body %s of session %s before delete: %w; nothing was deleted; retry the operation", digest, sid, err)
	}
	return found, trusted, nil
}

// SearchState returns the store-global search index health.
func (s *Store) SearchState(ctx context.Context) (SearchState, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return SearchState{}, fmt.Errorf("store: take connection to read the search index health: %w", err)
	}
	defer s.pool.Put(conn)
	return searchStateReadOnConn(conn)
}

// RebuildSearchIndex runs the whole-index rebuild in one transaction: it
// re-tokenizes the union view and clears the needs_rebuild flag, so search
// is trusted again. FTS5 cannot remove postings by rowid without the
// original values, so the rebuild is whole-index, never per-row.
func (s *Store) RebuildSearchIndex(ctx context.Context) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to rebuild the search index: %w", err)
	}
	defer s.pool.Put(conn)
	txnErr := error(nil)
	endFn := sqlitex.Transaction(conn)
	defer endFn(&txnErr)
	if err := rebuildSearchIndexOnConn(conn); err != nil {
		txnErr = err
		return txnErr
	}
	return nil
}

// EnsureSearchIndexHealthy runs the index-health gate every writer-lane entry
// point calls before other work: when needs_rebuild is set it rebuilds the
// whole index and clears the flag, so the flag keeps the state crash-safe.
// It reports whether it rebuilt.
func (s *Store) EnsureSearchIndexHealthy(ctx context.Context) (bool, error) {
	state, err := s.SearchState(ctx)
	if err != nil {
		return false, err
	}
	if !state.NeedsRebuild {
		return false, nil
	}
	if err := s.RebuildSearchIndex(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// rebuildSearchIndexOnConn runs the whole-index rebuild on the caller's
// connection: it re-tokenizes the union view and clears the needs_rebuild
// flag. Callers already inside a transaction (prune, the sweep) run it
// inline; RebuildSearchIndex wraps it in its own transaction. This is the
// one home for the rebuild statements.
func rebuildSearchIndexOnConn(conn *sqlite.Conn) error {
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_search_fts(session_search_fts) VALUES('rebuild')`, nil); err != nil {
		return fmt.Errorf("store: rebuild the search index over the union view: %w; the index is unchanged and search still refuses; free disk space and retry", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE session_search_state SET needs_rebuild = 0 WHERE id = 1`, nil); err != nil {
		return fmt.Errorf("store: clear the search rebuild flag after the rebuild: %w; the index was rebuilt but search still refuses; retry the flag clear", err)
	}
	return nil
}

// SearchRefusalError is the actionable refusal search reads return while the
// rebuild flag is set: it names what failed, why, where, and the fix.
func SearchRefusalError() error {
	return fmt.Errorf("store: search refuses while the search index needs a rebuild (a delete used untrustworthy values, so stale postings may remain): run `peasant harvest verify --content` to inspect the index and rebuild it; after the rebuild search is trusted again")
}

// UnifiedSearchHit is one consolidated-index hit row: the session and entry
// coordinates plus the display columns the search payload shapes.
type UnifiedSearchHit struct {
	SessionID   string
	EntryIndex  int
	Role        string
	Project     string
	ProjectHash string
	Snippet     string
	Rank        float64
	Harness     string
	GitBranch   string
	GitRemote   string
	ProjectName string
	GitWorktree string
}

// unifiedSearchSQLTemplate is the consolidated transcript search over the
// one index, with the rowid-space split as parameters: below the body base
// a hit joins the mirror row, at or above it joins the body row plus the
// active-generation main-partition mapping row by digest. The DDL keeps its
// own literal (SQLite cannot reference a Go constant); every Go use builds
// from BodyRowIDBase below, so the constant stays the one home.
const unifiedSearchSQLTemplate = `SELECT
    f.session_id,
    f.entry_index,
    COALESCE(e.role, b.role) AS role,
    COALESCE(p.canonical_cwd, s.project_hash, '') AS project,
    s.project_hash,
    snippet(session_search_fts, -1, '[', ']', '…', 12) AS snippet,
    bm25(session_search_fts) AS rank,
    s.model_harness,
    COALESCE(s.git_branch, ''),
    COALESCE(p.canonical_remote, ''),
    COALESCE(p.canonical_cwd, ''),
    COALESCE(s.git_worktree, '')
FROM session_search_fts f
LEFT JOIN session_entries e ON e.rowid = f.rowid AND f.rowid < %d
LEFT JOIN session_entry_bodies b ON b.body_id = f.rowid AND f.rowid >= %d
JOIN sessions s        ON s.session_id = f.session_id
LEFT JOIN projects p   ON p.project_hash = s.project_hash
WHERE session_search_fts MATCH ?
  AND (
    (
      f.rowid < %d AND e.session_id IS NOT NULL
      AND COALESCE(e.part_type, '') <> 'pi.carrier'
      AND NOT EXISTS (
        SELECT 1 FROM sessions s2
        JOIN session_generations g ON g.session_id = s2.session_id AND g.generation_id = s2.active_generation_id
        WHERE s2.session_id = e.session_id
      )
      AND NOT (
        e.depth > 0
        AND e.content_preview IS NOT NULL
        AND e.content_preview = (
          SELECT pe.content_preview FROM session_entries pe
          WHERE pe.session_id = e.session_id AND pe.entry_index = e.parent_index
        )
      )
    )
    OR
    (
      f.rowid >= %d AND b.session_id IS NOT NULL
      AND COALESCE(b.part_type, '') <> 'pi.carrier'
      AND EXISTS (
        SELECT 1 FROM sessions s2
        JOIN session_generations g ON g.session_id = s2.session_id AND g.generation_id = s2.active_generation_id
        JOIN session_generation_entries m ON m.session_id = b.session_id AND m.generation_id = g.generation_id AND m.partition_id = 0 AND m.body_digest = b.body_digest
        WHERE s2.session_id = b.session_id
      )
      AND NOT (
        b.depth > 0
        AND b.content_preview IS NOT NULL
        AND b.parent_index IS NOT NULL
        AND b.content_preview = (
          SELECT pb.content_preview
          FROM session_generation_entries pm
          JOIN session_entry_bodies pb ON pb.session_id = pm.session_id AND pb.body_digest = pm.body_digest
          WHERE pm.session_id = b.session_id
            AND pm.generation_id = (SELECT active_generation_id FROM sessions WHERE session_id = b.session_id)
            AND pm.partition_id = 0
            AND pm.entry_index = b.parent_index
        )
      )
    )
  )
ORDER BY rank, f.session_id, f.entry_index
LIMIT ? OFFSET ?`

// UnifiedSearchSQL is the consolidated transcript search over the one index,
// built from BodyRowIDBase: a body hit counts only through the digest join,
// so staged bodies, superseded-only bodies, and earlier-partition bodies
// never return. The depth-echo de-duplication compares full stored previews,
// and the mirror branch carries the representation filter so a converted
// session's leftover mirror rows never duplicate a body hit.
var UnifiedSearchSQL = fmt.Sprintf(
	unifiedSearchSQLTemplate,
	BodyRowIDBase, BodyRowIDBase, BodyRowIDBase, BodyRowIDBase,
)

// SearchUnifiedOnConn runs the consolidated-index query on the caller's
// connection. The caller checks the rebuild flag first: while it is set
// search refuses instead of serving postings that may be stale.
func SearchUnifiedOnConn(conn *sqlite.Conn, match string, limit, offset int) ([]UnifiedSearchHit, error) {
	var hits []UnifiedSearchHit
	if err := sqlitex.ExecuteTransient(conn, UnifiedSearchSQL, &sqlitex.ExecOptions{
		Args: []any{match, limit, offset},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			hits = append(hits, UnifiedSearchHit{
				SessionID:   stmt.ColumnText(0),
				EntryIndex:  int(stmt.ColumnInt64(1)),
				Role:        stmt.ColumnText(2),
				Project:     stmt.ColumnText(3),
				ProjectHash: stmt.ColumnText(4),
				Snippet:     stmt.ColumnText(5),
				Rank:        stmt.ColumnFloat(6),
				Harness:     stmt.ColumnText(7),
				GitBranch:   stmt.ColumnText(8),
				GitRemote:   stmt.ColumnText(9),
				ProjectName: stmt.ColumnText(10),
				GitWorktree: stmt.ColumnText(11),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: search the consolidated index for %q at offset %d with limit %d: %w", match, offset, limit, err)
	}
	return hits, nil
}

// LegacyMirrorSearchSQL is the transition-window mirror query over the
// legacy index. It carries the representation filter, so a converted
// session's leftover mirror rows never return beside the body hit.
const LegacyMirrorSearchSQL = `SELECT
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
    COALESCE(p.canonical_cwd, ''),
    COALESCE(s.git_worktree, '')
FROM session_entries_fts f
JOIN session_entries e ON e.rowid = f.rowid
JOIN sessions s        ON s.session_id = f.session_id
LEFT JOIN projects p   ON p.project_hash = s.project_hash
WHERE session_entries_fts MATCH ?
  AND COALESCE(e.part_type, '') <> 'pi.carrier'
  AND NOT EXISTS (
    SELECT 1 FROM sessions s2
    JOIN session_generations g ON g.session_id = s2.session_id AND g.generation_id = s2.active_generation_id
    WHERE s2.session_id = e.session_id
  )
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

// searchTableExists reports whether a table exists on the caller's connection.
func searchTableExists(conn *sqlite.Conn, table string) bool {
	found := false
	_ = sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, &sqlitex.ExecOptions{
		Args:       []any{table},
		ResultFunc: func(*sqlite.Stmt) error { found = true; return nil },
	})
	return found
}

// LegacySearchIndexExists reports whether the transition-window legacy index
// still exists.
func LegacySearchIndexExists(conn *sqlite.Conn) bool {
	return searchTableExists(conn, "session_entries_fts")
}

// UnifiedSearchIndexExists reports whether the consolidated index exists.
func UnifiedSearchIndexExists(conn *sqlite.Conn) bool {
	return searchTableExists(conn, "session_search_fts")
}

// SearchMergedOnConn runs the production transition-window search on the
// caller's connection: both indexes when both exist, otherwise whichever
// exists. Both sides are read with the full window so the merged pagination
// never skips a row that ranks past one side's bound. Pairs deduplicate by
// session and entry, with the unified body hit winning.
func SearchMergedOnConn(conn *sqlite.Conn, match string, limit, offset int) ([]UnifiedSearchHit, error) {
	legacy := LegacySearchIndexExists(conn)
	unified := UnifiedSearchIndexExists(conn)
	switch {
	case legacy && unified:
		window := limit + offset
		legacyHits, err := searchLegacyOnConn(conn, match, window, 0)
		if err != nil {
			return nil, err
		}
		unifiedHits, err := SearchUnifiedOnConn(conn, match, window, 0)
		if err != nil {
			return nil, err
		}
		merged := append(append([]UnifiedSearchHit(nil), legacyHits...), unifiedHits...)
		sortUnifiedHits(merged)
		seen := make(map[string]struct{}, len(merged))
		deduped := merged[:0]
		for _, hit := range merged {
			key := hit.SessionID + "\x00" + fmt.Sprintf("%d", hit.EntryIndex)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			deduped = append(deduped, hit)
		}
		if offset >= len(deduped) {
			return nil, nil
		}
		end := offset + limit
		if end > len(deduped) {
			end = len(deduped)
		}
		return append([]UnifiedSearchHit(nil), deduped[offset:end]...), nil
	case unified:
		return SearchUnifiedOnConn(conn, match, limit, offset)
	case legacy:
		return searchLegacyOnConn(conn, match, limit, offset)
	default:
		return nil, fmt.Errorf("store: search %q at offset %d with limit %d: no search index exists; run `peasant harvest index` to build one", match, offset, limit)
	}
}

func searchLegacyOnConn(conn *sqlite.Conn, match string, limit, offset int) ([]UnifiedSearchHit, error) {
	var hits []UnifiedSearchHit
	if err := sqlitex.ExecuteTransient(conn, LegacyMirrorSearchSQL, &sqlitex.ExecOptions{
		Args: []any{match, limit, offset},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			hits = append(hits, UnifiedSearchHit{
				SessionID:   stmt.ColumnText(0),
				EntryIndex:  int(stmt.ColumnInt64(1)),
				Role:        stmt.ColumnText(2),
				Project:     stmt.ColumnText(3),
				ProjectHash: stmt.ColumnText(4),
				Snippet:     stmt.ColumnText(5),
				Rank:        stmt.ColumnFloat(6),
				Harness:     stmt.ColumnText(7),
				GitBranch:   stmt.ColumnText(8),
				GitRemote:   stmt.ColumnText(9),
				ProjectName: stmt.ColumnText(10),
				GitWorktree: stmt.ColumnText(11),
			})
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: search the legacy mirror index for %q at offset %d with limit %d: %w", match, offset, limit, err)
	}
	return hits, nil
}

func sortUnifiedHits(hits []UnifiedSearchHit) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Rank != hits[j].Rank {
			return hits[i].Rank < hits[j].Rank
		}
		if hits[i].SessionID != hits[j].SessionID {
			return hits[i].SessionID < hits[j].SessionID
		}
		return hits[i].EntryIndex < hits[j].EntryIndex
	})
}

// CheckSessionEntriesRowidCeiling enforces the disjoint rowid spaces: every
// session_entries rowid must stay below BodyRowIDBase, the FTS rowid base for
// bodies. It refuses with an actionable error when the ceiling is reached.
func (s *Store) CheckSessionEntriesRowidCeiling(ctx context.Context) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to check the rowid-space ceiling: %w", err)
	}
	defer s.pool.Put(conn)
	return checkSessionEntriesRowidCeilingOnConn(conn)
}

func checkSessionEntriesRowidCeilingOnConn(conn *sqlite.Conn) error {
	var maxRowid int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT coalesce(max(rowid), 0) FROM session_entries`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			maxRowid = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		return fmt.Errorf("store: read max(session_entries.rowid) for the rowid-space guard: %w", err)
	}
	if maxRowid >= BodyRowIDBase {
		return fmt.Errorf("store: max(session_entries.rowid) = %d reaches the body rowid base %d: the two FTS rowid spaces would meet and union hits could collide; nothing was indexed; re-allocate the store before writing more mirror rows", maxRowid, int64(BodyRowIDBase))
	}
	return nil
}

// SearchSizeReport is the harvest verify --content size-ratio report: the
// index bytes, the indexed text bytes, the live document count, the
// fresh-build estimate, the ratio, and the needs-optimize state.
type SearchSizeReport struct {
	IndexBytes         int64
	TextBytes          int64
	LiveDocs           int64
	FreshEstimateBytes float64
	Ratio              float64
	NeedsOptimize      bool
}

// SearchSizeReport measures the ratio of the index's page count to its live
// documents' fresh-build estimate and reports the needs-optimize state when
// the ratio exceeds SearchNeedsOptimizeRatio.
func (s *Store) SearchSizeReport(ctx context.Context) (SearchSizeReport, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return SearchSizeReport{}, fmt.Errorf("store: take connection to measure the search index size: %w", err)
	}
	defer s.pool.Put(conn)
	return searchSizeReportOnConn(conn)
}

func searchSizeReportOnConn(conn *sqlite.Conn) (SearchSizeReport, error) {
	var report SearchSizeReport
	if err := sqlitex.ExecuteTransient(conn, `SELECT coalesce(sum(pgsize), 0) FROM dbstat WHERE name LIKE 'session_search_fts%'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			report.IndexBytes = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		return SearchSizeReport{}, fmt.Errorf("store: measure the search index footprint from dbstat: %w; run `peasant harvest verify --content` again after freeing disk space", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(*), coalesce(sum(coalesce(length(content_preview), 0) + coalesce(length(tool_input), 0) + coalesce(length(tool_output), 0)), 0) FROM session_search_source`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			report.LiveDocs = stmt.ColumnInt64(0)
			report.TextBytes = stmt.ColumnInt64(1)
			return nil
		},
	}); err != nil {
		return SearchSizeReport{}, fmt.Errorf("store: measure the indexed text over the union view: %w", err)
	}
	if report.TextBytes <= 0 {
		report.FreshEstimateBytes = 0
		report.Ratio = 1
		report.NeedsOptimize = false
		return report, nil
	}
	report.FreshEstimateBytes = searchFreshBuildFactor * float64(report.TextBytes)
	if report.FreshEstimateBytes <= 0 {
		report.Ratio = 1
		report.NeedsOptimize = false
		return report, nil
	}
	report.Ratio = float64(report.IndexBytes) / report.FreshEstimateBytes
	report.NeedsOptimize = report.Ratio > SearchNeedsOptimizeRatio
	return report, nil
}

// verifyBodiesForDeleteOnConn verifies every body row of the given sessions
// before any delete: a row whose serialization fails to hash-match counts as
// untrusted. On the first mismatch it sets needs_rebuild on the same
// connection and reports true, so the caller deletes anyway and the
// whole-index rebuild clears the stale postings. A match means the BEFORE
// DELETE trigger un-indexes exactly.
func verifyBodiesForDeleteOnConn(conn *sqlite.Conn, sessionIDs []string) (bool, error) {
	if len(sessionIDs) == 0 {
		return false, nil
	}
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	query := `SELECT session_id, body_digest FROM session_entry_bodies WHERE session_id IN (` + strings.Join(placeholders, `,`) + `)`
	mismatch := false
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if mismatch {
				return nil
			}
			sid, err := schema.NewSessionID(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			_, trusted, err := trustBodyForDelete(conn, sid, stmt.ColumnText(1))
			if err != nil {
				return err
			}
			if !trusted {
				mismatch = true
			}
			return nil
		},
	}); err != nil {
		return false, fmt.Errorf("store: verify %d sessions' bodies before delete: %w; nothing was deleted", len(sessionIDs), err)
	}
	if mismatch {
		if err := SearchStateSetNeedsRebuild(context.Background(), conn); err != nil {
			return true, fmt.Errorf("store: flag the search index for rebuild after an untrusted delete: %w; the row was verified but the flag is not set — retry the delete", err)
		}
	}
	return mismatch, nil
}
