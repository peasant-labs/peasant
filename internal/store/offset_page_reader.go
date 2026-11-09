package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// OffsetPageRead is one ordered read of a root or resolved-child page. IDs holds
// the requested page followed by up to one more page of rows, so a caller can
// fill Items and NextItems from one query without a second round trip. Len is
// bounded by 2*perPage+1 so the server can tell whether a page after NextItems
// exists.
type OffsetPageRead struct {
	IDs []string
}

// offsetPageWhere builds the shared pushed-session predicate for one offset
// page. parentState and resolvedParent are the page's ordered source; the
// publication filter is the same receipt rule the legacy window used.
func offsetPageWhere(filter SyncWindowFilter, parentState, resolvedParent string) (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if parentState != "" {
		where = append(where, "s.parent_state=?")
		args = append(args, parentState)
	}
	if resolvedParent != "" {
		where = append(where, "s.resolved_parent_id=?")
		args = append(args, resolvedParent)
	}
	if filter.Publication == schema.LocalSyncPublicationPublished || filter.Publication == schema.LocalSyncPublicationNotPublished {
		args = append(args, filter.Origin, filter.Owner)
		membership := `(SELECT session_id FROM session_publications WHERE village_origin=? AND owner_user_id=?)`
		if filter.Publication == schema.LocalSyncPublicationPublished {
			where = append(where, "s.session_id IN "+membership)
		} else {
			// The receipt schema makes session_id NOT NULL. NOT IN therefore
			// means exactly no receipt for this account across historical hashes.
			where = append(where, "s.session_id NOT IN "+membership)
		}
	}
	if filter.ProjectHash != "" {
		where = append(where, "s.project_hash=?")
		args = append(args, filter.ProjectHash)
	}
	return strings.Join(where, " AND "), args
}

// offsetPageSQL is the one ordered read behind both the root and the child
// page. The root and child indexes carry the same key order, so the planner
// serves either page from one ordered scan.
func offsetPageSQL(parentState, resolvedParent string, filter SyncWindowFilter, limit, offset int) (string, []any) {
	where, args := offsetPageWhere(filter, parentState, resolvedParent)
	query := `SELECT s.start_ms,s.session_id FROM sessions s JOIN session_metrics m ON m.session_id=s.session_id WHERE ` + where +
		` ORDER BY s.start_ms DESC, s.session_id ASC LIMIT ? OFFSET ?`
	return query, append(args, limit, offset)
}

func readOffsetPage(ctx context.Context, conn *sqlite.Conn, query string, args []any, maximum int) (OffsetPageRead, error) {
	page := OffsetPageRead{IDs: make([]string, 0, maximum)}
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		page.IDs = append(page.IDs, stmt.ColumnText(1))
		return nil
	}})
	return page, err
}

// RootPageWindow reads one page of pushable roots from the partial root index,
// plus the rows of the page after it. The caller supplies the offset, so the
// page number and the page size stay one policy in the API layer.
func (s *Store) RootPageWindow(ctx context.Context, page, perPage int, filter SyncWindowFilter) (OffsetPageRead, error) {
	if page < 1 || perPage < 1 || perPage > 400 {
		return OffsetPageRead{IDs: []string{}}, fmt.Errorf("store.RootPageWindow: page %d and perPage %d are outside the bounded read; no query executed; supply a one-based page and a positive page size", page, perPage)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return OffsetPageRead{IDs: []string{}}, err
	}
	defer s.pool.Put(conn)
	query, args := offsetPageSQL("root", "", filter, 2*perPage+1, (page-1)*perPage)
	window, err := readOffsetPage(ctx, conn, query, args, 2*perPage+1)
	if err != nil {
		return OffsetPageRead{IDs: []string{}}, fmt.Errorf("store.RootPageWindow: read ordered root keys: %w; no page usable; inspect the store and retry", err)
	}
	return window, nil
}

