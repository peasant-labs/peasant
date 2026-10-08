package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// seedStatsListSuite writes a native session (active generation, captured
// row, and legacy metrics row with different values) and a non-native
// session (legacy metrics row only) for the list/detail source test.
func seedStatsListSuite(t *testing.T, s *store.Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	exec := func(sql string, args ...any) {
		t.Helper()
		if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{Args: args}); err != nil {
			t.Fatalf("seed stats list: %v", err)
		}
	}
	exec(`INSERT INTO host_slugs(opaque_id, host_slug) VALUES('list-host','list-host')`)
	exec(`INSERT INTO projects(project_hash, canonical_cwd) VALUES('list-project','/synthetic/list')`)
	nativeID := "0a999aaa-36bc-424c-a789-8be54d9702e1"
	legacyID := "0a999aaa-36bc-424c-a789-8be54d9702e2"
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id, index_format_version)
VALUES(?, 'opencode', 'list-model', 'list-host', 'list-project', 1, 2, 3, '/synthetic/list.jsonl', 'jsonl', 11, 'gen-list', 2)`, nativeID)
	exec(`INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, index_format_version)
VALUES(?, 'opencode', 'list-model', 'list-host', 'list-project', 1, 2, 3, '/synthetic/list.jsonl', 'jsonl', 11, 2)`, legacyID)
	// Legacy metrics rows disagree with the captured row on purpose: the
	// native session must serve captured values, the other legacy ones.
	exec(`INSERT INTO session_metrics(session_id, turn_count, tool_calls, input_tokens, output_tokens, duration_minutes)
VALUES(?, 3, 4, 700, 100, 0.5)`, nativeID)
	exec(`INSERT INTO session_metrics(session_id, turn_count, tool_calls, input_tokens, output_tokens, duration_minutes)
VALUES(?, 3, 4, 700, 100, 0.5)`, legacyID)
	exec(`INSERT INTO session_captured_stats(session_id, turn_count, tool_call_count, tokens_out, duration_ms, tokens_in, source, updated_at_ms)
