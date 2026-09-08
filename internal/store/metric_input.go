package store

import (
	"context"
	"fmt"
	"reflect"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

var _ ingest.MetricInputStore = (*Store)(nil)

func (s *Store) ReadMetricInput(ctx context.Context, sid ingest.SessionID, includeModels bool) (_ *ingest.MetricInput, retErr error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, err
	}
	defer s.pool.Put(conn)
	end := sqlitex.Save(conn)
	defer end(&retErr)
	return s.readMetricInputOnConn(conn, sid, includeModels)
}

func (s *Store) readMetricInputOnConn(conn *sqlite.Conn, sid ingest.SessionID, includeModels bool) (*ingest.MetricInput, error) {
	input := &ingest.MetricInput{SessionID: sid, StoredModels: includeModels}
	var err error
	input.IndexState, err = readIndexStateOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	if input.IndexState == nil {
		return nil, fmt.Errorf("read metric input for session %s: session is not stored; no metrics changed; harvest the session before computing", sid)
	}
	input.Entries, err = s.listEntriesOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	if len(input.Entries) == 0 && (input.IndexState.IndexedAt == nil || input.IndexState.IndexerVersion <= 0 || input.IndexState.IndexVersion == nil || input.IndexState.SessionEntriesHash == nil) {
		return nil, fmt.Errorf("read metric input for session %s: empty entries have no completed index; prior metrics were preserved; index the session before retrying", sid)
	}
	input.Seed, err = getMetricSeedOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	input.Harness, input.ProjectPath, err = getTitleContextOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	err = sqlitex.ExecuteTransient(conn, "SELECT start_ms, end_ms FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
			input.StartMS, input.EndMS = stmt.ColumnInt64(0), stmt.ColumnInt64(1)
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	input.Existing, err = getMetricsOnConn(conn, sid)
	if err != nil {
		return nil, err
	}
	if includeModels {
		input.Model, err = metricModelOnConn(conn, ingest.MetricModelID(input.Entries))
		if err != nil {
			return nil, err
		}
	}
	input.DatabaseHash, err = input.Hash()
	return input, err
}

func metricModelOnConn(conn *sqlite.Conn, modelID string) (*ingest.MetricModel, error) {
	if modelID == "" {
		return nil, nil
	}
	var model *ingest.MetricModel
	err := sqlitex.ExecuteTransient(conn, `SELECT model_id, provider_key, context_window,
cost_input_per_mtok, cost_output_per_mtok, cost_reasoning_per_mtok,
cost_cache_read_per_mtok, cost_cache_write_per_mtok FROM models WHERE model_id = ?
ORDER BY CASE provider_key WHEN '' THEN 0 WHEN 'anthropic' THEN 1 WHEN 'openai' THEN 2 WHEN 'google' THEN 3 ELSE 4 END, provider_key
LIMIT 1`, &sqlitex.ExecOptions{Args: []any{modelID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		model = &ingest.MetricModel{ModelID: stmt.ColumnText(0), ProviderKey: stmt.ColumnText(1)}
		if stmt.ColumnType(2) != sqlite.TypeNull {
			value := stmt.ColumnInt(2)
			model.ContextWindow = &value
		}
		for index, field := range []**float64{&model.CostInputPerMTok, &model.CostOutputPerMTok, &model.CostReasoningPerMTok, &model.CostCacheReadPerMTok, &model.CostCacheWritePerMTok} {
			if stmt.ColumnType(index+3) != sqlite.TypeNull {
				value := stmt.ColumnFloat(index + 3)
				*field = &value
			}
		}
		return nil
	}})
	return model, err
}

// SaveMetricsForInput validates captured database inputs and prior output inside
// the same write transaction as the new values, algorithm and hashes.
func (s *Store) SaveMetricsForInput(ctx context.Context, expected *ingest.MetricInput, metrics *ingest.SessionMetrics) (retErr error) {
	if expected == nil || metrics == nil || expected.SessionID != metrics.SessionID || metrics.ComputeVersion == nil || *metrics.ComputeVersion < 1 || metrics.ComputedAt == nil || metrics.InputHash == nil || metrics.OutputHash == nil {
		return fmt.Errorf("save computed metrics: missing captured input or completion evidence; no values changed; capture and compute the session again")
	}
	inputHash, err := expected.Hash()
	if err != nil || inputHash != expected.DatabaseHash {
		return fmt.Errorf("save metrics for session %s: captured database input was modified; no values changed; capture and compute the session again", metrics.SessionID)
	}
	outputHash, err := ingest.MetricOutputHash(metrics)
	if err != nil || outputHash != *metrics.OutputHash {
		return fmt.Errorf("save metrics for session %s: output differs from its computation hash; no values changed; recompute before retrying", metrics.SessionID)
	}
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return err
	}
	defer s.pool.Put(conn)
	end, err := sqlitex.ImmediateTransaction(conn)
	if err != nil {
		return err
	}
	defer end(&retErr)
	current, err := s.readMetricInputOnConn(conn, expected.SessionID, expected.StoredModels)
	if err != nil {
		return err
	}
	if current.Existing != nil && current.Existing.ComputeVersion != nil && *current.Existing.ComputeVersion > *metrics.ComputeVersion {
		return fmt.Errorf("save metrics for session %s: stored compute version is newer; prior values and producer were preserved; upgrade Peasant before retrying", metrics.SessionID)
	}
	if current.DatabaseHash != expected.DatabaseHash || !reflect.DeepEqual(current.Existing, expected.Existing) {
		return fmt.Errorf("save metrics for session %s: captured inputs or prior result changed during computation; current values were preserved; retry metrics with a fresh snapshot", metrics.SessionID)
	}
	return saveMetricsOnConn(conn, metrics, metrics.InputHash, metrics.OutputHash)
}
