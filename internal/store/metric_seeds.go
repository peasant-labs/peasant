package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.MetricSeedStore = (*Store)(nil)

// GetMetricSeed returns adapter statistics retained with the current metadata.
// Computed metrics are never interpreted as recovered native evidence.
func (s *Store) GetMetricSeed(ctx context.Context, sid ingest.SessionID) (*ingest.StatsInfo, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: read retained metric seed for session %s: %w; retry when database access is available", sid, err)
	}
	defer s.pool.Put(conn)
	return getMetricSeedOnConn(conn, sid)
}

func getMetricSeedOnConn(conn *sqlite.Conn, sid ingest.SessionID) (*ingest.StatsInfo, error) {
	var seed *ingest.StatsInfo
	err := sqlitex.ExecuteTransient(conn, "SELECT metric_seed_json FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) == sqlite.TypeNull {
				return nil
			}
			raw := bytes.TrimSpace([]byte(stmt.ColumnText(0)))
			if len(raw) == 0 || raw[0] != '{' {
				return fmt.Errorf("retained metric seed must be a JSON object; NULL alone represents unknown input")
			}
			seed = &ingest.StatsInfo{}
			return json.Unmarshal(raw, seed)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: decode retained metric seed for session %s before computation: %w; prior metrics were preserved; reconcile valid managed metadata and retry", sid, err)
	}
	return seed, nil
}
