package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestMigrationV2_AppliesCleanly verifies that migration v2 correctly
// creates session_entries, widens session_metrics (dropping the GENERATED
// tokens_total column, adding v2 nullable columns), and sets compute_version
// with DEFAULT 0.
//
// A fresh DB always runs all migrations (v1→v2→v3), so this tests the
// complete migration sequence produces the expected schema.
func TestMigrationV2_AppliesCleanly(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify session_entries table was created by migration v2.
	seCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_entries'`)
	if seCount != 1 {
		t.Fatalf("session_entries table not found after migration v2")
	}

	// Verify session_metrics has compute_version column (added by v2).
	cvCol := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name='compute_version'`)
	if cvCol != 1 {
		t.Error("session_metrics missing compute_version column after migration v2")
	}

	// Verify tokens_total GENERATED column is gone (dropped by v2 migration).
	tokTotalCol := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name='tokens_total'`)
	if tokTotalCol != 0 {
		t.Error("session_metrics still has tokens_total column; should be dropped by migration v2")
	}

	// Verify v2 QualitySession columns exist (spot-check).
	for _, col := range []string{"outcome", "signal_density", "spec_quality_score", "title", "total_tokens"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("session_metrics missing v2 column %q after migration v2", col)
		}
	}

	// Verify renamed v1→v2 columns exist under their new names.
	for _, col := range []string{"input_tokens", "output_tokens", "tool_calls", "duration_minutes"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("session_metrics missing renamed column %q after migration v2", col)
		}
	}

	// Verify old v1 column names are gone (replaced by renamed v2 columns).
	for _, col := range []string{"tokens_in", "tokens_out", "tool_call_count", "duration_ms"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name=?`, col)
		if count != 0 {
			t.Errorf("session_metrics still has old v1 column %q; should have been renamed by migration v2", col)
		}
	}

	// Verify retained v1 columns still exist (turn_count, subagent_count have no v2 equivalent).
	for _, col := range []string{"turn_count", "subagent_count"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("session_metrics missing retained v1 column %q after migration v2", col)
		}
	}

	// Verify M-series columns exist (spot-check).
	for _, col := range []string{"m2_token_outcome_ratio", "m5_context_utilization_pct", "m7_spec_word_count", "m7_spec_has_examples"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("session_metrics missing M-series column %q after migration v2", col)
		}
	}
}

// TestMigrationV2_V1DataPreserved verifies that rows inserted via InsertSessions
// (which writes v1 columns only) survive in the widened schema with compute_version=0
// and correct token field values.
func TestMigrationV2_V1DataPreserved(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, "11111111-2222-3333-4444-555555555555", hash, "github.com-test-v1", defaults.HarnessClaudeCode, 1700000000000, 1234, 567)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify the row was written.
	rowCount := queryInt(t, conn, `SELECT COUNT(*) FROM session_metrics`)
	if rowCount != 1 {
		t.Fatalf("session_metrics: expected 1 row, got %d", rowCount)
	}

	// Verify v1 values were migrated to the new column names correctly.
	// tokens_in→input_tokens, tokens_out→output_tokens (direct copy, no conversion).
	tokIn := queryInt(t, conn, `SELECT input_tokens FROM session_metrics WHERE session_id = ?`, "11111111-2222-3333-4444-555555555555")
	if tokIn != 1234 {
		t.Errorf("input_tokens: expected 1234, got %d", tokIn)
	}
	tokOut := queryInt(t, conn, `SELECT output_tokens FROM session_metrics WHERE session_id = ?`, "11111111-2222-3333-4444-555555555555")
	if tokOut != 567 {
		t.Errorf("output_tokens: expected 567, got %d", tokOut)
	}
	// duration_ms→duration_minutes: 60000ms / 60000.0 = 1.0 minute.
	durMin := queryFloat(t, conn, `SELECT duration_minutes FROM session_metrics WHERE session_id = ?`, "11111111-2222-3333-4444-555555555555")
	if durMin != 1.0 {
		t.Errorf("duration_minutes: expected 1.0, got %f", durMin)
	}

	// Verify compute_version=0 (v1 default — row was not computed by the metrics engine).
	cv := queryInt(t, conn, `SELECT compute_version FROM session_metrics WHERE session_id = ?`, "11111111-2222-3333-4444-555555555555")
	if cv != 0 {
		t.Errorf("compute_version: expected 0 for v1-inserted row, got %d", cv)
	}

	// Verify v2 QualitySession columns are NULL (not yet computed by metrics engine).
	var outcomeIsNull bool
	err := sqlitex.ExecuteTransient(conn,
		`SELECT outcome IS NULL FROM session_metrics WHERE session_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{"11111111-2222-3333-4444-555555555555"},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				outcomeIsNull = stmt.ColumnInt(0) == 1
				return nil
			},
		})
	if err != nil {
		t.Fatalf("query outcome IS NULL: %v", err)
	}
	if !outcomeIsNull {
		t.Error("outcome: expected NULL for v1-inserted row (metrics engine has not run), got non-NULL")
	}
}

