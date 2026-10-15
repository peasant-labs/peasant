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

const syncParentEvidenceQuery = `SELECT s.session_id,
CASE
 WHEN r.kind IS NOT NULL THEN CASE
   WHEN r.target_state IN ('target_known','target_known_retained') THEN r.target_local_id
   ELSE NULL END
 WHEN x.kind IS NOT NULL THEN NULL
 ELSE NULL
END,
CASE WHEN r.target_state IN ('unknown','conflicting_current_native_evidence') THEN 1 ELSE 0 END,
r.kind IS NOT NULL OR x.kind IS NOT NULL
FROM sessions s
JOIN session_metrics m ON m.session_id = s.session_id
JOIN (SELECT DISTINCT session_id FROM session_relationship_evidence WHERE session_id IN (`

// syncParentEvidenceJoins is the trailing half of the identity evidence read.
// Its two evidence joins splice the shared generation scope, so the identity
// lane consults the same generation the parent answer does: the active
// generation when the session has one, otherwise its latest attempt. A child
// whose only evidence is its latest attempt must not read as a root.
var syncParentEvidenceJoins = `)) e ON e.session_id = s.session_id
LEFT JOIN session_relationship_evidence r
 ON r.session_id = s.session_id AND r.generation_id = ` + parentGenerationScopeSQL("s") + ` AND r.kind = 'started_by'
LEFT JOIN session_relationship_evidence x
 ON x.session_id = s.session_id AND x.generation_id = ` + parentGenerationScopeSQL("s") + ` AND x.kind = 'context_from'`

// SyncParentIdentities reads one raw row per pushable session, without changing
// the push readers. It consumes the sync list's existing base read, scans the
// indexed snapshot channel, and reads sparse evidence in bounded batches.
// Generation evidence (the shared scope: the active generation, or the latest
// attempt when there is none) precedes snapshot and cache. Evidence rows remain
// raw: duplicate query results are never collapsed into an identity map here.
func (s *Store) SyncParentIdentities(ctx context.Context, sessions []ingest.PushSessionRow) ([]ParentIdentityRow, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store.SyncParentIdentities: acquire connection while listing sync parents: %w; no identities returned; retry the sync list", err)
	}
	defer s.pool.Put(conn)
	snapshots := make(map[string]string)
	err = sqlitex.ExecuteTransient(conn, `SELECT session_id,json_extract(metadata_json,'$.parentUuid')
FROM session_publication_metadata WHERE json_extract(metadata_json,'$.parentUuid') IS NOT NULL`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			snapshots[stmt.ColumnText(0)] = stmt.ColumnText(1)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store.SyncParentIdentities: read snapshot parents while listing sync sessions: %w; no identities returned; run peasant ingest verify and retry", err)
	}
	rows := make([]ParentIdentityRow, 0, len(sessions))
	for start := 0; start < len(sessions); start += indexFormatReadBatchSize {
		selected := sessions[start:min(start+indexFormatReadBatchSize, len(sessions))]
		args := make([]any, len(selected))
		placeholders := make([]string, len(selected))
		fallback := make(map[string]string, len(selected))
		for i, session := range selected {
			args[i], placeholders[i] = session.SessionID, "?"
			parent := snapshots[session.SessionID]
			if parent == "" {
				parent = session.ParentID
			}
			fallback[session.SessionID] = parent
		}
		seen := make(map[string]bool)
		err := sqlitex.ExecuteTransient(conn, syncParentEvidenceQuery+strings.Join(placeholders, ",")+syncParentEvidenceJoins, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
			row := ParentIdentityRow{SessionID: stmt.ColumnText(0), ParentIdentity: stmt.ColumnText(1), ParentUnresolved: stmt.ColumnInt(2) != 0}
			// Snapshot/cache fallback is reachable only when BOTH generation
			// evidence arms are silent, never when authority supplied an
			// unnamed parent.
			if stmt.ColumnInt(3) == 0 {
				row.ParentIdentity = fallback[row.SessionID]
			}
			rows = append(rows, row)
			seen[row.SessionID] = true
			return nil
		}})
		if err != nil {
			return nil, fmt.Errorf("store.SyncParentIdentities: read parent generation evidence while listing sync sessions: %w; no identities returned; run peasant ingest verify and retry", err)
		}
		for _, session := range selected {
			if !seen[session.SessionID] {
				rows = append(rows, ParentIdentityRow{SessionID: session.SessionID, ParentIdentity: fallback[session.SessionID]})
			}
		}
	}
	return rows, nil
}
