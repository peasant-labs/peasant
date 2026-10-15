package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

var _ ingest.OrphanParentReconciler = (*Store)(nil)

// parentCacheLegacyLookupTemplate is the legacy half of the reverse lookup: the
// legacy publication metadata snapshot's retained parentUuid. The CROSS JOIN
// keeps the target list as the fixed outer loop, so the query seeks the
// parentUuid expression index by target rather than scanning the harness
// population; it reads only the stored children that name a target. A session
// whose generation carries relationship authority is excluded entirely:
// the legacy snapshot may predate that generation and must never resurrect a
// parent it cleared, marked unknown or conflicting, or pointed elsewhere. The
// generation scope is the one shared spelling, so a session whose only
// generation is the latest attempt is excluded on that attempt's evidence too.
// The trailing %s is the harness placeholder list.
var parentCacheLegacyLookupTemplate = `
SELECT s.session_id, json_extract(p.metadata_json, '$.parentUuid')
FROM json_each(?) AS target
CROSS JOIN session_publication_metadata p
CROSS JOIN sessions s
WHERE json_extract(p.metadata_json, '$.parentUuid') = target.value
  AND s.session_id = p.session_id
  AND json_extract(p.metadata_json, '$.parentUuid') IS NOT NULL
  AND target.value IS NOT NULL
  AND s.parent_id IS NULL
  AND s.model_harness IN (%s)
  AND NOT EXISTS (
    SELECT 1 FROM session_relationship_evidence r
    WHERE r.session_id = s.session_id
      AND r.generation_id = ` + parentGenerationScopeSQL("s") + `
  )`

// parentCacheDurableLookupTemplate is the durable half: the started_by
// relationship evidence of the session's generation (the active generation, or
// the latest attempt when no active generation exists). Its CROSS JOIN likewise
// keeps the target list as the fixed outer loop and seeks the
// relationship-evidence target index. The trailing %s is the harness
// placeholder list.
var parentCacheDurableLookupTemplate = `
SELECT r.session_id, r.target_local_id
FROM json_each(?) AS target
CROSS JOIN session_relationship_evidence r
CROSS JOIN sessions s
WHERE r.kind = 'started_by' AND r.target_local_id = target.value
  AND s.session_id = r.session_id AND r.generation_id = ` + parentGenerationScopeSQL("s") + `
  AND target.value IS NOT NULL
  AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  AND r.target_state IN ('target_known','target_known_retained')
  AND s.parent_id IS NULL
  AND s.model_harness IN (%s)`

// parentCacheLookupSQL renders one reverse-lookup template for the given number
// of harness placeholders. Production and its query-plan guard share it so the
// guarded statement is the statement production runs.
func parentCacheLookupSQL(template string, harnessCount int) string {
	return fmt.Sprintf(template, strings.TrimSuffix(strings.Repeat("?,", harnessCount), ","))
}

// Reverse logical-target cache reconciliation for independently admitted
// children.
//
// An independently admitted child can be stored while its logical parent is
// missing, unselected or otherwise unavailable: the child keeps its managed
// logical ParentUUID while the sessions.parent_id availability cache stays
// NULL. When a later harvest makes that logical parent available, the cache is
// healed here in one transaction. Only the cache moves: the child's content,
// location, origin, selection and managed metadata are untouched, and an
// update that would close a parent-cache cycle is refused.
//
// The reverse lookup is target-scoped: the caller names the parents this
// harvest made available, and the query returns only the stored children whose
// PERSISTED logical-parent evidence names one of those targets. It reads the
// database's durable relationship evidence and publication metadata snapshot,
// never the managed metadata files, so it opens nothing belonging to an
// unrelated stored root.

