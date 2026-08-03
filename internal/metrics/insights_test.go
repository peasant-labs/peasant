package metrics_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestComputeInsights_DailySummaryCreated verifies that Engine.ComputeInsights
// delegates to UpdateDailySummary and creates daily_summary rows with correct
// aggregations from sessions that have computed metrics.
func TestComputeInsights_DailySummaryCreated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Open a real SQLite store from the golden (pre-migrated) copy.
	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Insert two sessions on the same day (2024-01-15).
	// start_ms = 1705276800000 is 2024-01-15 00:00:00 UTC.
	dayMs := int64(1705276800000)
	sid1 := "11111111-1111-1111-1111-111111111111"
	sid2 := "22222222-2222-2222-2222-222222222222"
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	entries := []ingest.StoreEntry{
		makeInsightsStoreEntry(t, sid1, hash, "github.com-user-repo", dayMs, 1000, 500),
		makeInsightsStoreEntry(t, sid2, hash, "github.com-user-repo", dayMs+3600000, 2000, 1000),
	}
	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// Save metrics for both sessions (required for daily_summary aggregation).
	now := int64(1705276900000)
	for _, sid := range []string{sid1, sid2} {
		sessionID := ingest.SessionID(sid)
		tokIn := 1000
		tokOut := 500
		turns := 10
		toolCalls := 5
		durMin := 1.0
		cv := metrics.CurrentComputeVersion
		m := &ingest.SessionMetrics{
			SessionID: sessionID,
			QualityMetrics: schema.QualityMetrics{
				InputTokens:     &tokIn,
				OutputTokens:    &tokOut,
				ToolCalls:       &toolCalls,
				TurnCount:       &turns,
				DurationMinutes: &durMin,
				ComputedAt:      &now,
				ComputeVersion:  &cv,
			},
		}
		if err := s.SaveMetrics(ctx, m); err != nil {
			t.Fatalf("SaveMetrics(%s): %v", sid, err)
		}
	}

	// Create engine backed by the real store and call ComputeInsights.
	engine := metrics.NewEngine(s)
	if err := engine.ComputeInsights(ctx, []string{"2024-01-15"}); err != nil {
		t.Fatalf("ComputeInsights: %v", err)
	}

	// Verify daily_summary row was created by opening a separate read connection.
	verifyPool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("open verify pool: %v", err)
	}
	t.Cleanup(func() { _ = verifyPool.Close() })

	conn, err := verifyPool.Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	defer verifyPool.Put(conn)

	var sessionCount int
	var tokensIn int
	var found bool

	if err := sqlitex.ExecuteTransient(conn,
		`SELECT session_count, tokens_in FROM daily_summary WHERE date_utc = '2024-01-15'`,
		&sqlitex.ExecOptions{
			ResultFunc: func(stmt *sqlite.Stmt) error {
				found = true
				sessionCount = stmt.ColumnInt(0)
				tokensIn = stmt.ColumnInt(1)
				return nil
			},
		},
	); err != nil {
		t.Fatalf("query daily_summary: %v", err)
	}

	if !found {
		t.Fatal("daily_summary row for 2024-01-15 not found")
	}
	if sessionCount != 2 {
		t.Errorf("session_count: expected 2, got %d", sessionCount)
	}
	if tokensIn != 2000 { // 1000 + 1000
		t.Errorf("tokens_in: expected 2000, got %d", tokensIn)
	}
}

// makeInsightsStoreEntry builds an ingest.StoreEntry for the insights test.
func makeInsightsStoreEntry(t *testing.T, sessionID, projectHash, hostSlug string, startMs int64, tokensIn, tokensOut int) ingest.StoreEntry {
	t.Helper()

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	ph, err := ingest.NewProjectHash(projectHash)
	if err != nil {
		t.Fatalf("NewProjectHash(%q): %v", projectHash, err)
	}
	hs, err := ingest.NewHostSlug(hostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug(%q): %v", hostSlug, err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	srcPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}

	ingested := startMs + 120000
	return ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hs,
			Timestamp: ingest.TimestampInfo{
				Start:    startMs,
				End:      startMs + 60000,
				Ingested: &ingested,
			},
			Source: ingest.SourceInfo{
				FilePath: string(srcPath),
				Format:   ingest.SourceFormatJSONL,
			},
			Project: ingest.ProjectInfo{
				Hash:     ph,
				Name:     "test-project",
				FilePath: "/home/test/project",
			},
			Stats: ingest.StatsInfo{
				TurnCount:     10,
				ToolCallCount: 5,
				SubagentCount: 0,
				DurationMs:    60000,
				TokensIn:      tokensIn,
				TokensOut:     tokensOut,
			},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      defaults.HarnessClaudeCode,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
}
