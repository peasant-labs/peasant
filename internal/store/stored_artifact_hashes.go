package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// StoredArtifactHashes returns sessions.artifact_hash for every session that
// records a saved pair identity: each of these sessions has a metadata and
// transcript pair under the output directory whose identity must match.
func (s *Store) StoredArtifactHashes(ctx context.Context) (map[schema.SessionID]string, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection to read saved pair identities: %w", err)
	}
	defer s.pool.Put(conn)
	hashes := make(map[schema.SessionID]string)
	if err := sqlitex.Execute(conn, `SELECT session_id, artifact_hash FROM sessions WHERE artifact_hash IS NOT NULL`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			id, err := schema.NewSessionID(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			hashes[id] = stmt.ColumnText(1)
			return nil
		},
	}); err != nil {
		return nil, fmt.Errorf("store: read saved pair identities: %w; nothing was written", err)
	}
	return hashes, nil
}
