package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.OrphanParentReconciler = (*Store)(nil)

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
// authority: the active generation's started_by relationship evidence (the V2
// contract), and the session publication metadata snapshot's legacy
// parentUuid (the V1 pair). The result is deduplicated and ordered by child,
// then parent. The query is scoped to the given parents, so a stored
// independent root that does not name one of them is never considered and its
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

	harnessPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(harnessNames)), ",")
	harnessArgs := make([]any, 0, len(harnessNames))
	for _, name := range harnessNames {
		harnessArgs = append(harnessArgs, name)
	}
	targetArgs := append(append([]any{}, harnessArgs...), string(targetsJSON))

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

	legacyEvidence := `SELECT s.session_id, json_extract(p.metadata_json, '$.parentUuid')
FROM sessions s
JOIN session_publication_metadata p ON p.session_id = s.session_id
WHERE s.parent_id IS NULL
  AND s.model_harness IN (` + harnessPlaceholders + `)
  AND json_valid(p.metadata_json)
  AND json_extract(p.metadata_json, '$.parentUuid') IN (SELECT value FROM json_each(?))`
	if err := sqlitex.ExecuteTransient(conn, legacyEvidence, &sqlitex.ExecOptions{
		Args:       targetArgs,
		ResultFunc: collect,
	}); err != nil {
		return nil, fmt.Errorf("store: list uncached children from stored publication metadata: %w", err)
	}

	durableEvidence := `SELECT r.session_id, r.target_local_id
FROM session_relationship_evidence r
JOIN sessions s ON s.session_id = r.session_id AND s.active_generation_id = r.generation_id
WHERE r.kind = 'started_by'
  AND r.target_state IN ('target_known','target_known_retained')
  AND r.target_local_id IS NOT NULL AND r.target_local_id <> ''
  AND s.parent_id IS NULL
  AND s.model_harness IN (` + harnessPlaceholders + `)
  AND r.target_local_id IN (SELECT value FROM json_each(?))`
	if err := sqlitex.ExecuteTransient(conn, durableEvidence, &sqlitex.ExecOptions{
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
		if execErr := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET parent_id = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{string(update.Parent), string(update.Child)},
		}); execErr != nil {
			return fmt.Errorf("store: reconcile parent cache for child %s: %w; no cache update in this reconciliation was committed; retry harvest", update.Child, execErr)
		}
	}
	return nil
}
