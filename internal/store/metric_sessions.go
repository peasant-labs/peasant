package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

var _ ingest.MetricSessionReader = (*Store)(nil)

// buildListMetricSessionsQuery renders one metric-refresh page for the given
// cursor, limit and scope. Production and its query-plan guard call the same
// builder.
func buildListMetricSessionsQuery(after ingest.SessionID, limit int, scope ingest.StoreScope) (string, []any) {
	predicate, narrowedArgs := narrowedSessionPredicate("s", scope)
	args := make([]any, 0, 2+len(narrowedArgs))
	args = append(args, string(after))
	args = append(args, narrowedArgs...)
	args = append(args, limit)
	return `SELECT s.session_id, s.model_harness, s.start_ms FROM sessions s
WHERE s.session_id > ?` + predicate + ` ORDER BY s.session_id LIMIT ?`, args
}

// ListMetricSessions pages by immutable session ID, independently of discovery
// selection. No transcript, native harness or annotation rows are loaded here.
func (s *Store) ListMetricSessions(ctx context.Context, after ingest.SessionID, limit int, scope ingest.StoreScope) ([]ingest.MetricSession, error) {
	if err := scope.Validate("ListMetricSessions"); err != nil {
		return nil, err
	}
	if narrowedSessionsEmpty(scope) {
		return nil, nil
	}
	if limit < 1 || limit > 256 {
		return nil, fmt.Errorf("list stored metric sessions: page limit must be between 1 and 256; request a bounded page")
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	query, args := buildListMetricSessionsQuery(after, limit, scope)
	var sessions []ingest.MetricSession
	err = sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
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
