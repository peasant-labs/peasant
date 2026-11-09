package store

import (
	"context"
	"fmt"
	"strconv"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// Revision returns the store-wide page revision as the decimal string the wire
// carries. store_state holds exactly one row and the triggers advance it on
// every page-relevant write, so a reader that compares two revisions knows
// whether the page it cached can still answer.
//
// A page request reads it once and reuses that value across the page, the
// counts, and any payload composed in the same response: reading it twice
// would let a concurrent write make one response carry two revisions.
func (s *Store) Revision(ctx context.Context) (string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return "", fmt.Errorf("store: take a connection to read the store revision: %w; no page revision was read; restore database access and retry", err)
	}
	defer s.pool.Put(conn)
	var revision int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT revision FROM store_state WHERE id = 1`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			revision = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		return "", fmt.Errorf("store: read store_state.revision: %w; no page revision was read; run peasant harvest verify and retry", err)
	}
	return strconv.FormatInt(revision, 10), nil
}
