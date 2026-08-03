package store

import (
	"context"
	"fmt"

	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Compile-time interface guard.
var _ ingest.ModelsSyncer = (*Store)(nil)

// SQL constants for the models table.
const (
	sqlUpsertModel = `INSERT OR REPLACE INTO models (
    model_id, provider_key, display_name, family,
    context_window, max_output, reasoning, tool_call,
    cost_input_per_mtok, cost_output_per_mtok, cost_reasoning_per_mtok,
    cost_cache_read_per_mtok, cost_cache_write_per_mtok,
    release_date, last_synced
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlGetModel = `SELECT
    model_id, provider_key, display_name, family,
    context_window, max_output, reasoning, tool_call,
    cost_input_per_mtok, cost_output_per_mtok, cost_reasoning_per_mtok,
    cost_cache_read_per_mtok, cost_cache_write_per_mtok,
    release_date, last_synced
FROM models
WHERE model_id = ? AND provider_key = ?`

	sqlGetContextWindow = `SELECT context_window
FROM models
WHERE model_id = ? AND context_window IS NOT NULL
LIMIT 1`
)

// SyncModels upserts model entries into the models table.
func (s *Store) SyncModels(ctx context.Context, models []ingest.ModelInfo) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	endFn := sqlitex.Transaction(conn)
	defer endFn(&err)

	for i := range models {
		m := &models[i]
		if err = sqlitex.ExecuteTransient(conn, sqlUpsertModel, &sqlitex.ExecOptions{
			Args: []any{
				m.ModelID,
				m.ProviderKey,
				m.DisplayName,
				derefString2(m.Family),
				derefInt(m.ContextWindow),
				derefInt(m.MaxOutput),
				boolToInt(m.Reasoning),
				boolToInt(m.ToolCall),
				derefFloat64(m.CostInputPerMTok),
				derefFloat64(m.CostOutputPerMTok),
				derefFloat64(m.CostReasoningPerMTok),
				derefFloat64(m.CostCacheReadPerMTok),
				derefFloat64(m.CostCacheWritePerMTok),
				derefString2(m.ReleaseDate),
				m.LastSynced,
			},
		}); err != nil {
			return fmt.Errorf("store: upsert model %s/%s: %w", m.ModelID, m.ProviderKey, err)
		}
	}

	return nil
}

// GetModel returns a single model by ID and provider key, or nil if not found.
func (s *Store) GetModel(ctx context.Context, modelID, providerKey string) (*ingest.ModelInfo, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var m *ingest.ModelInfo
	err = sqlitex.ExecuteTransient(conn, sqlGetModel, &sqlitex.ExecOptions{
		Args: []any{modelID, providerKey},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			m = scanModelInfo(stmt)
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("store: get model %s/%s: %w", modelID, providerKey, err)
	}
	return m, nil
}

// GetContextWindow returns the context window for any matching model.
// Second return is false if model not found (not an error).
func (s *Store) GetContextWindow(ctx context.Context, modelID string) (int, bool, error) {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	var ctxWindow int
	var found bool
	err = sqlitex.ExecuteTransient(conn, sqlGetContextWindow, &sqlitex.ExecOptions{
		Args: []any{modelID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			ctxWindow = stmt.ColumnInt(0)
			found = true
			return nil
		},
	})
	if err != nil {
		return 0, false, fmt.Errorf("store: get context window for %s: %w", modelID, err)
	}
	return ctxWindow, found, nil
}

// sqlEnsureModelMinimal inserts a minimal model row if one does not already exist.
// Used by annotate import to satisfy the FK constraint on annotators before creating
// an agent annotator whose model hasn't been synced via `models sync`.
const sqlEnsureModelMinimal = `INSERT OR IGNORE INTO models
    (model_id, provider_key, display_name, reasoning, tool_call, last_synced)
    VALUES (?, ?, ?, 0, 0, '')`

// EnsureModelMinimal inserts a minimal model row (INSERT OR IGNORE) for the given
// model_id and provider_key. If a row already exists, it is left unchanged.
// The display_name is set to modelID for minimal rows. Other fields use defaults.
func (s *Store) EnsureModelMinimal(ctx context.Context, modelID, providerKey string) error {
	conn, err := s.pool.Take(ctx)
	if err != nil {
		return fmt.Errorf("store: take connection: %w", err)
	}
	defer s.pool.Put(conn)

	if err := sqlitex.ExecuteTransient(conn, sqlEnsureModelMinimal, &sqlitex.ExecOptions{
		Args: []any{modelID, providerKey, modelID},
	}); err != nil {
		return fmt.Errorf("store: ensure model %s/%s: %w", modelID, providerKey, err)
	}
	return nil
}

// scanModelInfo reads a ModelInfo from the current statement.
// Column order must match sqlGetModel.
func scanModelInfo(stmt *sqlite.Stmt) *ingest.ModelInfo {
	m := &ingest.ModelInfo{
		ModelID:     stmt.ColumnText(0),
		ProviderKey: stmt.ColumnText(1),
		DisplayName: stmt.ColumnText(2),
		Reasoning:   stmt.ColumnInt(6) == 1,
		ToolCall:    stmt.ColumnInt(7) == 1,
		LastSynced:  stmt.ColumnText(14),
	}
	// Column 3: family
	if stmt.ColumnType(3) != sqlite.TypeNull {
		v := stmt.ColumnText(3)
		m.Family = &v
	}
	// Column 4: context_window
	if stmt.ColumnType(4) != sqlite.TypeNull {
		v := stmt.ColumnInt(4)
		m.ContextWindow = &v
	}
	// Column 5: max_output
	if stmt.ColumnType(5) != sqlite.TypeNull {
		v := stmt.ColumnInt(5)
		m.MaxOutput = &v
	}
	// Column 8: cost_input_per_mtok
	if stmt.ColumnType(8) != sqlite.TypeNull {
		v := stmt.ColumnFloat(8)
		m.CostInputPerMTok = &v
	}
	// Column 9: cost_output_per_mtok
	if stmt.ColumnType(9) != sqlite.TypeNull {
		v := stmt.ColumnFloat(9)
		m.CostOutputPerMTok = &v
	}
	// Column 10: cost_reasoning_per_mtok
	if stmt.ColumnType(10) != sqlite.TypeNull {
		v := stmt.ColumnFloat(10)
		m.CostReasoningPerMTok = &v
	}
	// Column 11: cost_cache_read_per_mtok
	if stmt.ColumnType(11) != sqlite.TypeNull {
		v := stmt.ColumnFloat(11)
		m.CostCacheReadPerMTok = &v
	}
	// Column 12: cost_cache_write_per_mtok
	if stmt.ColumnType(12) != sqlite.TypeNull {
		v := stmt.ColumnFloat(12)
		m.CostCacheWritePerMTok = &v
	}
	// Column 13: release_date
	if stmt.ColumnType(13) != sqlite.TypeNull {
		v := stmt.ColumnText(13)
		m.ReleaseDate = &v
	}
	return m
}