// TestMigrationV2_SessionEntriesSchema verifies session_entries has the correct
// column set: provider (NOT NULL), nullable optional fields, correct entry_type
// CHECK constraint (accepts new variants, rejects old removed variants).
func TestMigrationV2_SessionEntriesSchema(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Insert required FK rows for session_entries.
	// V23+: projects(project_hash, canonical_cwd, canonical_remote); host_slugs(opaque_id, host_slug, git_remote, canonical_remote); sessions uses opaque_host_id.
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO projects (project_hash, canonical_cwd) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/p')`, nil); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const migSlugOpaqueID = "aa11bb22cc33dd44ee55ff6677889900aa11bb22cc33dd44ee55ff6677889900"
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+migSlugOpaqueID+`','mig-slug','git@test')`, nil); err != nil {
		t.Fatalf("insert host_slug: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO sessions (session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES ('mig-sess-1','claude-code','m','`+migSlugOpaqueID+`','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1,2,3,'/f','jsonl')`, nil); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Insert a valid session_entries row using new columns (provider, entry_id, parent_entry_id, tool_call_id).
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role, content_preview, tool_call_id, entry_id, parent_entry_id)
         VALUES ('mig-sess-1', 0, 'claude', 'tool_use', 'assistant', 'preview text', 'toolu_123', 'entry-abc', NULL)`, nil)
	if err != nil {
		t.Fatalf("insert session_entries with new columns: %v", err)
	}

	// Verify tool_call_id was stored correctly.
	toolCallID := queryText(t, conn, `SELECT tool_call_id FROM session_entries WHERE session_id='mig-sess-1' AND entry_index=0`)
	if toolCallID != "toolu_123" {
		t.Errorf("tool_call_id: expected %q, got %q", "toolu_123", toolCallID)
	}

	// Verify removed entry_type variants are rejected by CHECK constraint.
	for _, badType := range []string{"code_execution", "unknown"} {
		err = sqlitex.ExecuteTransient(conn,
			`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role) VALUES ('mig-sess-1', ?, 'claude', ?, 'user')`,
			&sqlitex.ExecOptions{Args: []any{99, badType}})
		if err == nil {
			t.Errorf("expected CHECK to reject entry_type %q (removed variant), but insert succeeded", badType)
		}
	}

	// Verify new entry_type variants are accepted by CHECK constraint.
	for i, et := range []string{"system", "result"} {
		err = sqlitex.ExecuteTransient(conn,
			`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role) VALUES ('mig-sess-1', ?, 'claude', ?, 'user')`,
			&sqlitex.ExecOptions{Args: []any{i + 10, et}})
		if err != nil {
			t.Errorf("entry_type %q should be valid but got error: %v", et, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Migration v5 tests: session_entries_ext EAV table
// ---------------------------------------------------------------------------

// TestMigrationV5_SessionEntriesExtTable verifies that migration v5 creates
// the session_entries_ext table with correct schema.
func TestMigrationV5_SessionEntriesExtTable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify session_entries_ext table exists.
	tableCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_entries_ext'`)
	if tableCount != 1 {
		t.Fatal("session_entries_ext table not found after migration v5")
	}

	// Verify required columns exist.
	for _, col := range []string{"session_id", "entry_index", "key", "value_text", "value_int", "value_real"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_entries_ext') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("session_entries_ext missing column %q", col)
		}
	}

	// Verify index exists.
	indexCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_entries_ext_key'`)
	if indexCount != 1 {
		t.Error("idx_entries_ext_key index not found")
	}
}

