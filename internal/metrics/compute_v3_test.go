package metrics_test

import (
	"context"
	"errors"
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

// TestOutputSurvival_NilAnalyzer verifies that M6 returns nil when GitDiffAnalyzer is nil.
func TestOutputSurvival_NilAnalyzer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	toolInput := `{"file_path":"/src/main.go","content":"package main"}`
	toolName := "Write"
	ts := int64(1708531200000)

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, TimestampMs: &ts},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, Depth: 1, ParentIndex: intPtr(0), HasToolUse: true, ToolNamesCSV: &toolName, ToolInput: &toolInput},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// NewEngine uses nil analyzer → M6 should return nil.
	engine := metrics.NewEngine(s)
	engine.ComputeMetrics(ctx, []ingest.SessionID{sid})

	m, _ := s.GetMetrics(ctx, sid)
	if m.M6OutputSurvivalPct != nil {
		t.Errorf("M6OutputSurvivalPct should be nil when analyzer is nil, got %f", *m.M6OutputSurvivalPct)
	}
	if m.M6LinesSurvived != nil {
		t.Errorf("M6LinesSurvived should be nil, got %d", *m.M6LinesSurvived)
	}
}

// TestOutputSurvival_WithAnalyzer verifies M6 computes output survival
// when a GitDiffAnalyzer is provided.
func TestOutputSurvival_WithAnalyzer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	// Write 2 lines; analyzer returns file with only 1 surviving line.
	toolInput := `{"file_path":"/src/main.go","content":"line1\nline2"}`
	toolName := "Write"
	ts := int64(1708531200000)

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, TimestampMs: &ts},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, Depth: 1, ParentIndex: intPtr(0), HasToolUse: true, ToolNamesCSV: &toolName, ToolInput: &toolInput, TimestampMs: &ts},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	stub := &testutil.StubGitDiffAnalyzer{
		FileContents: map[string][]byte{
			"/src/main.go@abc123": []byte("line1\nmodified_line2"),
		},
		Commits: []string{"abc123"},
	}

	engine := metrics.NewEngineWithAll(s, nil, stub)
	engine.ComputeMetrics(ctx, []ingest.SessionID{sid})

	m, _ := s.GetMetrics(ctx, sid)
	if m.M6OutputSurvivalPct == nil {
		t.Fatal("M6OutputSurvivalPct should be non-nil")
	}
	// 1 of 2 lines survived = 50%.
	if *m.M6OutputSurvivalPct != 50.0 {
		t.Errorf("M6OutputSurvivalPct: expected 50.0, got %f", *m.M6OutputSurvivalPct)
	}
	if m.M6LinesSurvived == nil || *m.M6LinesSurvived != 1 {
		t.Errorf("M6LinesSurvived: expected 1, got %v", m.M6LinesSurvived)
	}
	if m.M6LinesTotal == nil || *m.M6LinesTotal != 2 {
		t.Errorf("M6LinesTotal: expected 2, got %v", m.M6LinesTotal)
	}
}

// TestOutputSurvival_FileDeleted verifies M6 returns 0% when file was deleted.
func TestOutputSurvival_FileDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	sid := ingest.SessionID(testutil.TestSessionUUID)
	seedSession(t, ctx, s, string(sid))

	toolInput := `{"file_path":"/src/deleted.go","content":"line1\nline2\nline3"}`
	toolName := "Write"
	ts := int64(1708531200000)

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, TimestampMs: &ts},
		{SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, Depth: 1, ParentIndex: intPtr(0), HasToolUse: true, ToolNamesCSV: &toolName, ToolInput: &toolInput, TimestampMs: &ts},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	stub := &testutil.StubGitDiffAnalyzer{
		FileContents: map[string][]byte{}, // file not in git = deleted
		Commits:      []string{"abc123"},
		GetFileErr:   errors.New("file not found at commit"),
	}

	engine := metrics.NewEngineWithAll(s, nil, stub)
	engine.ComputeMetrics(ctx, []ingest.SessionID{sid})

	m, _ := s.GetMetrics(ctx, sid)
	if m.M6OutputSurvivalPct == nil {
		t.Fatal("M6OutputSurvivalPct should be non-nil (0% for deleted file)")
	}
	if *m.M6OutputSurvivalPct != 0.0 {
		t.Errorf("M6OutputSurvivalPct: expected 0.0, got %f", *m.M6OutputSurvivalPct)
	}
}

// TestClassifyScope verifies scope classification heuristics.
func TestClassifyScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		gitRemote string
		want      string
	}{
		{"empty", "", "unknown"},
		{"localhost", "http://localhost/repo", "unknown"},
		{"file_protocol", "file:///home/user/repo", "unknown"},
		{"github", "git@github.com:user/repo.git", "personal"},
		{"github_https", "https://github.com/org/repo", "personal"},
		{"gitlab", "git@gitlab.com:org/repo.git", "org"},
		{"enterprise", "git@git.enterprise.example.com:org/repo.git", "org"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := metrics.ClassifyScope(tt.gitRemote)
			if got != tt.want {
				t.Errorf("ClassifyScope(%q) = %q, want %q", tt.gitRemote, got, tt.want)
			}
		})
	}
}

