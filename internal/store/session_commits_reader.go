package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// sqlListSessionCommits reads the current commit projection for one session.
//
// The order is deterministic for a display list: rows with a known author time
// first, oldest first, then the rows whose provider recorded no author time,
// and commit hash breaks any remaining tie. A consumer therefore renders the
// same sequence on every read.
const sqlListSessionCommits = `SELECT
    commit_hash, author_name, author_email, message, commit_time, author_time
FROM session_commits
WHERE session_id = ?
ORDER BY author_time IS NULL, author_time, commit_hash`

// ListSessionCommits returns the commits recorded for one session, ordered by
// author time with unknown times last. A session with no recorded commits
// returns an empty slice, not an error.
//
// The attribution columns are nullable because not every provider exposes full
// commit metadata; a NULL reads back as the zero value of its field.
func (s *Store) ListSessionCommits(ctx context.Context, sessionID ingest.SessionID) ([]ingest.CommitInfo, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.ListSessionCommits: take connection for session %s: %w; no commits were read, so retry after the store becomes available", sessionID, err)
	}
	defer s.pool.Put(conn)

	commits := make([]ingest.CommitInfo, 0)
	err = sqlitex.ExecuteTransient(conn, sqlListSessionCommits, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			var commit ingest.CommitInfo
			commit.Hash = stmt.ColumnText(0)
			if stmt.ColumnType(1) != sqlite.TypeNull {
				commit.AuthorName = stmt.ColumnText(1)
			}
			if stmt.ColumnType(2) != sqlite.TypeNull {
				commit.AuthorEmail = stmt.ColumnText(2)
			}
			if stmt.ColumnType(3) != sqlite.TypeNull {
				commit.Message = stmt.ColumnText(3)
			}
			if stmt.ColumnType(4) != sqlite.TypeNull {
				commit.CommitTime = stmt.ColumnInt64(4)
			}
			if stmt.ColumnType(5) != sqlite.TypeNull {
				commit.AuthorTime = stmt.ColumnInt64(5)
			}
			commits = append(commits, commit)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store.ListSessionCommits: read commits for session %s: %w; no commits were returned, so retry after resolving the reported cause", sessionID, err)
	}
	return commits, nil
}