// ListUncachedChildrenOfParents returns the stored, still-uncached
// independently admitted children whose durable logical-parent evidence names
// one of the given parents. The child side is the FK-availability cache:
// only rows with parent_id IS NULL are returned, because a populated cache is
// authoritative and is never overwritten from this reverse pass.
//
// Durable evidence is read from two persisted sources, in this order of
// authority. The active generation's started_by relationship evidence (the V2
// contract) is authoritative: a session that carries relationship evidence for
// its active generation is excluded from the legacy pass entirely, so a legacy
// publication snapshot can never resurrect a parent the active generation
// cleared (explicit_none), marked unknown or conflicting, or pointed at a
// different target. The publication metadata snapshot's legacy parentUuid (the
// V1 pair) is consulted only for sessions with no active relationship
// evidence. The result is deduplicated and ordered by child, then parent.
//
// Both queries are driven from the named parents: the target list is the outer
// loop and each persisted evidence form is seeked by target, so a stored
// independent root that does not name one of them is never touched and its
// managed metadata is never opened.
func (s *Store) ListUncachedChildrenOfParents(ctx context.Context, parents []ingest.SessionID, harnesses []ingest.Harness) ([]ingest.ParentCacheReconcile, error) {
	if len(parents) == 0 || len(harnesses) == 0 {
		return nil, nil
	}

	harnessNames := make([]string, 0, len(harnesses))
	seenHarness := make(map[string]struct{}, len(harnesses))
	for _, harness := range harnesses {
		name := harness.String()
		if name == "" {
			return nil, fmt.Errorf("store: list uncached children of stored parents: harness name is empty; supply a named harness before enumerating uncached children")
		}
		if _, duplicate := seenHarness[name]; duplicate {
			continue
		}
		seenHarness[name] = struct{}{}
		harnessNames = append(harnessNames, name)
	}
	if len(harnessNames) == 0 {
		return nil, nil
	}

	targets := make([]string, 0, len(parents))
	seenTarget := make(map[string]struct{}, len(parents))
	for _, parent := range parents {
		value := string(parent)
		if value == "" {
			return nil, fmt.Errorf("store: list uncached children of stored parents: parent id is empty; name the sessions this harvest made available before reconciling")
		}
		if _, duplicate := seenTarget[value]; duplicate {
			continue
		}
		seenTarget[value] = struct{}{}
		targets = append(targets, value)
	}
	if len(targets) == 0 {
		return nil, nil
	}
	targetsJSON, err := json.Marshal(targets)
	if err != nil {
		return nil, fmt.Errorf("store: encode stored parent targets for cache reconciliation: %w", err)
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to list uncached children of stored parents: %w", err)
	}
	defer s.pool.Put(conn)

	harnessArgs := make([]any, 0, len(harnessNames))
	for _, name := range harnessNames {
		harnessArgs = append(harnessArgs, name)
	}
	// The target list is the driving argument: json_each(?) is the first
	// placeholder in both templates, so it binds before the harness names.
	targetArgs := append([]any{string(targetsJSON)}, harnessArgs...)

	pairs := make(map[ingest.SessionID]ingest.SessionID)
	collect := func(stmt *sqlite.Stmt) error {
		child, parseErr := ingest.NewSessionID(stmt.ColumnText(0))
		if parseErr != nil {
			return parseErr
		}
		parent, parseErr := ingest.NewSessionID(stmt.ColumnText(1))
		if parseErr != nil {
			return parseErr
		}
		pairs[child] = parent
		return nil
	}

	if err := sqlitex.ExecuteTransient(conn, parentCacheLookupSQL(parentCacheLegacyLookupTemplate, len(harnessNames)), &sqlitex.ExecOptions{
		Args:       targetArgs,
		ResultFunc: collect,
	}); err != nil {
		return nil, fmt.Errorf("store: list uncached children from stored publication metadata: %w", err)
	}

	if err := sqlitex.ExecuteTransient(conn, parentCacheLookupSQL(parentCacheDurableLookupTemplate, len(harnessNames)), &sqlitex.ExecOptions{
		Args:       targetArgs,
		ResultFunc: collect,
	}); err != nil {
		return nil, fmt.Errorf("store: list uncached children from stored relationship evidence: %w", err)
	}

	if len(pairs) == 0 {
		return nil, nil
	}
	updates := make([]ingest.ParentCacheReconcile, 0, len(pairs))
	for child, parent := range pairs {
		updates = append(updates, ingest.ParentCacheReconcile{Child: child, Parent: parent})
	}
	sort.Slice(updates, func(i, j int) bool {
		if updates[i].Child != updates[j].Child {
			return updates[i].Child < updates[j].Child
		}
		return updates[i].Parent < updates[j].Parent
	})
	return updates, nil
}

// sqlInvalidateStaleParentOrigins re-queues every settled, parent-linked,
// independently admitted row whose verdict is still unknown at the current
// origin rule version. Its parent test is the session-level logical-parent
// answer, rendered through LogicalParentSQL, not the raw availability cache: a
// child whose generation carries started_by evidence is parent-linked even
// while its cache is NULL, so the re-queue set is exactly the set the resolve
// pass's own work list (sqlListStaleOriginSessions) treats as parent-linked.
// The predicate is whole-population and idempotent: once a row is reset to
// version 0 it no longer matches, and the resolve pass moves it back to the
// rule version with its parent-link verdict.
var sqlInvalidateStaleParentOrigins = `UPDATE sessions SET origin_version = 0
WHERE session_origin = 'unknown'
  AND COALESCE(` + LogicalParentSQL("sessions") + `, '') <> ''
  AND origin_version = ?
  AND model_harness IN (?, ?)`