// TestComputeScope_Integration verifies that scope is populated in metrics.
func TestComputeScope_Integration(t *testing.T) {
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
	if m.Scope == nil {
		t.Fatal("Scope should be non-nil")
	}
	// With no git_remote in entries, scope defaults to "unknown".
	if *m.Scope != "unknown" {
		t.Errorf("Scope: expected 'unknown', got %q", *m.Scope)
	}
}

// TestAcceptanceRate_DailySummary verifies that acceptance_rate is computed
// correctly in daily_summary.
func TestAcceptanceRate_DailySummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Create 3 sessions on 2024-01-15: 2 resolved, 1 failed.
	// 2024-01-15 00:00:00 UTC = 1705276800000 ms
	baseMs := int64(1705276800000)
	sid1 := "99d59925-36bc-424c-a789-8be54d9702b1"
	sid2 := "99d59925-36bc-424c-a789-8be54d9702b2"
	sid3 := "99d59925-36bc-424c-a789-8be54d9702b3"

	seedSessionWithDate(t, ctx, s, sid1, baseMs)
	seedSessionWithDate(t, ctx, s, sid2, baseMs+1000)
	seedSessionWithDate(t, ctx, s, sid3, baseMs+2000)

	now := int64(1705277000000)
	resolved := ingest.OutcomeResolved
	failed := ingest.OutcomeFailed

	for _, tc := range []struct {
		sid     string
		outcome ingest.SessionOutcome
	}{
		{sid1, resolved},
		{sid2, resolved},
		{sid3, failed},
	} {
		tokIn := 100
		cv := metrics.CurrentComputeVersion
		m := &ingest.SessionMetrics{
			SessionID: ingest.SessionID(tc.sid),
			QualityMetrics: schema.QualityMetrics{
				InputTokens:    &tokIn,
				Outcome:        &tc.outcome,
				ComputedAt:     &now,
				ComputeVersion: &cv,
			},
		}
		if err := s.SaveMetrics(ctx, m); err != nil {
			t.Fatalf("SaveMetrics %s: %v", tc.sid, err)
		}
	}

	engine := metrics.NewEngine(s)
	if err := engine.ComputeInsights(ctx, []string{"2024-01-15"}); err != nil {
		t.Fatalf("ComputeInsights: %v", err)
	}

	// Query acceptance_rate from daily_summary.
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

	var rate float64
	var found bool
	sqlitex.ExecuteTransient(conn, `SELECT acceptance_rate FROM daily_summary WHERE date_utc = '2024-01-15'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rate = stmt.ColumnFloat(0)
			found = true
			return nil
		},
	})
	if !found {
		t.Fatal("daily_summary row not found for 2024-01-15")
	}

	// 2 resolved out of 3 sessions = 0.6667.
	expected := 2.0 / 3.0
	if rate < expected-0.01 || rate > expected+0.01 {
		t.Errorf("acceptance_rate: expected ~%.4f, got %.4f", expected, rate)
	}
}

// TestPerProjectDailySummary verifies that daily_summary_by_project is populated.
func TestPerProjectDailySummary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	baseMs := int64(1705276800000) // 2024-01-15
	sid := testutil.TestSessionUUID
	seedSessionWithDate(t, ctx, s, sid, baseMs)

	now := int64(1705277000000)
	tokIn := 100
	resolvedOutcome := ingest.OutcomeResolved
	cv2 := metrics.CurrentComputeVersion
	m := &ingest.SessionMetrics{
		SessionID: ingest.SessionID(sid),
		QualityMetrics: schema.QualityMetrics{
			InputTokens:    &tokIn,
			Outcome:        &resolvedOutcome,
			ComputedAt:     &now,
			ComputeVersion: &cv2,
		},
	}
	if err := s.SaveMetrics(ctx, m); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	engine := metrics.NewEngine(s)
	if err := engine.ComputeInsights(ctx, []string{"2024-01-15"}); err != nil {
		t.Fatalf("ComputeInsights: %v", err)
	}

	// Query daily_summary_by_project.
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

	var projectCount int
	var sessionCount int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*), SUM(session_count) FROM daily_summary_by_project WHERE date_utc = '2024-01-15'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			projectCount = stmt.ColumnInt(0)
			sessionCount = stmt.ColumnInt(1)
			return nil
		},
	})
	if projectCount != 1 {
		t.Errorf("expected 1 project row, got %d", projectCount)
	}
	if sessionCount != 1 {
		t.Errorf("expected session_count=1, got %d", sessionCount)
	}
}
