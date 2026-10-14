package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

var _ ingest.MetricSeedStore = (*Store)(nil)

// GetMetricSeed returns adapter statistics retained with the current
// metadata: the harness-only seed home for native sessions, the legacy
// sessions column otherwise. Computed metrics are never interpreted as
// recovered native evidence.
func (s *Store) GetMetricSeed(ctx context.Context, sid ingest.SessionID) (*ingest.StatsInfo, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read retained metric seed for session %s: %w; retry when database access is available", sid, err)
	}
	defer s.pool.Put(conn)
	return readMetricSeedOnConn(conn, schema.SessionID(sid))
}

func getMetricSeedOnConn(conn *sqlite.Conn, sid ingest.SessionID) (*ingest.StatsInfo, error) {
	var seed *ingest.StatsInfo
	err := sqlitex.ExecuteTransient(conn, "SELECT metric_seed_json FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) == sqlite.TypeNull {
				return nil
			}
			return decodeSeedDocument(stmt.ColumnText(0), &seed)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: decode retained metric seed for session %s before computation: %w; prior metrics were preserved; reconcile valid managed metadata and retry", sid, err)
	}
	return seed, nil
}