// TestMigrationV5_InsertExtRow verifies that session_entries_ext accepts
// known key values (tokens_reasoning, cache_read, cache_write, model_id).
func TestMigrationV5_InsertExtRow(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Insert required FK rows.
	// V23+: projects(project_hash, canonical_cwd, canonical_remote); host_slugs(opaque_id, host_slug, ...); sessions uses opaque_host_id.
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO projects (project_hash, canonical_cwd) VALUES ('v5test','/p')`, nil); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const v5SlugOpaqueID = "55aa66bb77cc88dd99ee00ff1122334455aa66bb77cc88dd99ee00ff11223344"
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+v5SlugOpaqueID+`','v5slug','git@test')`, nil); err != nil {
		t.Fatalf("insert host_slug: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO sessions (session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES ('v5sess','claude-code','m','`+v5SlugOpaqueID+`','v5test',1,2,3,'/f','jsonl')`, nil); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role) VALUES ('v5sess', 0, 'claude', 'tool_use', 'assistant')`, nil); err != nil {
		t.Fatalf("insert session_entries: %v", err)
	}

	// Insert known keys into session_entries_ext.
	for _, testCase := range []struct {
		key      string
		valueInt *int
		valueTxt *string
	}{
		{"tokens_reasoning", intPtr(1000), nil},
		{"cache_read", intPtr(500), nil},
		{"cache_write", intPtr(200), nil},
		{"model_id", nil, strPtr("claude-3-opus-20240229")},
	} {
		var err error
		if testCase.valueInt != nil {
			err = sqlitex.ExecuteTransient(conn,
				`INSERT INTO session_entries_ext (session_id, entry_index, key, value_int) VALUES ('v5sess', 0, ?, ?)`,
				&sqlitex.ExecOptions{Args: []any{testCase.key, *testCase.valueInt}})
		} else if testCase.valueTxt != nil {
			err = sqlitex.ExecuteTransient(conn,
				`INSERT INTO session_entries_ext (session_id, entry_index, key, value_text) VALUES ('v5sess', 0, ?, ?)`,
				&sqlitex.ExecOptions{Args: []any{testCase.key, *testCase.valueTxt}})
		}
		if err != nil {
			t.Errorf("insert %q: %v", testCase.key, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Migration v6 tests: models reference table
// ---------------------------------------------------------------------------

// TestMigrationV6_ModelsTable verifies that migration v6 creates the models
// table with all expected columns.
func TestMigrationV6_ModelsTable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify models table exists.
	tableCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='models'`)
	if tableCount != 1 {
		t.Fatal("models table not found after migration v6")
	}

	// Verify required columns exist.
	expectedCols := []string{
		"model_id", "provider_key", "display_name", "family",
		"context_window", "max_output", "reasoning", "tool_call",
		"cost_input_per_mtok", "cost_output_per_mtok", "cost_reasoning_per_mtok",
		"cost_cache_read_per_mtok", "cost_cache_write_per_mtok", "release_date", "last_synced",
	}
	for _, col := range expectedCols {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('models') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("models missing column %q", col)
		}
	}

	// Verify model_id and provider_key columns exist and are part of PK (notnull=1).
	// In SQLite STRICT tables, required columns have notnull=1.
	modelIDNotNull := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('models') WHERE name='model_id' AND "notnull"=1`)
	if modelIDNotNull != 1 {
		t.Error("model_id should be NOT NULL (part of primary key)")
	}
	providerKeyNotNull := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('models') WHERE name='provider_key' AND "notnull"=1`)
	if providerKeyNotNull != 1 {
		t.Error("provider_key should be NOT NULL (part of primary key)")
	}

	// Verify index exists.
	indexCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_models_family'`)
	if indexCount != 1 {
		t.Error("idx_models_family index not found")
	}
}

// TestMigrationV6_InsertModel verifies that the models table accepts a row
// with all fields populated.
func TestMigrationV6_InsertModel(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO models (model_id, provider_key, display_name, family, context_window, max_output, reasoning, tool_call, last_synced)
		VALUES ('claude-3-opus-20240229', 'anthropic', 'Claude 3 Opus', 'claude', 200000, 4096, 1, 1, '2024-03-01')`,
		nil)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Migration v7 tests: cost columns
// ---------------------------------------------------------------------------

// TestMigrationV7_CostColumns verifies that migration v7 adds cost columns
// to session_metrics and daily_summary.
func TestMigrationV7_CostColumns(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify session_metrics cost columns.
	sessionMetricsCols := []string{
		"cost_input_usd", "cost_output_usd", "cost_reasoning_usd",
		"cost_cache_read_usd", "cost_cache_write_usd", "cost_total_usd", "cost_model_id",
	}
	for _, col := range sessionMetricsCols {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("session_metrics missing cost column %q", col)
		}
	}

	// Verify daily_summary cost columns.
	dailySummaryCols := []string{"total_cost_usd", "avg_cost_per_session_usd"}
	for _, col := range dailySummaryCols {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('daily_summary') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("daily_summary missing cost column %q", col)
		}
	}
}

// ---------------------------------------------------------------------------
// Migration v8 tests: daily_summary_by_project table
// ---------------------------------------------------------------------------

// TestMigrationV8_DailySummaryByProjectTable verifies that migration v8
// creates the daily_summary_by_project table and adds acceptance_rate.
func TestMigrationV8_DailySummaryByProjectTable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify daily_summary_by_project table exists.
	tableCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='daily_summary_by_project'`)
	if tableCount != 1 {
		t.Fatal("daily_summary_by_project table not found after migration v8")
	}

	// Verify required columns exist.
	expectedCols := []string{
		"date_utc", "project_hash", "project_path", "session_count",
		"total_tokens", "total_tool_calls", "total_duration_minutes",
		"resolved_count", "failed_count", "partial_count",
		"avg_spec_quality", "acceptance_rate", "total_cost_usd",
	}
	for _, col := range expectedCols {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('daily_summary_by_project') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("daily_summary_by_project missing column %q", col)
		}
	}

	// Verify acceptance_rate added to daily_summary.
	arCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('daily_summary') WHERE name='acceptance_rate'`)
	if arCount != 1 {
		t.Error("daily_summary missing acceptance_rate column")
	}

	// Verify index exists.
	indexCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_daily_project_hash'`)
	if indexCount != 1 {
		t.Error("idx_daily_project_hash index not found")
	}
}

