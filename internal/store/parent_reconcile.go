package store

import (
	"context"
	"fmt"
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
// NULL. When a later harvest stores that logical parent, the cache is healed
// here in one transaction. Only the cache moves: the child's content,
// location, origin, selection and managed metadata are untouched, and an
// update that would close a parent-cache cycle is refused.

// ListUncachedIndependentChildren returns the stored sessions of the given
// harnesses whose parent cache is NULL, ordered by session id. It reads
// identifiers only; the caller resolves each child's logical parent from its
// own managed evidence.
func (s *Store) ListUncachedIndependentChildren(ctx context.Context, harnesses []ingest.Harness) ([]ingest.SessionID, error) {
	if len(harnesses) == 0 {
		return nil, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to list uncached independent children: %w", err)
	}
	defer s.pool.Put(conn)

	seen := make(map[string]struct{}, len(harnesses))
	placeholders := make([]string, 0, len(harnesses))
	args := make([]any, 0, len(harnesses))
	for _, harness := range harnesses {
		name := harness.String()
		if name == "" {
			return nil, fmt.Errorf("store: list uncached independent children: harness name is empty; supply a named harness before enumerating uncached children")
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, name)
	}
	if len(args) == 0 {
		return nil, nil
	}

	query := `SELECT session_id FROM sessions WHERE parent_id IS NULL AND model_harness IN (` + strings.Join(placeholders, ",") + `) ORDER BY session_id`
	var ids []ingest.SessionID
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			id, parseErr := ingest.NewSessionID(stmt.ColumnText(0))
			if parseErr != nil {
				return parseErr
			}
			ids = append(ids, id)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: list uncached independent children: %w", err)
	}
	return ids, nil
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