// ChildPageWindow reads one page of resolved children of parentID from the
// child index, plus the rows of the page after it. The state predicate is part
// of the read, so an unresolved child is never served as a resolved one.
func (s *Store) ChildPageWindow(ctx context.Context, parentID string, page, perPage int) (OffsetPageRead, error) {
	if page < 1 || perPage < 1 || perPage > 400 {
		return OffsetPageRead{IDs: []string{}}, fmt.Errorf("store.ChildPageWindow: page %d and perPage %d are outside the bounded read; no query executed; supply a one-based page and a positive page size", page, perPage)
	}
	if parentID == "" {
		return OffsetPageRead{IDs: []string{}}, fmt.Errorf("store.ChildPageWindow: parent ID is empty; no query executed; open a parent from the current Home list and retry")
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return OffsetPageRead{IDs: []string{}}, err
	}
	defer s.pool.Put(conn)
	query, args := offsetPageSQL("resolved", parentID, SyncWindowFilter{Publication: schema.LocalSyncPublicationAll}, 2*perPage+1, (page-1)*perPage)
	window, err := readOffsetPage(ctx, conn, query, args, 2*perPage+1)
	if err != nil {
		return OffsetPageRead{IDs: []string{}}, fmt.Errorf("store.ChildPageWindow: read ordered child keys: %w; no page usable; inspect the store and retry", err)
	}
	return window, nil
}

// RootSessionIDs returns every pushable root under the filter in read order. It
// is the exhaustive traversal the auto-publish count needs, because that
// decision is app-side; the plain page read never calls it.
func (s *Store) RootSessionIDs(ctx context.Context, filter SyncWindowFilter) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	where, args := offsetPageWhere(filter, "root", "")
	query := `SELECT s.start_ms,s.session_id FROM sessions s JOIN session_metrics m ON m.session_id=s.session_id WHERE ` + where +
		` ORDER BY s.start_ms DESC, s.session_id ASC`
	ids := make([]string, 0)
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids = append(ids, stmt.ColumnText(1))
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store.RootSessionIDs: read ordered roots: %w; no page usable; inspect the store and retry", err)
	}
	return ids, nil
}

// AllPushableSessionIDs returns every pushable session ID in read order. The
// auto-publish count needs it because the decision is app-side.
func (s *Store) AllPushableSessionIDs(ctx context.Context, filter SyncWindowFilter) ([]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	where, args := offsetPageWhere(filter, "", "")
	query := `SELECT s.start_ms,s.session_id FROM sessions s JOIN session_metrics m ON m.session_id=s.session_id WHERE ` + where +
		` ORDER BY s.start_ms DESC, s.session_id ASC`
	ids := make([]string, 0)
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ids = append(ids, stmt.ColumnText(1))
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store.AllPushableSessionIDs: read ordered pushable sessions: %w; no count usable; inspect the store and retry", err)
	}
	return ids, nil
}

func countOffsetRows(ctx context.Context, conn *sqlite.Conn, parentState, resolvedParent string, filter SyncWindowFilter) (int, error) {
	where, args := offsetPageWhere(filter, parentState, resolvedParent)
	query := `SELECT COUNT(*) FROM sessions s JOIN session_metrics m ON m.session_id=s.session_id WHERE ` + where
	var total int
	err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		total = stmt.ColumnInt(0)
		return nil
	}})
	return total, err
}

// CountRootSessions counts the pushable roots under the page filter. It is the
// page's total, so the client can render real page numbers.
func (s *Store) CountRootSessions(ctx context.Context, filter SyncWindowFilter) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, err
	}
	defer s.pool.Put(conn)
	total, err := countOffsetRows(ctx, conn, "root", "", filter)
	if err != nil {
		return 0, fmt.Errorf("store.CountRootSessions: count roots: %w; no total usable; inspect the store and retry", err)
	}
	return total, nil
}

