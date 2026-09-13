package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// sessionsWithoutEntriesChunkSize bounds one entries-membership IN-list below
// the SQLite variable limit, the same conservative ceiling the classifier
// prefetch uses. The failed set behind a forced rebuild can name thousands of
// sessions, so the membership read walks it in chunks of this size rather than
// binding it all at once.
const sessionsWithoutEntriesChunkSize = 900

var _ ingest.IndexCoverageReader = (*Store)(nil)

// SessionsWithoutEntries reports which of the given sessions hold no
// session_entries rows. Every distinct requested session is present in the
// answer: true means no entries rows exist for it, false means at least one
// does. Sessions never requested are never present.
//
// The read is one SELECT DISTINCT per chunk over the requested IDs: no
// per-session savepoint, no per-session round trip. An empty request answers
// empty without touching a connection.
func (s *Store) SessionsWithoutEntries(ctx context.Context, sessionIDs []ingest.SessionID) (map[ingest.SessionID]bool, error) {
	without := make(map[ingest.SessionID]bool, len(sessionIDs))
	if len(sessionIDs) == 0 {
		return without, nil
	}
	distinct := make([]ingest.SessionID, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		if _, seen := without[id]; seen {
			continue
		}
		without[id] = true
		distinct = append(distinct, id)
	}

	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: SessionsWithoutEntries: take connection for %d session(s): %w; no membership was reported; check the analytics store is open and retry", len(distinct), err)
	}
	defer s.pool.Put(conn)

	for start := 0; start < len(distinct); start += sessionsWithoutEntriesChunkSize {
		end := start + sessionsWithoutEntriesChunkSize
		if end > len(distinct) {
			end = len(distinct)
		}
		chunk := distinct[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = string(id)
		}
		query := `SELECT DISTINCT session_id FROM session_entries WHERE session_id IN (` + strings.Join(placeholders, ",") + `)`
		if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
			Args: args,
			ResultFunc: func(stmt *sqlite.Stmt) error {
				without[ingest.SessionID(stmt.ColumnText(0))] = false
				return nil
			},
		}); err != nil {
			return nil, fmt.Errorf("store: SessionsWithoutEntries: read entries membership for sessions %d-%d of %d: %w; no membership was reported; re-run the harvest, and if the failure persists check the analytics store is readable",
				start, end, len(distinct), err)
		}
	}
	return without, nil
}
