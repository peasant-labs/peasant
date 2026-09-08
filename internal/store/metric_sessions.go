package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.MetricSessionReader = (*Store)(nil)

// ListMetricSessions pages by immutable session ID, independently of discovery
// selection. No transcript, native harness or annotation rows are loaded here.
func (s *Store) ListMetricSessions(ctx context.Context, after ingest.SessionID, limit int) ([]ingest.MetricSession, error) {
	if limit < 1 || limit > 256 {
		return nil, fmt.Errorf("list stored metric sessions: page limit must be between 1 and 256; request a bounded page")
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	var sessions []ingest.MetricSession
	err = sqlitex.ExecuteTransient(conn, `SELECT session_id, model_harness, start_ms FROM sessions
WHERE session_id > ? ORDER BY session_id LIMIT ?`, &sqlitex.ExecOptions{
		Args: []any{string(after), limit}, ResultFunc: func(stmt *sqlite.Stmt) error {
			sid, err := ingest.NewSessionID(stmt.ColumnText(0))
			if err != nil {
				return err
			}
			var harness ingest.Harness
			for _, known := range ingest.AllHarnesses {
				if string(known) == stmt.ColumnText(1) {
					harness = known
					break
				}
			}
			if harness == "" {
				return fmt.Errorf("list stored metric session %s: unrecognized harness %q; upgrade Peasant before retrying", sid, stmt.ColumnText(1))
			}
			sessions = append(sessions, ingest.MetricSession{SessionID: sid, Harness: harness, StartMS: stmt.ColumnInt64(2)})
			return nil
		},
	})
	return sessions, err
}
