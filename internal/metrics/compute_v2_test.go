package metrics_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// openTestStore opens a Store backed by a copy of the golden (pre-migrated) DB.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t)
}

// TestComputeMetrics_V2Integration runs the full Engine.ComputeMetrics flow
// with depth=1 fixture entries and verifies all v2 MetricFuncs produce results.
func TestComputeMetrics_V2Integration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	// Seed session.
	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	// Create depth=0 + depth=1 entries simulating a real full-depth session.
	parentIdx0 := 0
	parentIdx2 := 2
	toolInput1 := `{"file_path":"/src/main.go","content":"package main"}`
	toolInput2 := `{"file_path":"/src/util.go","content":"package util"}`
	toolOutput := `file contents here`
	toolUseID1 := "toolu_001"
	toolUseID2 := "toolu_002"
	readToolName := "Read"
	writeToolName := "Write"
	thinkingText := "Let me analyze the codebase structure and determine what changes are needed for this feature implementation."
	userPreview := "Please implement a function that checks if a number is prime. You must handle edge cases."
	responseText := "I'll implement the prime checker."

	tokIn1 := 500
	tokOut1 := 200
	tokIn2 := 300
	tokOut2 := 100
	ts1 := int64(1708531200000)
	ts2 := int64(1708531260000)
	ts3 := int64(1708531320000)

	entries := []schema.SessionEntry{
		// depth=0: user message
		{
			SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeText, Role: ingest.RoleUser,
			ContentPreview: &userPreview, TimestampMs: &ts1,
			TokensIn: &tokIn1,
		},
		// depth=1: user text content block (child of 0)
		{
			SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeText, Role: ingest.RoleUser,
			Depth: 1, ParentIndex: &parentIdx0,
			ContentPreview: &userPreview,
		},
		// depth=0: assistant message with tool use
		{
			SessionID: sid, EntryIndex: 2, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant,
			HasToolUse: true, HasThinking: true, TimestampMs: &ts2,
			TokensIn: &tokIn2, TokensOut: &tokOut1,
		},
		// depth=1: thinking block (child of 2)
		{
			SessionID: sid, EntryIndex: 3, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeThinking, Role: ingest.RoleAssistant,
			Depth: 1, ParentIndex: &parentIdx2, HasThinking: true,
			ContentPreview: &thinkingText,
		},
		// depth=1: text block (child of 2)
		{
			SessionID: sid, EntryIndex: 4, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			Depth: 1, ParentIndex: &parentIdx2,
			ContentPreview: &responseText,
		},
		// depth=1: tool_use Read (child of 2)
		{
			SessionID: sid, EntryIndex: 5, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant,
			Depth: 1, ParentIndex: &parentIdx2, HasToolUse: true,
			ToolNamesCSV: &readToolName, ToolInput: &toolInput1, ToolCallID: &toolUseID1,
		},
		// depth=1: tool_use Write (child of 2)
		{
			SessionID: sid, EntryIndex: 6, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant,
			Depth: 1, ParentIndex: &parentIdx2, HasToolUse: true,
			ToolNamesCSV: &writeToolName, ToolInput: &toolInput2, ToolCallID: &toolUseID2,
		},
		// depth=0: tool result
		{
			SessionID: sid, EntryIndex: 7, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeToolResult, Role: ingest.RoleTool,
			TimestampMs: &ts3, TokensOut: &tokOut2,
		},
		// depth=1: tool_result (child of 7)
		{
			SessionID: sid, EntryIndex: 8, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeToolResult, Role: ingest.RoleTool,
			Depth: 1, ParentIndex: intPtr(7),
			ToolOutput: &toolOutput, ToolCallID: &toolUseID1,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Run the full metrics computation.
	engine := metrics.NewEngine(s)
	computed, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if computed != 1 {
		t.Fatalf("expected 1 session computed, got %d", computed)
	}

	// Read back metrics and verify key fields are populated.
	m, err := s.GetMetrics(ctx, sid)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if m == nil {
		t.Fatal("GetMetrics returned nil")
	}

	// ComputeVersion should be CurrentComputeVersion.
	if m.ComputeVersion == nil || *m.ComputeVersion != metrics.CurrentComputeVersion {
		t.Errorf("ComputeVersion: expected %d, got %v", metrics.CurrentComputeVersion, m.ComputeVersion)
	}

	// SignalDensity should be non-nil (we have depth=1 entries).
	if m.SignalDensity == nil {
		t.Error("SignalDensity should be non-nil for sessions with depth=1 entries")
	}

	// FilesTouched should be non-nil (we have tool_use entries with file_path in ToolInput).
	if m.FilesTouched == nil {
		t.Error("FilesTouched should be non-nil")
	} else if *m.FilesTouched != 2 {
		t.Errorf("FilesTouched: expected 2, got %d", *m.FilesTouched)
	}

	// M3UniqueToolCount should be non-nil.
	if m.M3UniqueToolCount == nil {
		t.Error("M3UniqueToolCount should be non-nil")
	} else if *m.M3UniqueToolCount != 2 {
		t.Errorf("M3UniqueToolCount: expected 2 (Read, Write), got %d", *m.M3UniqueToolCount)
	}

	// M4ErrorRecoveryCount should be non-nil.
	if m.M4ErrorRecoveryCount == nil {
		t.Error("M4ErrorRecoveryCount should be non-nil")
	}

	// M5ContextUtilizationPct should be non-nil.
	if m.M5ContextUtilizationPct == nil {
		t.Error("M5ContextUtilizationPct should be non-nil")
	}

	// M5PeakContextTokens should be non-nil.
	if m.M5PeakContextTokens == nil {
		t.Error("M5PeakContextTokens should be non-nil")
	}

	// Outcome should be set.
	if m.Outcome == nil {
		t.Error("Outcome should be non-nil")
	}

	// Title should be set from first user message.
	if m.TitleGenerated == nil {
		t.Error("TitleGenerated should be non-nil")
	}

	// SpecQualityScore should be non-nil (user message has constraints: "must").
	if m.SpecQualityScore == nil {
		t.Error("SpecQualityScore should be non-nil")
	}

	// M7SpecHasConstraints should be true ("must" in user message).
	if m.M7SpecHasConstraints == nil || !*m.M7SpecHasConstraints {
		t.Error("M7SpecHasConstraints should be true")
	}
}

// TestComputeSignalDensity_HalfSignal verifies 50% signal density.
// Signal density now uses depth=0 user+assistant entries.
func TestComputeSignalDensity_HalfSignal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	shortPreview := "hi" // ≤20 chars → not meaningful
	longPreview := "This is a meaningful text block with more than twenty characters of content."

	entries := []schema.SessionEntry{
		// depth=0 assistant: short preview → not meaningful
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant, ContentPreview: &shortPreview},
		// depth=0 assistant: long preview → meaningful
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant, ContentPreview: &longPreview},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	engine := metrics.NewEngine(s)
	_, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}

	m, err := s.GetMetrics(ctx, sid)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if m.SignalDensity == nil {
		t.Fatal("SignalDensity should be non-nil")
	}
	// 1 meaningful out of 2 = 50%.
	if *m.SignalDensity != 50.0 {
		t.Errorf("SignalDensity: expected 50.0, got %f", *m.SignalDensity)
	}
}