// InvalidateStaleParentOrigins re-queues the stored codex and opencode rows that
// carry a parent link but kept an unknown origin verdict at the current rule
// version — verdicts judged before their parent link was repaired. It resets
// only origin_version, never the verdict, the link, or any other column, and
// returns the number of rows re-queued; a second application returns zero.
// Claude Code rows and parentless roots are never touched.
func (s *Store) InvalidateStaleParentOrigins(ctx context.Context) (int64, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: take connection to re-queue stale parent-linked origin verdicts: %w; no origin verdict was re-queued; restore database access and rerun harvest", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, sqlInvalidateStaleParentOrigins, &sqlitex.ExecOptions{
		Args: []any{ingest.OriginRuleVersion, ingest.HarnessCodex.String(), ingest.HarnessOpenCode.String()},
	}); err != nil {
		return 0, fmt.Errorf("store: re-queue stale parent-linked origin verdicts at rule version %d: %w; no origin verdict was re-queued; restore database access and rerun harvest", ingest.OriginRuleVersion, err)
	}
	return int64(conn.Changes()), nil
}

// ReconcileParentCache atomically applies a bounded set of child-to-parent
// cache updates. Each update is applied only when the child and parent rows
// exist, the edge is not a self-parent, and following the parent cache from
// the target parent cannot reach the child. All applied updates commit
// together; a failure rolls the whole reconciliation back.
func (s *Store) ReconcileParentCache(ctx context.Context, updates []ingest.ParentCacheReconcile) (err error) {
	if len(updates) == 0 {
		return nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection to reconcile the independent child parent cache: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	// effective is the parent cache as this transaction will leave it; exists
	// records whether the session row is present. Both are memoized so a chain
	// walk does not repeat a query.
	effective := make(map[ingest.SessionID]*ingest.SessionID)
	exists := make(map[ingest.SessionID]bool)
	load := func(id ingest.SessionID) (*ingest.SessionID, bool, error) {
		if parent, ok := effective[id]; ok {
			return parent, exists[id], nil
		}
		var parent *ingest.SessionID
		found := false
		if loadErr := sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(id)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				if stmt.ColumnType(0) == sqlite.TypeNull {
					return nil
				}
				value, parseErr := ingest.NewSessionID(stmt.ColumnText(0))
				if parseErr != nil {
					return parseErr
				}
				parent = &value
				return nil
			},
		}); loadErr != nil {
			return nil, false, loadErr
		}
		effective[id] = parent
		exists[id] = found
		return parent, found, nil
	}
	wouldCycle := func(parent, child ingest.SessionID) (bool, error) {
		visited := map[ingest.SessionID]struct{}{parent: {}}
		cursor := parent
		for {
			next, found, loadErr := load(cursor)
			if loadErr != nil {
				return false, loadErr
			}
			if !found || next == nil {
				return false, nil
			}
			if *next == child {
				return true, nil
			}
			if _, seen := visited[*next]; seen {
				return false, nil
			}
			visited[*next] = struct{}{}
			cursor = *next
		}
	}

	for _, update := range updates {
		if update.Child == update.Parent {
			continue
		}
		childParent, childExists, loadErr := load(update.Child)
		if loadErr != nil {
			return fmt.Errorf("store: reconcile parent cache for child %s: %w; the availability cache was left unchanged; retry harvest", update.Child, loadErr)
		}
		if !childExists {
			continue
		}
		if childParent != nil {
			// An existing cache is authoritative: only a nil availability
			// cache is healed. A populated cache that disagrees is never
			// overwritten from this reverse pass.
			continue
		}
		if _, parentExists, loadErr := load(update.Parent); loadErr != nil {
			return fmt.Errorf("store: reconcile parent cache for child %s: %w; the availability cache was left unchanged; retry harvest", update.Child, loadErr)
		} else if !parentExists {
			continue // the parent row is not committed; leave the child an orphan
		}
		cyclic, cycleErr := wouldCycle(update.Parent, update.Child)
		if cycleErr != nil {
			return fmt.Errorf("store: reconcile parent cache for child %s: %w; the availability cache was left unchanged; retry harvest", update.Child, cycleErr)
		}
		if cyclic {
			continue
		}
		parent := update.Parent
		effective[update.Child] = &parent
		// The heal and the origin requeue move in ONE statement: a child can
		// never be found with a repaired parent link still carrying the origin
		// verdict judged before the link existed, so the next resolve pass
		// re-judges it. writeParentCacheOnConn is that statement.
		if execErr := writeParentCacheOnConn(conn, update.Child, &parent); execErr != nil {
			return fmt.Errorf("store: reconcile parent cache for child %s: %w; no cache update in this reconciliation was committed; retry harvest", update.Child, execErr)
		}
	}
	return nil
}
