package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// HomeCandidate carries selection/ancestry metadata, never entry or quality payloads.
// The cohort includes sessions without metrics so clone ambiguity is not narrowed
// accidentally to the pushable set.
type HomeCandidate struct {
	Identity  SessionRow
	Pushable  bool
	Published bool
}

func (s *Store) HomeCandidates(ctx context.Context, origin, owner string) ([]HomeCandidate, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Home metadata: acquire store connection: %w; retry the page", err)
	}
	defer s.pool.Put(conn)
	rows := make([]HomeCandidate, 0)
	err = sqlitex.ExecuteTransient(conn, `SELECT s.session_id,s.model_harness,s.project_hash,
COALESCE(p.canonical_cwd,p.project_hash,''),s.start_ms,s.git_branch,s.parent_id,
COALESCE(h.git_remote,''),`+sqlRecordedDirectory+`,m.session_id IS NOT NULL,
r.session_id IS NOT NULL
FROM sessions s LEFT JOIN session_metrics m ON m.session_id=s.session_id
LEFT JOIN projects p ON p.project_hash=s.project_hash
LEFT JOIN host_slugs h ON h.opaque_id=s.opaque_host_id
LEFT JOIN (SELECT DISTINCT session_id FROM session_publications WHERE village_origin=? AND owner_user_id=?) r
ON r.session_id=s.session_id`, &sqlitex.ExecOptions{Args: []any{origin, owner}, ResultFunc: func(stmt *sqlite.Stmt) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		row := SessionRow{SessionID: stmt.ColumnText(0), ModelHarness: stmt.ColumnText(1), ProjectHash: stmt.ColumnText(2), ProjectName: stmt.ColumnText(3), StartMs: stmt.ColumnInt64(4), GitWorktree: stmt.ColumnText(8)}
		branch, parent, remote := stmt.ColumnText(5), stmt.ColumnText(6), stmt.ColumnText(7)
		row.GitBranch, row.ParentID, row.CanonicalRemote = &branch, &parent, &remote
		rows = append(rows, HomeCandidate{Identity: row, Pushable: stmt.ColumnBool(9), Published: origin != "" && owner != "" && stmt.ColumnBool(10)})
		return nil
	}})
	if err != nil {
		return nil, fmt.Errorf("read Home selection metadata: %w; no page returned; inspect the store and retry", err)
	}
	return rows, nil
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
