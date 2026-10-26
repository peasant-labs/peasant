package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// HomeCandidate carries selection/ancestry metadata, never entry or quality payloads.
// The cohort includes sessions without metrics so clone ambiguity is not narrowed
// accidentally to the pushable set.
type HomeCandidate struct {
	Identity SessionRow
	Pushable bool
}

func (s *Store) HomeIdentityCohort(ctx context.Context) ([]HomeCandidate, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Home metadata: acquire store connection: %w; retry the page", err)
	}
	defer s.pool.Put(conn)
	rows := make([]HomeCandidate, 0)
	err = sqlitex.ExecuteTransient(conn, `SELECT s.session_id,s.model_harness,s.project_hash,
COALESCE(p.canonical_cwd,p.project_hash,''),s.start_ms,s.git_branch,s.parent_id,
COALESCE(h.git_remote,''),`+sqlRecordedDirectory+`,m.session_id IS NOT NULL,COALESCE(s.git_worktree,'')
FROM sessions s LEFT JOIN session_metrics m ON m.session_id=s.session_id
LEFT JOIN projects p ON p.project_hash=s.project_hash
LEFT JOIN host_slugs h ON h.opaque_id=s.opaque_host_id`, &sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := SessionRow{SessionID: stmt.ColumnText(0), ModelHarness: stmt.ColumnText(1), ProjectHash: stmt.ColumnText(2), ProjectName: stmt.ColumnText(3), StartMs: stmt.ColumnInt64(4), GitWorktree: stmt.ColumnText(8), RecordedWorktree: stmt.ColumnText(10)}
		branch, parent, remote := stmt.ColumnText(5), stmt.ColumnText(6), stmt.ColumnText(7)
		row.GitBranch, row.ParentID, row.CanonicalRemote = &branch, &parent, &remote
		rows = append(rows, HomeCandidate{Identity: row, Pushable: stmt.ColumnBool(9)})
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("read Home selection metadata: %w; no page returned; inspect the store and retry", err)
	}
	return rows, nil
}

// SyncPageBoundary is an order position, not a row reference: deleting the row
// does not invalidate traversal through tied timestamps.
type SyncPageBoundary struct {
	StartMs int64
	ID      string
}

type SyncWindowFilter struct {
	ProjectHash string
	Origin      string
	Owner       string
	Publication schema.LocalSyncPublicationFilter
}

type SyncCandidateWindow struct {
	Candidates []SyncPageBoundary
	More       bool
}

// syncCandidateWindowSQL bounds returned metadata, not physical SQLite work.
// Selection and parent authority are deliberately absent from these predicates.
func syncCandidateWindowSQL(after *SyncPageBoundary, maximum int, filter SyncWindowFilter) (string, []any) {
	query := `SELECT s.start_ms,s.session_id FROM sessions s JOIN session_metrics m ON m.session_id=s.session_id`
	args := []any{}
	where := []string{"1=1"}
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
	if after != nil {
		where = append(where, "(s.start_ms < ? OR (s.start_ms = ? AND s.session_id > ?))")
		args = append(args, after.StartMs, after.StartMs, after.ID)
	}
	query += " WHERE " + strings.Join(where, " AND ") + " ORDER BY s.start_ms DESC,s.session_id ASC LIMIT ?"
	return query, append(args, maximum+1)
}

// SyncHomeWindow returns at most maximum candidates and one existence bit from
// a cheap order-key sentinel. It never reads titles, bodies or receipt JSON.
func (s *Store) SyncHomeWindow(ctx context.Context, after *SyncPageBoundary, maximum int, filter SyncWindowFilter) (SyncCandidateWindow, error) {
	window := SyncCandidateWindow{}
	if maximum < 1 || maximum > 400 {
		return window, fmt.Errorf("store.SyncHomeWindow: candidate maximum must be 1..400; no query executed; correct the page budget")
	}
	window.Candidates = make([]SyncPageBoundary, 0, maximum)
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return window, err
	}
	defer s.pool.Put(conn)
	query, args := syncCandidateWindowSQL(after, maximum, filter)
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(window.Candidates) == maximum {
			window.More = true
			return nil
		}
		window.Candidates = append(window.Candidates, SyncPageBoundary{StartMs: stmt.ColumnInt64(0), ID: stmt.ColumnText(1)})
		return nil
	}})
	if err != nil {
		return window, fmt.Errorf("store.SyncHomeWindow: read ordered candidate keys: %w; no page usable; inspect the store and retry", err)
	}
	return window, nil
}

type HomePageRow struct {
	Sync  ingest.PushSessionRow
	Title string
}

// HomePageRows reads sync scalars and stored titles only for the selected page.
func (s *Store) HomePageRows(ctx context.Context, ids []string) ([]HomePageRow, error) {
	rows := make([]HomePageRow, 0, len(ids))
	if len(ids) == 0 {
		return rows, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	args := make([]any, len(ids))
	marks := make([]string, len(ids))
	for i, id := range ids {
		args[i] = id
		marks[i] = "?"
	}
	query := strings.Replace(sqlPushSessionsBase, "    s.session_origin\n", "    s.session_origin, COALESCE(m.title,'')\n", 1) + " WHERE s.session_id IN (" + strings.Join(marks, ",") + ")"
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows = append(rows, HomePageRow{Sync: scanPushSessionRow(stmt), Title: stmt.ColumnText(24)})
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("read Home page sync scalars and titles: %w; no page returned; retry", err)
	}
	return rows, nil
}