// TestComputeFileAnalysis_NoToolUse verifies nil result when no tool_use entries.
func TestComputeFileAnalysis_NoToolUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	engine := metrics.NewEngine(s)
	engine.ComputeMetrics(ctx, []ingest.SessionID{sid})

	m, _ := s.GetMetrics(ctx, sid)
	if m.FilesTouched != nil {
		t.Errorf("FilesTouched should be nil for no-tool sessions, got %d", *m.FilesTouched)
	}
}

// TestComputeReverts_Detected verifies file re-edit detection.
func TestComputeReverts_Detected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	// Two writes to same file → 1 file re-edited (count ≥ 2).
	writeInput1 := `{"file_path":"/src/main.go","content":"package main\nfunc init() {}"}`
	writeInput2 := `{"file_path":"/src/main.go","content":"package main\nfunc main() {}"}`
	toolName := "Write"

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, Depth: 1, ParentIndex: intPtr(0), HasToolUse: true, ToolNamesCSV: &toolName, ToolInput: &writeInput1},
		{SessionID: sid, EntryIndex: 2, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, Depth: 1, ParentIndex: intPtr(0), HasToolUse: true, ToolNamesCSV: &toolName, ToolInput: &writeInput2},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	engine := metrics.NewEngine(s)
	engine.ComputeMetrics(ctx, []ingest.SessionID{sid})

	m, _ := s.GetMetrics(ctx, sid)
	if m.WithinSessionReverts == nil {
		t.Fatal("WithinSessionReverts should be non-nil")
	}
	// 1 file with 2 edits → 1 re-edit.
	if *m.WithinSessionReverts != 1 {
		t.Errorf("WithinSessionReverts: expected 1, got %d", *m.WithinSessionReverts)
	}
}