// CountPushableSessions counts every pushable session under the same filter,
// independent of the page's root or child scope. It fills totals.sessions.
func (s *Store) CountPushableSessions(ctx context.Context, filter SyncWindowFilter) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, err
	}
	defer s.pool.Put(conn)
	total, err := countOffsetRows(ctx, conn, "", "", filter)
	if err != nil {
		return 0, fmt.Errorf("store.CountPushableSessions: count pushable sessions: %w; no total usable; inspect the store and retry", err)
	}
	return total, nil
}

// CountResolvedChildren counts the resolved children of one parent. It is the
// child page's total.
func (s *Store) CountResolvedChildren(ctx context.Context, parentID string) (int, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, err
	}
	defer s.pool.Put(conn)
	total, err := countOffsetRows(ctx, conn, "resolved", parentID, SyncWindowFilter{Publication: schema.LocalSyncPublicationAll})
	if err != nil {
		return 0, fmt.Errorf("store.CountResolvedChildren: count resolved children of %q: %w; no total usable; inspect the store and retry", parentID, err)
	}
	return total, nil
}

// ResolvedChildCounts is the grouped immediate-child count for the returned
// root IDs. It reads only the resolved child index and never loads child rows.
func (s *Store) ResolvedChildCounts(ctx context.Context, parentIDs []string) (map[string]int, error) {
	counts := make(map[string]int, len(parentIDs))
	if len(parentIDs) == 0 {
		return counts, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	args := make([]any, len(parentIDs))
	marks := make([]string, len(parentIDs))
	for i, id := range parentIDs {
		args[i] = id
		marks[i] = "?"
	}
	query := `SELECT s.resolved_parent_id,COUNT(*) FROM sessions s JOIN session_metrics m ON m.session_id=s.session_id
WHERE s.parent_state='resolved' AND s.resolved_parent_id IN (` + strings.Join(marks, ",") + `) GROUP BY s.resolved_parent_id`
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		counts[stmt.ColumnText(0)] = stmt.ColumnInt(1)
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store.ResolvedChildCounts: count resolved children for the page: %w; no child counts usable; inspect the store and retry", err)
	}
	return counts, nil
}

// RootProjectOption is one metadata-only root project for the Home picker.
type RootProjectOption struct {
	ProjectHash string
	ProjectName string
}

// RootProjectOptions returns the distinct projects of pushable roots. It reads
// the root lane directly, so the picker never materializes the stored cohort.
func (s *Store) RootProjectOptions(ctx context.Context) ([]RootProjectOption, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	query := `SELECT DISTINCT s.project_hash, COALESCE(p.canonical_cwd, s.project_hash, '')
FROM sessions s
JOIN session_metrics m ON m.session_id=s.session_id
LEFT JOIN projects p ON p.project_hash=s.project_hash
WHERE s.parent_state='root'`
	options := make([]RootProjectOption, 0)
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		options = append(options, RootProjectOption{ProjectHash: stmt.ColumnText(0), ProjectName: stmt.ColumnText(1)})
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("store.RootProjectOptions: read root projects: %w; no picker usable; inspect the store and retry", err)
	}
	return options, nil
}

// PushableSessionExists reports whether id names one stored pushable session.
// The child page uses it to refuse an unknown parent without reading the root
// cohort.
func (s *Store) PushableSessionExists(ctx context.Context, id string) (bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return false, err
	}
	defer s.pool.Put(conn)
	found := false
	err = sqlitex.ExecuteTransient(conn, `SELECT 1 FROM sessions s JOIN session_metrics m ON m.session_id=s.session_id WHERE s.session_id=? LIMIT 1`, &sqlitex.ExecOptions{Args: []any{id}, ResultFunc: func(stmt *sqlite.Stmt) error {
		found = true
		return nil
	}})
	if err != nil {
		return false, fmt.Errorf("store.PushableSessionExists: read parent %q: %w; no child page usable; inspect the store and retry", id, err)
	}
	return found, nil
}