VALUES(?, 10, 20, 300, 60000, 111, 'harness', 100)`, nativeID)
}

// TestListDetailStatsSource pins list-detail-stats-source: list and detail
// rows serve the captured measurements for native sessions (with the peak
// input tokens and the summed total) and the legacy columns otherwise.
func TestListDetailStatsSource(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	seedStatsListSuite(t, s)
	nativeID := "0a999aaa-36bc-424c-a789-8be54d9702e1"
	legacyID := "0a999aaa-36bc-424c-a789-8be54d9702e2"
	rows, err := s.ListSessionsFiltered(ctx, store.SessionListFilter{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	byID := make(map[string]store.SessionRow, len(rows))
	for _, row := range rows {
		byID[row.SessionID] = row
	}
	native, ok := byID[nativeID]
	if !ok {
		t.Fatalf("native session missing from list: %v", byID)
	}
	if native.TurnCount != 10 || native.ToolCalls != 20 || native.OutputTokens != 300 {
		t.Fatalf("native list = (%d, %d, %d), want captured (10, 20, 300)",
			native.TurnCount, native.ToolCalls, native.OutputTokens)
	}
	if native.InputTokens != 700 {
		t.Fatalf("native input tokens = %d, want peak 700", native.InputTokens)
	}
	if native.TokensTotal != 1000 {
		t.Fatalf("native total = %d, want peak 700 + captured 300", native.TokensTotal)
	}
	if native.DurationMinutes != 1.0 {
		t.Fatalf("native duration = %v, want 1.0 minutes", native.DurationMinutes)
	}
	legacy, ok := byID[legacyID]
	if !ok {
		t.Fatalf("legacy session missing from list")
	}
	if legacy.TurnCount != 3 || legacy.ToolCalls != 4 || legacy.OutputTokens != 100 || legacy.TokensTotal != 800 {
		t.Fatalf("legacy list = %+v, want legacy values", legacy)
	}
	detail, err := s.SessionByID(ctx, nativeID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail.TurnCount != 10 || detail.ToolCalls != 20 || detail.OutputTokens != 300 {
		t.Fatalf("native detail = %+v, want captured values", detail)
	}
}

// TestComputeDerivedScopeNativeVsLegacy pins the COMPUTE writer retirement:
// a native save nulls the moved analysis columns and upserts derived values
// into the captured row, while a non-native save writes the legacy row whole
// and creates no captured row.
func TestComputeDerivedScopeNativeVsLegacy(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	seedStatsListSuite(t, s)
	nativeID := ingest.SessionID("0a999aaa-36bc-424c-a789-8be54d9702e1")
	legacyID := ingest.SessionID("0a999aaa-36bc-424c-a789-8be54d9702e2")
	turns := 12
	tools := 22
	out := 333
	peak := 700
	minutes := 2.0
	computedAt := int64(200)
	metrics := &ingest.SessionMetrics{
		SessionID: nativeID,
	}
	metrics.TurnCount = &turns
	metrics.ToolCalls = &tools
	metrics.OutputTokens = &out
	metrics.InputTokens = &peak
	metrics.DurationMinutes = &minutes
	metrics.ComputedAt = &computedAt
	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("native save: %v", err)
	}
	stats, err := s.ReadCapturedStats(ctx, schema.SessionID(nativeID))
	if err != nil {
		t.Fatalf("captured read: %v", err)
	}
	if stats.Source != store.StatsSourceDerived {
		t.Fatalf("captured source = %s, want derived", stats.Source)
	}
	if stats.TurnCount == nil || *stats.TurnCount != 12 {
		t.Fatalf("captured turn = %v, want 12", stats.TurnCount)
	}
	if stats.DurationMs == nil || *stats.DurationMs != 120000 {
		t.Fatalf("captured duration = %v, want 120000ms", stats.DurationMs)
	}
	got, err := s.GetMetrics(ctx, nativeID)
	if err != nil || got == nil {
		t.Fatalf("get metrics: %v %v", got, err)
	}
	if got.TurnCount == nil || *got.TurnCount != 12 {
		t.Fatalf("metrics turn = %v, want derived 12", got.TurnCount)
	}
	if got.InputTokens == nil || *got.InputTokens != 700 {
		t.Fatalf("metrics peak input = %v, want analysis 700", got.InputTokens)
	}
	legacyMetrics := &ingest.SessionMetrics{SessionID: legacyID}
	legacyMetrics.TurnCount = &turns
	if err := s.SaveMetrics(ctx, legacyMetrics); err != nil {
		t.Fatalf("legacy save: %v", err)
	}
	if _, err := s.ReadCapturedStats(ctx, schema.SessionID(legacyID)); err == nil {
		t.Fatal("legacy save created a captured row, want none")
	}
	legacyGot, err := s.GetMetrics(ctx, legacyID)
	if err != nil || legacyGot == nil || legacyGot.TurnCount == nil || *legacyGot.TurnCount != 12 {
		t.Fatalf("legacy metrics = %+v, %v; want turn 12", legacyGot, err)
	}
}

// TestInsertSessionsSeedingGates pins the ingest writer retirement: a
// session that already owns an active generation gets no seed document and
// no measurement seeding, while a session without one is seeded as before.
func TestInsertSessionsSeedingGates(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	hash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	entry := makeStoreEntry(t, "0b999aaa-36bc-424c-a789-8be54d9702e3", hash, "github.com-user-repo3", defaults.HarnessOpenCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	seed, err := s.GetMetricSeed(ctx, "0b999aaa-36bc-424c-a789-8be54d9702e3")
	if err != nil || seed == nil {
		t.Fatalf("seed after first insert = (%+v, %v), want a document", seed, err)
	}
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET active_generation_id = 'gen-seeded' WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{"0b999aaa-36bc-424c-a789-8be54d9702e3"},
	}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("set active: %v", err)
	}
	s.PoolForTest().Put(conn)
	// Re-ingest with different stats: the retired homes must not move.
	entry.Metadata.Stats.TurnCount = 999
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	seed, err = s.GetMetricSeed(ctx, "0b999aaa-36bc-424c-a789-8be54d9702e3")
	if err != nil {
		t.Fatalf("seed after re-insert: %v", err)
	}
	if seed != nil {
		t.Fatalf("seed after native re-insert = %+v, want cleared (nil)", seed)
	}
}

// TestStoredMetadataSeedSource pins the retained-evidence read: the
// embedded stats document comes from the harness-only seed home for native
// sessions and from the legacy column otherwise.
func TestStoredMetadataSeedSource(t *testing.T) {
	s := openV2TestStore(t)
	ctx := context.Background()
	seedStatsListSuite(t, s)
	nativeID := ingest.SessionID("0a999aaa-36bc-424c-a789-8be54d9702e1")
	legacyID := ingest.SessionID("0a999aaa-36bc-424c-a789-8be54d9702e2")
	if _, err := s.UpsertCapturedStats(ctx, store.CapturedStats{
		SessionID: schema.SessionID(nativeID), TurnCount: statsInt(10),
		SeedJSON: statsString(`{"turnCount":10,"toolCallCount":20}`),
		Source:   store.StatsSourceHarness, UpdatedAtMs: 200,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	data, err := s.ReadStoredMetadata(ctx, nativeID)
	if err != nil {
		t.Fatalf("stored metadata native: %v", err)
	}
	var doc struct {
		Stats *schema.SessionStats `json:"stats"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode stored metadata: %v", err)
	}
	if doc.Stats == nil || doc.Stats.TurnCount != 10 || doc.Stats.ToolCallCount != 20 {
		t.Fatalf("native embedded stats = %+v, want {10 20}", doc.Stats)
	}
	legacyData, err := s.ReadStoredMetadata(ctx, legacyID)
	if err != nil {
		t.Fatalf("stored metadata legacy: %v", err)
	}
	var legacyDoc struct {
		Stats *schema.SessionStats `json:"stats"`
	}
	if err := json.Unmarshal(legacyData, &legacyDoc); err != nil {
		t.Fatalf("decode legacy stored metadata: %v", err)
	}
	_ = legacyDoc
}