// TestComputeMetrics_V2Recompute verifies that sessions with v1 metrics
// are recomputed when CurrentComputeVersion > their compute_version.
func TestComputeMetrics_V2Recompute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	// Save v1 metrics (compute_version=1, which is < CurrentComputeVersion=2).
	tokIn := 1000
	tokOut := 500
	turns := 10
	toolCalls := 5
	durMin := 1.0
	v1Time := int64(1705276900000)
	v1Metrics := &ingest.SessionMetrics{
		SessionID: sid,
		QualityMetrics: schema.QualityMetrics{
			InputTokens:     &tokIn,
			OutputTokens:    &tokOut,
			ToolCalls:       &toolCalls,
			TurnCount:       &turns,
			DurationMinutes: &durMin,
			ComputedAt:      &v1Time,
			ComputeVersion:  intPtr(1),
		},
	}
	if err := s.SaveMetrics(ctx, v1Metrics); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	// Insert some entries so ComputeMetrics has data.
	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Compute: should recompute because compute_version=1 < CurrentComputeVersion=2.
	engine := metrics.NewEngine(s)
	computed, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid})
	if err != nil {
		t.Fatalf("ComputeMetrics: %v", err)
	}
	if computed != 1 {
		t.Errorf("expected 1 session recomputed, got %d", computed)
	}

	// Verify new compute_version.
	m, _ := s.GetMetrics(ctx, sid)
	if m.ComputeVersion == nil || *m.ComputeVersion != metrics.CurrentComputeVersion {
		t.Errorf("ComputeVersion: expected %d, got %v", metrics.CurrentComputeVersion, m.ComputeVersion)
	}
}

// seedSession is a helper that inserts minimal session data for metrics tests.
func ptrInt64(v int64) *int64 { return &v }

func seedSession(t *testing.T, ctx context.Context, s *store.Store, sessionID string) {
	t.Helper()

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	ph, err := ingest.NewProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("NewProjectHash: %v", err)
	}
	hs, err := ingest.NewHostSlug(testutil.TestHostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug: %v", err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	srcPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}

	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hs,
			Timestamp:     ingest.TimestampInfo{Start: 1000, End: 2000, Ingested: ptrInt64(3000)},
			Source:        ingest.SourceInfo{FilePath: string(srcPath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: ph, Name: "test-project", FilePath: "/home/test/project"},
			Stats:         ingest.StatsInfo{TurnCount: 10, ToolCallCount: 5},
		},
		Session: ingest.DiscoveredSession{
			SessionID: sid, Harness: defaults.HarnessClaudeCode, SourceFormat: ingest.SourceFormatJSONL,
		},
	}

	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
}

func intPtr(v int) *int { return &v }

// seedSessionWithDate is like seedSession but allows specifying a start_ms timestamp
// so that date(start_ms / 1000, 'unixepoch') maps to a specific date for daily_summary tests.
func seedSessionWithDate(t *testing.T, ctx context.Context, s *store.Store, sessionID string, startMs int64) {
	t.Helper()

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	ph, err := ingest.NewProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("NewProjectHash: %v", err)
	}
	hs, err := ingest.NewHostSlug(testutil.TestHostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug: %v", err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	srcPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}

	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hs,
			Timestamp:     ingest.TimestampInfo{Start: startMs, End: startMs + 1000, Ingested: ptrInt64(startMs + 2000)},
			Source:        ingest.SourceInfo{FilePath: string(srcPath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: ph, Name: "test-project", FilePath: "/home/test/project"},
			Stats:         ingest.StatsInfo{TurnCount: 10, ToolCallCount: 5},
		},
		Session: ingest.DiscoveredSession{
			SessionID: sid, Harness: defaults.HarnessClaudeCode, SourceFormat: ingest.SourceFormatJSONL,
		},
	}

	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
}

// Verify daily_summary still works with the existing insights test helper.
func TestComputeInsights_StillWorks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSessionWithDate(t, ctx, s, string(sid), 1705276800000) // 2024-01-15 00:00:00 UTC

	// Save metrics with the new compute version.
	tokIn := 1000
	tokOut := 500
	turns := 10
	toolCalls := 5
	durMin := 1.0
	now := int64(1705276900000)
	cv := metrics.CurrentComputeVersion
	m := &ingest.SessionMetrics{
		SessionID: sid,
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
		t.Fatalf("SaveMetrics: %v", err)
	}

	engine := metrics.NewEngine(s)
	if err := engine.ComputeInsights(ctx, []string{"2024-01-15"}); err != nil {
		t.Fatalf("ComputeInsights: %v", err)
	}

	// Verify daily_summary exists.
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer pool.Put(conn)

	var found bool
	sqlitex.ExecuteTransient(conn, `SELECT 1 FROM daily_summary WHERE date_utc = '2024-01-15'`, &sqlitex.ExecOptions{
		ResultFunc: func(_ *sqlite.Stmt) error { found = true; return nil },
	})
	if !found {
		t.Error("daily_summary row for 2024-01-15 not created")
	}
}