// TestMigrationV8_InsertDailySummaryByProject verifies the table accepts a row.
func TestMigrationV8_InsertDailySummaryByProject(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO daily_summary_by_project (date_utc, project_hash, session_count, total_tokens, total_tool_calls, total_duration_minutes)
		VALUES ('2024-03-01', 'hash123', 10, 100000, 500, 120.5)`,
		nil)
	if err != nil {
		t.Fatalf("insert daily_summary_by_project: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Migration v9 tests: tags and scope columns
// ---------------------------------------------------------------------------

// TestMigrationV9_TagsAndScopeColumns verifies that migration v9 adds tags
// column to sessions and scope column to session_metrics.
func TestMigrationV9_TagsAndScopeColumns(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify tags column added to sessions.
	tagsCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='tags'`)
	if tagsCount != 1 {
		t.Error("sessions missing tags column")
	}

	// Verify scope column added to session_metrics.
	scopeCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_metrics') WHERE name='scope'`)
	if scopeCount != 1 {
		t.Error("session_metrics missing scope column")
	}
}

// TestMigrationV9_InsertWithTagsAndScope verifies that sessions.tags and
// session_metrics.scope accept values.
func TestMigrationV9_InsertWithTagsAndScope(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, "agent-a3aee4f", hash, "github.com-v9", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Update tags on sessions.
	err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET tags = '["feature","bug"]' WHERE session_id = 'agent-a3aee4f'`, nil)
	if err != nil {
		t.Fatalf("update tags: %v", err)
	}

	// Update scope on session_metrics.
	err = sqlitex.ExecuteTransient(conn, `UPDATE session_metrics SET scope = 'production' WHERE session_id = 'agent-a3aee4f'`, nil)
	if err != nil {
		t.Fatalf("update scope: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Migration v23 tests: opaque PKs + canonical_cwd
// ---------------------------------------------------------------------------

// TestMigrationV23_SchemaAppliesCleanly verifies that migration V23 produces
// the expected schema: host_slugs with opaque_id PK, projects without
// project_name/project_path, sessions with opaque_host_id FK.
func TestMigrationV23_SchemaAppliesCleanly(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify host_slugs has opaque_id as PRIMARY KEY.
	opaqIDCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('host_slugs') WHERE name='opaque_id'`)
	if opaqIDCount != 1 {
		t.Error("host_slugs missing opaque_id column after V23 migration")
	}
	// Verify host_slug is still a column (not PK, just a regular column now).
	hostSlugCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('host_slugs') WHERE name='host_slug'`)
	if hostSlugCount != 1 {
		t.Error("host_slugs missing host_slug column after V23 migration")
	}

	// Verify projects no longer has project_name or project_path columns.
	for _, col := range []string{"project_name", "project_path"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name=?`, col)
		if count != 0 {
			t.Errorf("projects still has removed column %q after V23 migration", col)
		}
	}
	// Verify projects has canonical_cwd and canonical_remote.
	for _, col := range []string{"canonical_cwd", "canonical_remote"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('projects') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("projects missing column %q after V23 migration", col)
		}
	}

	// Verify sessions has opaque_host_id and no host_slug column.
	opaqHostCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='opaque_host_id'`)
	if opaqHostCount != 1 {
		t.Error("sessions missing opaque_host_id column after V23 migration")
	}
	oldHostCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name='host_slug'`)
	if oldHostCount != 0 {
		t.Error("sessions still has removed host_slug column after V23 migration")
	}

	// Verify no orphan _v2 tables remain.
	for _, tbl := range []string{"host_slugs_v2", "projects_v2", "sessions_v2", "daily_summary_by_project_v2", "file_familiarity_v2"} {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl)
		if count != 0 {
			t.Errorf("orphan table %q still exists after V23 migration", tbl)
		}
	}
}

