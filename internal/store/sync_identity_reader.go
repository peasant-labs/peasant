package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// ParentIdentityRow describes a pushable session's logical spawned-child parent.
// The identity can name an unstored session; an unresolved child has no identifier.
type ParentIdentityRow struct {
	SessionID        string
	ParentIdentity   string
	ParentUnresolved bool
}

// SyncParentIdentities reads one raw row per pushable session, without changing
// the push readers. It answers through the SAME LogicalParentSQL /
// ParentUnresolvedSQL expressions the parent_state projection carries, so the
// Home page's membership and the row's parent markers cannot disagree: evidence
// in the generation scope is authoritative, and only a session with no evidence
// at all falls back to the ParentUUID snapshot and then the availability cache.
// Evidence rows remain raw: duplicate query results are never collapsed into an
// identity map here.
func (s *Store) SyncParentIdentities(ctx context.Context, sessions []ingest.PushSessionRow) ([]ParentIdentityRow, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.SyncParentIdentities: acquire connection while listing sync parents: %w; no identities returned; retry the sync list", err)
	}
	defer s.pool.Put(conn)
	rows := make([]ParentIdentityRow, 0, len(sessions))
	for start := 0; start < len(sessions); start += indexFormatReadBatchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		selected := sessions[start:min(start+indexFormatReadBatchSize, len(sessions))]
		args := make([]any, len(selected))
		placeholders := make([]string, len(selected))
		for i, session := range selected {
			args[i], placeholders[i] = session.SessionID, "?"
		}
		seen := make(map[string]bool, len(selected))
		query := `SELECT s.session_id, ` + LogicalParentSQL("s") + `, ` + ParentUnresolvedSQL("s") + `
FROM sessions s
JOIN session_metrics m ON m.session_id = s.session_id
WHERE s.session_id IN (` + strings.Join(placeholders, ",") + `)`
		err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			row := ParentIdentityRow{SessionID: stmt.ColumnText(0), ParentIdentity: stmt.ColumnText(1), ParentUnresolved: stmt.ColumnInt(2) != 0}
			rows = append(rows, row)
			seen[row.SessionID] = true
			return nil
		}})
		if err != nil {
			return nil, fmt.Errorf("store.SyncParentIdentities: read parent generation evidence while listing sync sessions: %w; no identities returned; run peasant ingest verify and retry", err)
		}
		for _, session := range selected {
			if !seen[session.SessionID] {
				// A session the push list names but that carries no metrics row
				// is not a stored pushable row, so it has no projection. Its push
				// row's parent cache is the only data left; no stored row exists
				// for the snapshot tier to diverge from.
				rows = append(rows, ParentIdentityRow{SessionID: session.SessionID, ParentIdentity: session.ParentID})
			}
		}
	}
	return rows, nil
}