// TestMigrationV23_DataPreserved verifies that rows inserted via InsertSessions
// survive V23 migration with opaque_host_id FK intact and canonical_cwd populated.
func TestMigrationV23_DataPreserved(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "23230000000000000000000000000000000000000000000000000000000000aa"
	const v23SessID = "23000000-2300-2300-2300-230000000001"
	entry := makeStoreEntry(t, v23SessID, hash, "github.com-v23-test", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify the session has a non-empty opaque_host_id (64-char hex).
	var opaqueHostID string
	err := sqlitex.ExecuteTransient(conn,
		`SELECT opaque_host_id FROM sessions WHERE session_id=?`,
		&sqlitex.ExecOptions{
			Args: []any{v23SessID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				opaqueHostID = stmt.ColumnText(0)
				return nil
			},
		})
	if err != nil {
		t.Fatalf("query opaque_host_id: %v", err)
	}
	if len(opaqueHostID) != 64 {
		t.Errorf("opaque_host_id: expected 64-char hex, got %q (len=%d)", opaqueHostID, len(opaqueHostID))
	}

	// Verify the host_slug row exists with the correct FK.
	slugCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM host_slugs h JOIN sessions s ON h.opaque_id = s.opaque_host_id WHERE s.session_id=?`,
		v23SessID)
	if slugCount != 1 {
		t.Error("host_slugs JOIN sessions: expected 1 row, FK constraint not satisfied")
	}

	// Verify canonical_cwd is populated on the project.
	canonicalCWD := queryText(t, conn,
		`SELECT canonical_cwd FROM projects WHERE project_hash=?`, hash)
	if canonicalCWD == "" {
		t.Error("canonical_cwd: expected non-empty, got empty string")
	}
}
