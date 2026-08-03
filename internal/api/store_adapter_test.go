package api_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_discovery.yaml
var selectionDiscoveryFixtureYAML []byte

type selectionDiscoveryFixture struct {
	Harness         string `yaml:"harness"`
	SelectedSession string `yaml:"selectedSession"`
	HiddenSession   string `yaml:"hiddenSession"`
	Sessions        []struct {
		ID            string `yaml:"id"`
		ProjectHash   string `yaml:"projectHash"`
		HostSlug      string `yaml:"hostSlug"`
		StartMs       int64  `yaml:"startMs"`
		TokensIn      int    `yaml:"tokensIn"`
		TokensOut     int    `yaml:"tokensOut"`
		Project       string `yaml:"project"`
		TurnCount     int    `yaml:"turnCount"`
		ToolCallCount int    `yaml:"toolCallCount"`
		DurationMs    int64  `yaml:"durationMs"`
	} `yaml:"sessions"`
	Expected struct {
		VisibleSessions int `yaml:"visibleSessions"`
		VisibleProjects int `yaml:"visibleProjects"`
		VisibleTokens   int `yaml:"visibleTokens"`
	} `yaml:"expected"`
}

func loadSelectionDiscoveryFixture(t *testing.T) selectionDiscoveryFixture {
	t.Helper()
	var fixture selectionDiscoveryFixture
	decoder := yaml.NewDecoder(bytes.NewReader(selectionDiscoveryFixtureYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode selection discovery fixture: %v", err)
	}
	if len(fixture.Sessions) < 2 || fixture.SelectedSession == fixture.HiddenSession {
		t.Fatal("selection discovery fixture must contain distinct selected and hidden sessions")
	}
	selectedTokens := 0
	seenSelected, seenHidden := false, false
	for _, session := range fixture.Sessions {
		if session.ID == fixture.SelectedSession {
			seenSelected = true
			selectedTokens += session.TokensIn + session.TokensOut
		}
		if session.ID == fixture.HiddenSession {
			seenHidden = true
		}
	}
	if !seenSelected || !seenHidden {
		t.Fatal("selection discovery fixture must include both referenced sessions")
	}
	if selectedTokens != fixture.Expected.VisibleTokens {
		t.Fatalf("fixture visibleTokens = %d, selected rows total %d", fixture.Expected.VisibleTokens, selectedTokens)
	}
	return fixture
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// openTestStore opens a Store backed by a copy of the golden (pre-migrated) DB.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t)
}

// makeStoreEntry builds a StoreEntry with sensible defaults for testing.
// All newtype fields are validated via constructors to catch invalid test data early.
func makeStoreEntry(
	t *testing.T,
	sessionID, projectHash, hostSlug string,
	harness defaults.Harness,
	startMs int64,
	tokensIn, tokensOut int,
	projectName string,
	turnCount, toolCallCount int,
	durationMs int64,
) ingest.StoreEntry {
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

	ingested := startMs + durationMs + 60000
	return ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  harness,
			Model:         model,
			HostSlug:      hs,
			Timestamp: ingest.TimestampInfo{
				Start:    startMs,
				End:      startMs + durationMs,
				Ingested: &ingested,
			},
			Source: ingest.SourceInfo{
				FilePath: string(srcPath),
				Format:   ingest.SourceFormatJSONL,
			},
			Project: ingest.ProjectInfo{
				Hash:     ph,
				Name:     projectName,
				FilePath: "/home/test/" + projectName,
			},
			Stats: ingest.StatsInfo{
				TurnCount:     turnCount,
				ToolCallCount: toolCallCount,
				SubagentCount: 0,
				DurationMs:    durationMs,
				TokensIn:      tokensIn,
				TokensOut:     tokensOut,
			},
			Version:     "2.1.14",
			Subagents:   []ingest.SubagentRef{},
			Diagnostics: ingest.DiagnosticsInfo{Warnings: []ingest.DiagnosticEntry{}},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      harness,
			SourcePath:   srcPath,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
}

// seedStore inserts entries into a test store and returns the provider.
func seedStore(t *testing.T, s *store.Store, entries []ingest.StoreEntry) *api.StoreDataProvider {
	t.Helper()
	ctx := context.Background()
	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	// Compute daily summaries (decoupled from InsertSessions).
	daySet := make(map[string]bool)
	for _, e := range entries {
		if e.Metadata != nil && e.Metadata.Timestamp.Start > 0 {
			day := time.Unix(e.Metadata.Timestamp.Start/1000, 0).UTC().Format("2006-01-02")
			daySet[day] = true
		}
	}
	days := make([]string, 0, len(daySet))
	for d := range daySet {
		days = append(days, d)
	}
	if err := s.UpdateDailySummary(ctx, days); err != nil {
		t.Fatalf("UpdateDailySummary: %v", err)
	}
	return api.NewStoreDataProvider(s, sessionvisibility.All())
}

// seedStoreWithFS is seedStore but wires the given FileSystem into the
// provider (via NewStoreDataProviderWithFS) instead of the real OS
// filesystem, so SessionByID's content overlay re-index reads from a MemFS
// fixture in tests.
func seedStoreWithFS(t *testing.T, s *store.Store, entries []ingest.StoreEntry, fs ingest.FileSystem) *api.StoreDataProvider {
	t.Helper()
	ctx := context.Background()
	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	daySet := make(map[string]bool)
	for _, e := range entries {
		if e.Metadata != nil && e.Metadata.Timestamp.Start > 0 {
			day := time.Unix(e.Metadata.Timestamp.Start/1000, 0).UTC().Format("2006-01-02")
			daySet[day] = true
		}
	}
	days := make([]string, 0, len(daySet))
	for d := range daySet {
		days = append(days, d)
	}
	if err := s.UpdateDailySummary(ctx, days); err != nil {
		t.Fatalf("UpdateDailySummary: %v", err)
	}
	return api.NewStoreDataProviderWithFS(s, sessionvisibility.All(), fs)
}

// TestStoreDataProvider_AnnotationsForSessionIncludesAssociationTarget proves
// the WebSocket DataProvider path exposes association annotations alongside
// normal session annotations while preserving the opaque target ID.
func TestStoreDataProvider_AnnotationsForSessionIncludesAssociationTarget(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	sessionID := testutil.TestSessionUUID
	provider := seedStore(t, db, []ingest.StoreEntry{makeStoreEntry(
		t,
		sessionID,
		string(testutil.TestProjectHash),
		testutil.TestHostSlug,
		defaults.HarnessClaudeCode,
		1_700_000_000_000,
		100,
		50,
		testutil.TestProjectName,
		1,
		0,
		60_000,
	)})
	ctx := context.Background()
	if err := db.UpsertSessionCommits(ctx, ingest.SessionID(sessionID), []ingest.CommitInfo{{
		Hash:    "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Message: "association provider commit",
	}}); err != nil {
		t.Fatalf("UpsertSessionCommits: %v", err)
	}
	associations, err := db.ListCurrentSessionCommitAssociations(ctx, ingest.SessionID(sessionID))
	if err != nil {
		t.Fatalf("ListCurrentSessionCommitAssociations: %v", err)
	}
	if len(associations) != 1 {
		t.Fatalf("current associations = %d, want 1", len(associations))
	}
	annotationTypeID, err := db.GetAnnotationTypeID(ctx, testutil.TestTypeIDSessionOutcome)
	if err != nil {
		t.Fatalf("GetAnnotationTypeID: %v", err)
	}
	annotatorID, err := db.GetAnnotatorIDByName(ctx, "outcome-classifier")
	if err != nil {
		t.Fatalf("GetAnnotatorIDByName: %v", err)
	}
	if _, err := db.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AssociationID:    &associations[0].ID,
		AnnotationTypeID: annotationTypeID,
		AnnotatorID:      annotatorID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	annotations, err := provider.AnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("AnnotationsForSession: %v", err)
	}
	if len(annotations) != 1 {
		t.Fatalf("AnnotationsForSession = %d, want 1", len(annotations))
	}
	annotation := annotations[0]
	if annotation.TargetKind != schema.TargetAssociation {
		t.Errorf("annotation target kind = %q, want %q", annotation.TargetKind, schema.TargetAssociation)
	}
	if annotation.TargetAssociationID == nil || *annotation.TargetAssociationID != associations[0].ID {
		t.Errorf("annotation target association ID = %v, want %q", annotation.TargetAssociationID, associations[0].ID)
	}
}

func TestStoreDataProvider_SelectedSessionScopesDiscoveryButNotDetail(t *testing.T) {
	fixture := loadSelectionDiscoveryFixture(t)
	s := openTestStore(t)
	if fixture.Harness != defaults.HarnessClaudeCode.String() {
		t.Fatalf("fixture harness = %q, want %q", fixture.Harness, defaults.HarnessClaudeCode)
	}
	harness := defaults.HarnessClaudeCode
	entries := make([]ingest.StoreEntry, 0, len(fixture.Sessions))
	for _, session := range fixture.Sessions {
		entries = append(entries, makeStoreEntry(
			t,
			session.ID,
			session.ProjectHash,
			session.HostSlug,
			harness,
			session.StartMs,
			session.TokensIn,
			session.TokensOut,
			session.Project,
			session.TurnCount,
			session.ToolCallCount,
			session.DurationMs,
		))
	}
	seedStore(t, s, entries)
	policy, err := sessionvisibility.New(config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			harness.String(): {Sessions: []string{fixture.SelectedSession}},
		},
	})
	if err != nil {
		t.Fatalf("selection policy: %v", err)
	}
	provider := api.NewStoreDataProvider(s, policy)
	ctx := context.Background()

	sessions, err := provider.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(sessions) != fixture.Expected.VisibleSessions || string(sessions[0].ID) != fixture.SelectedSession {
		t.Fatalf("Sessions = %+v, want only the explicit session", sessions)
	}
	summaries, err := provider.SessionSummaries(ctx)
	if err != nil || len(summaries) != fixture.Expected.VisibleSessions {
		t.Fatalf("SessionSummaries = %+v, %v; want one visible row", summaries, err)
	}
	dashboard, err := provider.DashboardMetrics(ctx)
	if err != nil || dashboard.TotalSessions != fixture.Expected.VisibleSessions || dashboard.TotalTokens != fixture.Expected.VisibleTokens {
		t.Fatalf("DashboardMetrics = %+v, %v; want selected-session totals", dashboard, err)
	}
	trends, err := provider.TrendsData(ctx)
	if err != nil || trends.TotalSessions != fixture.Expected.VisibleSessions || trends.TotalTokens != fixture.Expected.VisibleTokens {
		t.Fatalf("TrendsData = %+v, %v; want selected-session totals", trends, err)
	}
	quality, err := provider.QualitySessions(ctx, api.QualityFilter{})
	if err != nil || len(quality) != fixture.Expected.VisibleSessions {
		t.Fatalf("QualitySessions = %+v, %v; want one visible row", quality, err)
	}
	projects, err := provider.ProjectSummaries(ctx)
	if err != nil || len(projects.Projects) != fixture.Expected.VisibleProjects || projects.Projects[0].Sessions != fixture.Expected.VisibleSessions {
		t.Fatalf("ProjectSummaries = %+v, %v; want one project with one visible session", projects, err)
	}

	// Direct access remains available even though the sibling is absent from discovery.
	detail, err := provider.SessionByID(ctx, fixture.HiddenSession)
	if err != nil || detail == nil {
		t.Fatalf("SessionByID hidden sibling = %+v, %v; want retained direct access", detail, err)
	}
}

// ---------------------------------------------------------------------------
// Test constants
// ---------------------------------------------------------------------------

const (
	hash1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hash2 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// 2024-01-15T00:00:00Z in ms
	day1Ms = int64(1705276800000)
	// 2024-01-16T00:00:00Z in ms
	day2Ms = int64(1705363200000)
)

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func TestStoreDataProvider_Sessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-user-repo1",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-alpha", 10, 5, 60000),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-user-repo1",
			defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800, "project-alpha", 15, 8, 90000),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash2, "github.com-user-repo2",
			defaults.HarnessOpenCode, day2Ms, 3000, 1200, "project-beta", 20, 12, 120000),
	}

	provider := seedStore(t, s, entries)
	ctx := context.Background()

	sessions, err := provider.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}

	if len(sessions) != 3 {
		t.Fatalf("Sessions: expected 3, got %d", len(sessions))
	}

	// Ordered by start_ms DESC: session 33 first.
	sess := sessions[0]
	if sess.ID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("sessions[0].ID: expected 33..., got %s", sess.ID)
	}
	if sess.Harness != defaults.HarnessOpenCode {
		t.Errorf("sessions[0].Harness: expected %q, got %q", defaults.HarnessOpenCode, sess.Harness)
	}

	expectedStart := time.UnixMilli(day2Ms)
	if !sess.StartTime.Equal(expectedStart) {
		t.Errorf("sessions[0].StartTime: expected %v, got %v", expectedStart, sess.StartTime)
	}

	expectedEnd := time.UnixMilli(day2Ms + 120000)
	if !sess.EndTime.Equal(expectedEnd) {
		t.Errorf("sessions[0].EndTime: expected %v, got %v", expectedEnd, sess.EndTime)
	}

	if sess.Turns != nil {
		t.Errorf("sessions[0].Turns: expected nil (v2), got %d turns", len(sess.Turns))
	}

	// Verify metadata mapping.
	if sess.Metadata.TokensIn != 3000 {
		t.Errorf("sessions[0].Metadata.TokensIn: expected 3000, got %d", sess.Metadata.TokensIn)
	}
	if sess.Metadata.TokensOut != 1200 {
		t.Errorf("sessions[0].Metadata.TokensOut: expected 1200, got %d", sess.Metadata.TokensOut)
	}
	if sess.Metadata.TotalTokens != 4200 {
		t.Errorf("sessions[0].Metadata.TotalTokens: expected 4200, got %d", sess.Metadata.TotalTokens)
	}
	if sess.Metadata.TurnCount != 20 {
		t.Errorf("sessions[0].Metadata.TurnCount: expected 20, got %d", sess.Metadata.TurnCount)
	}
	if sess.Metadata.ToolCallCount != 12 {
		t.Errorf("sessions[0].Metadata.ToolCallCount: expected 12, got %d", sess.Metadata.ToolCallCount)
	}
	expectedDuration := 120 * time.Second
	if sess.Metadata.Duration != expectedDuration {
		t.Errorf("sessions[0].Metadata.Duration: expected %v, got %v", expectedDuration, sess.Metadata.Duration)
	}
}

// ---------------------------------------------------------------------------
// SessionByID
// ---------------------------------------------------------------------------

func TestStoreDataProvider_SessionByID_Found(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	entry := makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-user-repo1",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-alpha", 10, 5, 60000)

	provider := seedStore(t, s, []ingest.StoreEntry{entry})
	ctx := context.Background()

	sess, err := provider.SessionByID(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}

	if sess.ID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("ID: expected 11..., got %s", sess.ID)
	}
	if sess.Harness != defaults.HarnessClaudeCode {
		t.Errorf("Harness:  expected %q, got %q", defaults.HarnessClaudeCode, sess.Harness)
	}
	if sess.Metadata.TokensIn != 1000 {
		t.Errorf("Metadata.TokensIn: expected 1000, got %d", sess.Metadata.TokensIn)
	}
	if sess.Metadata.TokensOut != 500 {
		t.Errorf("Metadata.TokensOut: expected 500, got %d", sess.Metadata.TokensOut)
	}
	if sess.Metadata.TotalTokens != 1500 {
		t.Errorf("Metadata.TotalTokens: expected 1500, got %d", sess.Metadata.TotalTokens)
	}
	if sess.Metadata.TurnCount != 10 {
		t.Errorf("Metadata.TurnCount: expected 10, got %d", sess.Metadata.TurnCount)
	}
	if sess.Metadata.ToolCallCount != 5 {
		t.Errorf("Metadata.ToolCallCount: expected 5, got %d", sess.Metadata.ToolCallCount)
	}
	expectedDuration := 60 * time.Second
	if sess.Metadata.Duration != expectedDuration {
		t.Errorf("Metadata.Duration: expected %v, got %v", expectedDuration, sess.Metadata.Duration)
	}
}

func TestStoreDataProvider_SessionByID_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	provider := api.NewStoreDataProvider(s, sessionvisibility.All())
	ctx := context.Background()

	sess, err := provider.SessionByID(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("SessionByID: expected error for nonexistent ID, got nil")
	}
	if sess != nil {
		t.Errorf("SessionByID: expected nil session, got %+v", sess)
	}
}

// ---------------------------------------------------------------------------
// DashboardMetrics
// ---------------------------------------------------------------------------

func TestStoreDataProvider_DashboardMetrics(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "test-project", 10, 5, 60000),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800, "test-project", 10, 5, 60000),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash1, "github.com-test",
			defaults.HarnessOpenCode, day2Ms, 3000, 1200, "test-project", 10, 5, 60000),
	}

	provider := seedStore(t, s, entries)
	ctx := context.Background()

	dash, err := provider.DashboardMetrics(ctx)
	if err != nil {
		t.Fatalf("DashboardMetrics: %v", err)
	}
	if dash == nil {
		t.Fatal("DashboardMetrics: expected non-nil payload")
	}

	if dash.TotalSessions != 3 {
		t.Errorf("TotalSessions: expected 3, got %d", dash.TotalSessions)
	}

	// TotalTokens = (1000+500) + (2000+800) + (3000+1200) = 8500
	if dash.TotalTokens != 8500 {
		t.Errorf("TotalTokens: expected 8500, got %d", dash.TotalTokens)
	}

	// AvgDurationMins = 60000ms / 60000 = 1.0 min (all sessions have same duration)
	if dash.AvgDurationMins != 1.0 {
		t.Errorf("AvgDurationMins: expected 1.0, got %f", dash.AvgDurationMins)
	}

	// AvgTurnsPerSession = 10.0 (all sessions have 10 turns)
	if dash.AvgTurnsPerSession != 10.0 {
		t.Errorf("AvgTurnsPerSession: expected 10.0, got %f", dash.AvgTurnsPerSession)
	}

	// AcceptanceRate should be 0 for v1.
	if dash.AcceptanceRate != 0.0 {
		t.Errorf("AcceptanceRate: expected 0.0, got %f", dash.AcceptanceRate)
	}

	// HarnessBreakdown: 2 claude, 1 opencode.
	if dash.HarnessBreakdown[defaults.HarnessClaudeCode] != 2 {
		t.Errorf("HarnessBreakdown[claude]: expected 2, got %d", dash.HarnessBreakdown[defaults.HarnessClaudeCode])
	}
	if dash.HarnessBreakdown[defaults.HarnessOpenCode] != 1 {
		t.Errorf("HarnessBreakdown[opencode]: expected 1, got %d", dash.HarnessBreakdown[defaults.HarnessOpenCode])
	}
}

func TestStoreDataProvider_DashboardMetrics_Empty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	provider := api.NewStoreDataProvider(s, sessionvisibility.All())
	ctx := context.Background()

	dash, err := provider.DashboardMetrics(ctx)
	if err != nil {
		t.Fatalf("DashboardMetrics: %v", err)
	}
	if dash == nil {
		t.Fatal("DashboardMetrics: expected non-nil zeroed payload")
	}

	if dash.TotalSessions != 0 {
		t.Errorf("TotalSessions: expected 0, got %d", dash.TotalSessions)
	}
	if dash.TotalTokens != 0 {
		t.Errorf("TotalTokens: expected 0, got %d", dash.TotalTokens)
	}
	if dash.AvgDurationMins != 0.0 {
		t.Errorf("AvgDurationMins: expected 0.0, got %f", dash.AvgDurationMins)
	}
	if dash.AcceptanceRate != 0.0 {
		t.Errorf("AcceptanceRate: expected 0.0, got %f", dash.AcceptanceRate)
	}
}

// ---------------------------------------------------------------------------
// TrendsData
// ---------------------------------------------------------------------------

func TestStoreDataProvider_TrendsData(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "test-project", 10, 5, 60000),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800, "test-project", 10, 5, 60000),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash1, "github.com-test",
			defaults.HarnessOpenCode, day2Ms, 3000, 1200, "test-project", 10, 5, 60000),
	}

	provider := seedStore(t, s, entries)
	ctx := context.Background()

	trends, err := provider.TrendsData(ctx)
	if err != nil {
		t.Fatalf("TrendsData: %v", err)
	}
	if trends == nil {
		t.Fatal("TrendsData: expected non-nil payload")
	}

	if len(trends.Days) != 2 {
		t.Fatalf("TrendsData.Days: expected 2, got %d", len(trends.Days))
	}

	// Ordered by date_utc DESC from store: 2024-01-16 first.
	if trends.Days[0].Date != "2024-01-16" {
		t.Errorf("Days[0].Date: expected %q, got %q", "2024-01-16", trends.Days[0].Date)
	}
	if trends.Days[1].Date != "2024-01-15" {
		t.Errorf("Days[1].Date: expected %q, got %q", "2024-01-15", trends.Days[1].Date)
	}

	// Day 2 (row 0): 1 session, tokens = 3000+1200 = 4200.
	if trends.Days[0].Sessions != 1 {
		t.Errorf("Days[0].Sessions: expected 1, got %d", trends.Days[0].Sessions)
	}
	if trends.Days[0].Tokens != 4200 {
		t.Errorf("Days[0].Tokens: expected 4200, got %d", trends.Days[0].Tokens)
	}

	// Day 1 (row 1): 2 sessions, tokens = (1000+500)+(2000+800) = 4300.
	if trends.Days[1].Sessions != 2 {
		t.Errorf("Days[1].Sessions: expected 2, got %d", trends.Days[1].Sessions)
	}
	if trends.Days[1].Tokens != 4300 {
		t.Errorf("Days[1].Tokens: expected 4300, got %d", trends.Days[1].Tokens)
	}

	// TotalTokens = 4200 + 4300 = 8500.
	if trends.TotalTokens != 8500 {
		t.Errorf("TotalTokens: expected 8500, got %d", trends.TotalTokens)
	}
	// TotalSessions = 1 + 2 = 3.
	if trends.TotalSessions != 3 {
		t.Errorf("TotalSessions: expected 3, got %d", trends.TotalSessions)
	}
}

// ---------------------------------------------------------------------------
// QualitySessions
// ---------------------------------------------------------------------------

func TestStoreDataProvider_QualitySessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-alpha", 10, 5, 60000),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800, "project-alpha", 15, 8, 90000),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash2, "github.com-test",
			defaults.HarnessOpenCode, day2Ms, 3000, 1200, "project-beta", 20, 12, 120000),
	}

	provider := seedStore(t, s, entries)
	ctx := context.Background()

	// No filter: returns all sessions.
	all, err := provider.QualitySessions(ctx, api.QualityFilter{})
	if err != nil {
		t.Fatalf("QualitySessions(no filter): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("QualitySessions(no filter): expected 3, got %d", len(all))
	}

	// Verify mapping of first result (newest session, ordered DESC).
	qs := all[0]
	if qs.ID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("QualitySessions[0].ID: expected 33..., got %s", qs.ID)
	}
	if qs.Date != "2024-01-16" {
		t.Errorf("QualitySessions[0].Date: expected %q, got %q", "2024-01-16", qs.Date)
	}
	// V23+: Project = canonical_cwd = Project.FilePath = "/home/test/" + projectName.
	if qs.Project != "/home/test/project-beta" {
		t.Errorf("QualitySessions[0].Project: expected %q, got %q", "/home/test/project-beta", qs.Project)
	}
	if qs.TotalTokens != 4200 {
		t.Errorf("QualitySessions[0].TotalTokens: expected 4200, got %d", qs.TotalTokens)
	}
	if qs.InputTokens != 3000 {
		t.Errorf("QualitySessions[0].InputTokens: expected 3000, got %d", qs.InputTokens)
	}
	if qs.OutputTokens != 1200 {
		t.Errorf("QualitySessions[0].OutputTokens: expected 1200, got %d", qs.OutputTokens)
	}
	if qs.TurnCount != 20 {
		t.Errorf("QualitySessions[0].TurnCount: expected 20, got %d", qs.TurnCount)
	}
	if qs.ToolCalls != 12 {
		t.Errorf("QualitySessions[0].ToolCalls: expected 12, got %d", qs.ToolCalls)
	}
	expectedDurMins := 120000.0 / 60000.0 // 2.0
	if qs.DurationMinutes != expectedDurMins {
		t.Errorf("QualitySessions[0].DurationMinutes: expected %f, got %f", expectedDurMins, qs.DurationMinutes)
	}

	// v2 fields should all be zero-valued.
	if qs.Scope != "" || qs.Title != "" || qs.Outcome != "" {
		t.Errorf("v2 string fields should be empty: scope=%q title=%q outcome=%q", qs.Scope, qs.Title, qs.Outcome)
	}
	if qs.FilesTouched != 0 || qs.LinesChanged != 0 || qs.RetryLoops != 0 || qs.RetryTokensWasted != 0 || qs.WithinSessionReverts != 0 {
		t.Error("v2 int fields should be zero")
	}
}

func TestStoreDataProvider_QualitySessions_DateFilter(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-alpha", 10, 5, 60000),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800, "project-alpha", 15, 8, 90000),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash2, "github.com-test",
			defaults.HarnessOpenCode, day2Ms, 3000, 1200, "project-beta", 20, 12, 120000),
	}

	provider := seedStore(t, s, entries)
	ctx := context.Background()

	// Filter to only sessions on day 2 (2024-01-16).
	// StartFrom >= day1Ms+2000 excludes both day1 sessions (day1Ms and day1Ms+1000),
	// keeps session 3 (day2Ms = 1705363200000).
	startFrom := time.UnixMilli(day1Ms + 2000)
	endBefore := time.UnixMilli(day2Ms + 1)

	filtered, err := provider.QualitySessions(ctx, api.QualityFilter{
		DateRange: &api.DateRange{
			Start: startFrom,
			End:   endBefore,
		},
	})
	if err != nil {
		t.Fatalf("QualitySessions(date filter): %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("QualitySessions(date filter): expected 1, got %d", len(filtered))
	}
	if filtered[0].ID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("filtered[0].ID: expected 33..., got %s", filtered[0].ID)
	}
}

func TestStoreDataProvider_QualitySessions_ProjectFilter(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-alpha", 10, 5, 60000),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash2, "github.com-test",
			defaults.HarnessOpenCode, day2Ms, 3000, 1200, "project-beta", 20, 12, 120000),
	}

	provider := seedStore(t, s, entries)
	ctx := context.Background()

	// Filter to only "project-beta" by canonical_cwd path (V23+: ProjectName = canonical_cwd).
	filtered, err := provider.QualitySessions(ctx, api.QualityFilter{
		Projects: []string{"/home/test/project-beta"},
	})
	if err != nil {
		t.Fatalf("QualitySessions(project filter): %v", err)
	}
	if len(filtered) != 1 {
		t.Fatalf("QualitySessions(project filter): expected 1, got %d", len(filtered))
	}
	// V23+: Project = canonical_cwd.
	if filtered[0].Project != "/home/test/project-beta" {
		t.Errorf("filtered[0].Project: expected %q, got %q", "/home/test/project-beta", filtered[0].Project)
	}
}

// ---------------------------------------------------------------------------
// Pointer helpers (package api_test cannot access store_test helpers)
// ---------------------------------------------------------------------------

func intPtr(v int) *int                                         { return &v }
func int64Ptr(v int64) *int64                                   { return &v }
func strPtr(v string) *string                                   { return &v }
func float64Ptr(v float64) *float64                             { return &v }
func boolPtr(v bool) *bool                                      { return &v }
func outcomePtr(v ingest.SessionOutcome) *ingest.SessionOutcome { return &v }

// ---------------------------------------------------------------------------
// Integration: QualitySessions with non-null quality metrics
// ---------------------------------------------------------------------------

func TestStoreDataProvider_QualitySessions_WithQualityMetrics(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// 1. Insert a session.
	entry := makeStoreEntry(t, "44444444-4444-4444-4444-444444444444", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-quality", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	// 2. Save non-null quality metrics via SaveMetrics (the production write path).
	sid := ingest.SessionID("44444444-4444-4444-4444-444444444444")
	metrics := &ingest.SessionMetrics{
		SessionID: sid,
		QualityMetrics: schema.QualityMetrics{
			TitleGenerated:       strPtr("Fix authentication bug"),
			Outcome:              outcomePtr(ingest.OutcomeResolved),
			TotalTokens:          intPtr(1500),
			InputTokens:          intPtr(1000),
			OutputTokens:         intPtr(500),
			ToolCalls:            intPtr(5),
			TurnCount:            intPtr(10),
			SubagentCount:        intPtr(1),
			Scope:                strPtr("personal"),
			FilesTouched:         intPtr(3),
			LinesChanged:         intPtr(42),
			DurationMinutes:      float64Ptr(1.0),
			RetryLoops:           intPtr(2),
			RetryTokensWasted:    intPtr(300),
			WithinSessionReverts: intPtr(1),
			SignalDensity:        float64Ptr(0.75),
			SpecQualityScore:     float64Ptr(0.85),
			ExplorationRatio:     float64Ptr(0.20),
			ScopeBreadth:         intPtr(4),
			DiscoveryTurns:       intPtr(3),
			ComputeVersion:       intPtr(1),
		},
	}
	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	// 3. Read back via QualitySessions (exercises scanSessionRow cols 16-28).
	all, err := provider.QualitySessions(ctx, api.QualityFilter{})
	if err != nil {
		t.Fatalf("QualitySessions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("QualitySessions: expected 1, got %d", len(all))
	}

	qs := all[0]

	// Verify all 13 quality fields round-tripped through scanSessionRow → sessionRowToQuality.
	if qs.Title != "Fix authentication bug" {
		t.Errorf("Title: expected %q, got %q", "Fix authentication bug", qs.Title)
	}
	if qs.Outcome != string(ingest.OutcomeResolved) {
		t.Errorf("Outcome: expected %q, got %q", string(ingest.OutcomeResolved), qs.Outcome)
	}
	if qs.Scope != "personal" {
		t.Errorf("Scope: expected %q, got %q", "personal", qs.Scope)
	}
	if qs.FilesTouched != 3 {
		t.Errorf("FilesTouched: expected 3, got %d", qs.FilesTouched)
	}
	if qs.LinesChanged != 42 {
		t.Errorf("LinesChanged: expected 42, got %d", qs.LinesChanged)
	}
	if qs.RetryLoops != 2 {
		t.Errorf("RetryLoops: expected 2, got %d", qs.RetryLoops)
	}
	if qs.RetryTokensWasted != 300 {
		t.Errorf("RetryTokensWasted: expected 300, got %d", qs.RetryTokensWasted)
	}
	if qs.WithinSessionReverts != 1 {
		t.Errorf("WithinSessionReverts: expected 1, got %d", qs.WithinSessionReverts)
	}
	if qs.SignalDensity != 0.75 {
		t.Errorf("SignalDensity: expected 0.75, got %f", qs.SignalDensity)
	}
	if qs.SpecQualityScore != 0.85 {
		t.Errorf("SpecQualityScore: expected 0.85, got %f", qs.SpecQualityScore)
	}
	if qs.ExplorationRatio != 0.20 {
		t.Errorf("ExplorationRatio: expected 0.20, got %f", qs.ExplorationRatio)
	}
	if qs.ScopeBreadth != 4 {
		t.Errorf("ScopeBreadth: expected 4, got %d", qs.ScopeBreadth)
	}
	if qs.DiscoveryTurns != 3 {
		t.Errorf("DiscoveryTurns: expected 3, got %d", qs.DiscoveryTurns)
	}
}

// ---------------------------------------------------------------------------
// Integration: SessionByID with session entries → Turns wiring
// ---------------------------------------------------------------------------

func TestStoreDataProvider_SessionByID_WithEntries(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// 1. Insert a session.
	entry := makeStoreEntry(t, "55555555-5555-5555-5555-555555555555", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-turns", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	// 2. Index session entries (text + tool_use + system).
	sid := ingest.SessionID("55555555-5555-5555-5555-555555555555")
	entries := []schema.SessionEntry{
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("Please fix the login bug"),
			HasToolUse:     false,
		},
		{
			SessionID:      sid,
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeToolUse,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000001000),
			ContentPreview: strPtr("Reading auth.go..."),
			HasToolUse:     true,
			ToolNamesCSV:   strPtr("Read"),
			ToolCallID:     strPtr("tool-abc"),
			ToolInput:      strPtr(`{"file":"auth.go"}`),
			ToolOutput:     strPtr("file contents here"),
		},
		{
			SessionID:      sid,
			EntryIndex:     2,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleSystem,
			TimestampMs:    int64Ptr(1700000002000),
			ContentPreview: strPtr("System prompt"),
			HasToolUse:     false,
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// 3. Read back via SessionByID (exercises ListEntries → entriesToTurns).
	sess, err := provider.SessionByID(ctx, "55555555-5555-5555-5555-555555555555")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}

	// Verify turns are populated.
	if len(sess.Turns) != 3 {
		t.Fatalf("Turns: expected 3, got %d", len(sess.Turns))
	}

	// Turn 0: user text.
	t0 := sess.Turns[0]
	if t0.Index != 0 {
		t.Errorf("Turns[0].Index: expected 0, got %d", t0.Index)
	}
	if t0.Role != ingest.RoleUser {
		t.Errorf("Turns[0].Role: expected %q, got %q", ingest.RoleUser, t0.Role)
	}
	if t0.Content != "Please fix the login bug" {
		t.Errorf("Turns[0].Content: expected %q, got %q", "Please fix the login bug", t0.Content)
	}
	if t0.Timestamp.UnixMilli() != 1700000000000 {
		t.Errorf("Turns[0].Timestamp: expected 1700000000000ms, got %d", t0.Timestamp.UnixMilli())
	}
	if len(t0.ToolCalls) != 0 {
		t.Errorf("Turns[0].ToolCalls: expected 0, got %d", len(t0.ToolCalls))
	}

	// Turn 1: assistant tool_use.
	t1 := sess.Turns[1]
	if t1.Role != ingest.RoleAssistant {
		t.Errorf("Turns[1].Role: expected %q, got %q", ingest.RoleAssistant, t1.Role)
	}
	if len(t1.ToolCalls) != 1 {
		t.Fatalf("Turns[1].ToolCalls: expected 1, got %d", len(t1.ToolCalls))
	}
	tc := t1.ToolCalls[0]
	if tc.Name != "Read" {
		t.Errorf("ToolCalls[0].Name: expected %q, got %q", "Read", tc.Name)
	}
	if tc.ID != "tool-abc" {
		t.Errorf("ToolCalls[0].ID: expected %q, got %q", "tool-abc", tc.ID)
	}
	if tc.Arguments != `{"file":"auth.go"}` {
		t.Errorf("ToolCalls[0].Arguments: expected %q, got %q", `{"file":"auth.go"}`, tc.Arguments)
	}
	if tc.Result != "file contents here" {
		t.Errorf("ToolCalls[0].Result: expected %q, got %q", "file contents here", tc.Result)
	}

	// Turn 2: system entry.
	t2 := sess.Turns[2]
	if t2.Role != ingest.RoleSystem {
		t.Errorf("Turns[2].Role: expected %q, got %q", ingest.RoleSystem, t2.Role)
	}
	if t2.Content != "System prompt" {
		t.Errorf("Turns[2].Content: expected %q, got %q", "System prompt", t2.Content)
	}
}

// ---------------------------------------------------------------------------
// Integration: SessionSummaries preview field (first user message)
// ---------------------------------------------------------------------------

// TestStoreDataProvider_SessionSummaries_Preview verifies the Preview field is
// populated from the content_preview of the first top-level user entry — the
// already-redacted indexed transcript text — and is empty when no user entry
// exists.
func TestStoreDataProvider_SessionSummaries_Preview(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	const (
		withPreviewID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		noUserID      = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	)

	// Two sessions: one with user entries, one with only an assistant entry.
	withPreview := makeStoreEntry(t, withPreviewID, hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-preview", 4, 2, 60000)
	noUser := makeStoreEntry(t, noUserID, hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms+1000, 800, 400, "project-preview", 2, 1, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{withPreview, noUser})

	// Index entries for the first session: a depth=1 content part precedes the
	// depth=0 user message in entry_index order, proving the subquery filters on
	// depth=0 and orders by entry_index.
	sidWith := ingest.SessionID(withPreviewID)
	if err := s.IndexSessionEntries(ctx, sidWith, []schema.SessionEntry{
		{
			SessionID:      sidWith,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			Depth:          1, // content part — must be ignored
			ContentPreview: strPtr("nested content part"),
		},
		{
			SessionID:      sidWith,
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			Depth:          0,
			ContentPreview: strPtr("Add dark mode to settings page"),
		},
		{
			SessionID:      sidWith,
			EntryIndex:     2,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			Depth:          0,
			ContentPreview: strPtr("Sure, I'll add that."),
		},
		{
			SessionID:      sidWith,
			EntryIndex:     3,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			Depth:          0,
			ContentPreview: strPtr("Second user message — must not win"),
		},
	}); err != nil {
		t.Fatalf("IndexSessionEntries(withPreview): %v", err)
	}

	// Second session: only an assistant entry, no user entry.
	sidNoUser := ingest.SessionID(noUserID)
	if err := s.IndexSessionEntries(ctx, sidNoUser, []schema.SessionEntry{
		{
			SessionID:      sidNoUser,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			Depth:          0,
			ContentPreview: strPtr("Assistant-only entry"),
		},
	}); err != nil {
		t.Fatalf("IndexSessionEntries(noUser): %v", err)
	}

	summaries, err := provider.SessionSummaries(ctx)
	if err != nil {
		t.Fatalf("SessionSummaries: %v", err)
	}

	byID := make(map[string]api.SessionSummary, len(summaries))
	for _, sm := range summaries {
		byID[sm.ID] = sm
	}

	got, ok := byID[withPreviewID]
	if !ok {
		t.Fatalf("SessionSummaries: missing session %s", withPreviewID)
	}
	if got.Preview != "Add dark mode to settings page" {
		t.Errorf("Preview: expected first depth=0 user message, got %q", got.Preview)
	}
	// Summaries carry the opaque project hash so the frontend
	// can resolve display name → hash for the Map/Review REST endpoints.
	if got.ProjectHash != hash1 {
		t.Errorf("ProjectHash: expected %q, got %q", hash1, got.ProjectHash)
	}

	gotNoUser, ok := byID[noUserID]
	if !ok {
		t.Fatalf("SessionSummaries: missing session %s", noUserID)
	}
	if gotNoUser.Preview != "" {
		t.Errorf("Preview: expected empty for session with no user entry, got %q", gotNoUser.Preview)
	}
}

// ---------------------------------------------------------------------------
// Integration: SessionByID with Depth and ParentIndex
// ---------------------------------------------------------------------------

func TestStoreDataProvider_SessionByID_DepthAndParentIndex(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// 1. Insert a session.
	entry := makeStoreEntry(t, "77777777-7777-7777-7777-777777777777", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-depth", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	// 2. Index session entries with non-zero Depth and non-nil ParentIndex.
	sid := ingest.SessionID("77777777-7777-7777-7777-777777777777")
	entries := []schema.SessionEntry{
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("Initial user message"),
			Depth:          0,
		},
		{
			SessionID:      sid,
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000001000),
			ContentPreview: strPtr("Top-level response"),
			Depth:          0,
		},
		{
			SessionID:      sid,
			EntryIndex:     2,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			TimestampMs:    int64Ptr(1700000002000),
			ContentPreview: strPtr("Nested content part"),
			Depth:          1,
			ParentIndex:    intPtr(0),
		},
		{
			SessionID:      sid,
			EntryIndex:     3,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000003000),
			ContentPreview: strPtr("Nested assistant response"),
			Depth:          1,
			ParentIndex:    intPtr(0),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// 3. Read back via SessionByID.
	sess, err := provider.SessionByID(ctx, "77777777-7777-7777-7777-777777777777")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}

	if len(sess.Turns) != 4 {
		t.Fatalf("Turns: expected 4, got %d", len(sess.Turns))
	}

	// Turn 0: depth=0, ParentIndex=nil.
	if sess.Turns[0].Depth != 0 {
		t.Errorf("Turns[0].Depth: expected 0, got %d", sess.Turns[0].Depth)
	}
	if sess.Turns[0].ParentIndex != nil {
		t.Errorf("Turns[0].ParentIndex: expected nil, got %v", sess.Turns[0].ParentIndex)
	}

	// Turn 1: depth=0, ParentIndex=nil.
	if sess.Turns[1].Depth != 0 {
		t.Errorf("Turns[1].Depth: expected 0, got %d", sess.Turns[1].Depth)
	}
	if sess.Turns[1].ParentIndex != nil {
		t.Errorf("Turns[1].ParentIndex: expected nil, got %v", sess.Turns[1].ParentIndex)
	}

	// Turn 2: depth=1, ParentIndex=0.
	if sess.Turns[2].Depth != 1 {
		t.Errorf("Turns[2].Depth: expected 1, got %d", sess.Turns[2].Depth)
	}
	if sess.Turns[2].ParentIndex == nil {
		t.Fatal("Turns[2].ParentIndex: expected non-nil, got nil")
	}
	if *sess.Turns[2].ParentIndex != 0 {
		t.Errorf("Turns[2].ParentIndex: expected 0, got %d", *sess.Turns[2].ParentIndex)
	}

	// Turn 3: depth=1, ParentIndex=0.
	if sess.Turns[3].Depth != 1 {
		t.Errorf("Turns[3].Depth: expected 1, got %d", sess.Turns[3].Depth)
	}
	if sess.Turns[3].ParentIndex == nil {
		t.Fatal("Turns[3].ParentIndex: expected non-nil, got nil")
	}
	if *sess.Turns[3].ParentIndex != 0 {
		t.Errorf("Turns[3].ParentIndex: expected 0, got %d", *sess.Turns[3].ParentIndex)
	}
}

// ---------------------------------------------------------------------------
// Integration: SessionByID — ToolKind and StopReason round-trip (push-v2)
// ---------------------------------------------------------------------------

// TestStoreDataProvider_SessionByID_ToolKindAndStopReason verifies that
// ToolKind and StopReason written via IndexSessionEntries are preserved through
// the store and surfaced on the ingest.Turn via SessionByID.
//
// ToolKind is set on depth=1 tool_use entries; StopReason on depth=0 assistant
// messages. Both are push-v2 additions to the session_entries schema.
func TestStoreDataProvider_SessionByID_ToolKindAndStopReason(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "88888888-8888-8888-8888-888888888888", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-toolkind", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("88888888-8888-8888-8888-888888888888")
	toolKind := schema.ToolCallKindRead
	stopReason := schema.StopReasonEndTurn
	entries := []schema.SessionEntry{
		// depth=0 assistant message with stop_reason
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("I will read the file."),
			HasToolUse:     true,
			StopReason:     &stopReason,
			Depth:          0,
		},
		// depth=1 tool_use entry with ToolKind and ToolCallID
		{
			SessionID:    sid,
			EntryIndex:   1,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000001000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Read"),
			ToolCallID:   strPtr("toolu_r1"),
			ToolKind:     &toolKind,
			Depth:        1,
			ParentIndex:  intPtr(0),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "88888888-8888-8888-8888-888888888888")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}
	// After the depth=1 fold, the tool_use entry is folded into its depth=0
	// parent. Only 1 turn is emitted: the depth=0 assistant message with the
	// ToolCall attached.
	if len(sess.Turns) != 1 {
		t.Fatalf("Turns: expected 1 (depth=1 tool_use folded into parent), got %d", len(sess.Turns))
	}

	// Turn 0: depth=0 assistant, now carries folded ToolCall from depth=1 child.
	t0 := sess.Turns[0]
	if t0.Content != "I will read the file." {
		t.Errorf("Turns[0].Content: expected %q, got %q", "I will read the file.", t0.Content)
	}
	if len(t0.ToolCalls) != 1 {
		t.Fatalf("Turns[0].ToolCalls: expected 1 (folded from depth=1), got %d", len(t0.ToolCalls))
	}
	tc := t0.ToolCalls[0]
	if tc.ID != "toolu_r1" {
		t.Errorf("ToolCalls[0].ID: expected %q, got %q", "toolu_r1", tc.ID)
	}
	if tc.Name != "Read" {
		t.Errorf("ToolCalls[0].Name: expected %q, got %q", "Read", tc.Name)
	}
	if tc.ToolKind != schema.ToolCallKindRead {
		t.Errorf("ToolCalls[0].ToolKind: expected %q, got %q", schema.ToolCallKindRead, tc.ToolKind)
	}
}

// ---------------------------------------------------------------------------
// Unit: entriesToTurns edge cases
// ---------------------------------------------------------------------------

func TestStoreDataProvider_SessionByID_NoEntries(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Session seeded but no entries indexed — Turns should be nil.
	entry := makeStoreEntry(t, "66666666-6666-6666-6666-666666666666", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-empty", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sess, err := provider.SessionByID(context.Background(), "66666666-6666-6666-6666-666666666666")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess.Turns != nil {
		t.Errorf("Turns: expected nil for session with no entries, got %d turns", len(sess.Turns))
	}
}

// ---------------------------------------------------------------------------
// Integration: SessionByID — empty-entry suppression and consecutive dedup
// ---------------------------------------------------------------------------

// TestStoreDataProvider_SessionByID_EmptyEntrySuppression seeds a session with
// entries that include one entry whose ContentPreview is empty and HasToolUse is
// false (the DB-backed representation of an "empty" turn). It calls SessionByID
// and asserts the empty entry is absent from the returned Turns slice. This
// exercises the suppression logic end-to-end through IndexSessionEntries →
// ListEntries → EntriesToTurns → SessionByID.
func TestStoreDataProvider_SessionByID_EmptyEntrySuppression(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "10101010-1010-1010-1010-101010101010", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-empty-suppress", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("10101010-1010-1010-1010-101010101010")
	entries := []schema.SessionEntry{
		// Has content: keep.
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("Tell me about Go interfaces"),
			HasToolUse:     false,
		},
		// Empty content, no tools: must be suppressed.
		{
			SessionID:      sid,
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000001000),
			ContentPreview: strPtr(""),
			HasToolUse:     false,
		},
		// Has content: keep.
		{
			SessionID:      sid,
			EntryIndex:     2,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000002000),
			ContentPreview: strPtr("Go interfaces are implicit contracts"),
			HasToolUse:     false,
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "10101010-1010-1010-1010-101010101010")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}

	// The empty entry (index 1) must be suppressed; only 2 turns returned.
	if len(sess.Turns) != 2 {
		t.Fatalf("Turns: expected 2 after empty-entry suppression, got %d", len(sess.Turns))
	}
	if sess.Turns[0].Content != "Tell me about Go interfaces" {
		t.Errorf("Turns[0].Content: expected %q, got %q", "Tell me about Go interfaces", sess.Turns[0].Content)
	}
	if sess.Turns[1].Content != "Go interfaces are implicit contracts" {
		t.Errorf("Turns[1].Content: expected %q, got %q", "Go interfaces are implicit contracts", sess.Turns[1].Content)
	}
}

// TestStoreDataProvider_SessionByID_ConsecutiveDedup seeds a session with
// consecutive entries sharing the same role and identical content. It calls
// SessionByID and asserts that duplicate turns are collapsed to one, proving
// the dedup logic works end-to-end through IndexSessionEntries → ListEntries →
// EntriesToTurns → SessionByID.
func TestStoreDataProvider_SessionByID_ConsecutiveDedup(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "20202020-2020-2020-2020-202020202020", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-dedup", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("20202020-2020-2020-2020-202020202020")
	entries := []schema.SessionEntry{
		// First occurrence: keep.
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("Processing your request…"),
			HasToolUse:     false,
		},
		// Consecutive same role + same content: must be deduplicated.
		{
			SessionID:      sid,
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000001000),
			ContentPreview: strPtr("Processing your request…"),
			HasToolUse:     false,
		},
		// Different content: keep.
		{
			SessionID:      sid,
			EntryIndex:     2,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000002000),
			ContentPreview: strPtr("Done."),
			HasToolUse:     false,
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "20202020-2020-2020-2020-202020202020")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}

	// The duplicate (index 1) must be collapsed; only 2 turns returned.
	if len(sess.Turns) != 2 {
		t.Fatalf("Turns: expected 2 after consecutive dedup, got %d", len(sess.Turns))
	}
	if sess.Turns[0].Content != "Processing your request…" {
		t.Errorf("Turns[0].Content: expected %q, got %q", "Processing your request…", sess.Turns[0].Content)
	}
	// The first occurrence is kept; its original EntryIndex should be 0.
	if sess.Turns[0].Index != 0 {
		t.Errorf("Turns[0].Index: expected 0 (first occurrence kept), got %d", sess.Turns[0].Index)
	}
	if sess.Turns[1].Content != "Done." {
		t.Errorf("Turns[1].Content: expected %q, got %q", "Done.", sess.Turns[1].Content)
	}
}

// TestStoreDataProvider_SessionByID_PartType seeds a session with entries
// carrying PartType values and asserts each Turn carries the correct PartType
// through the full DB round-trip: IndexSessionEntries → ListEntries →
// EntriesToTurns → SessionByID.
func TestStoreDataProvider_SessionByID_PartType(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "30303030-3030-3030-3030-303030303030", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-part-type", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("30303030-3030-3030-3030-303030303030")
	entries := []schema.SessionEntry{
		// depth=0 message: PartType nil (not a content part).
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("Hello"),
			HasToolUse:     false,
			Depth:          0,
			PartType:       nil,
		},
		// depth=1 reasoning part.
		{
			SessionID:      sid,
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeThinking,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000001000),
			ContentPreview: strPtr("thinking…"),
			HasToolUse:     false,
			Depth:          1,
			ParentIndex:    intPtr(0),
			PartType:       strPtr("reasoning"),
		},
		// depth=1 step-start part.
		{
			SessionID:      sid,
			EntryIndex:     2,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000002000),
			ContentPreview: strPtr(""),
			HasToolUse:     false,
			Depth:          1,
			ParentIndex:    intPtr(0),
			PartType:       strPtr("step-start"),
		},
		// depth=1 text part.
		{
			SessionID:      sid,
			EntryIndex:     3,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000003000),
			ContentPreview: strPtr("Here is the answer."),
			HasToolUse:     false,
			Depth:          1,
			ParentIndex:    intPtr(0),
			PartType:       strPtr("text"),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "30303030-3030-3030-3030-303030303030")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}

	// Entry 0 (depth=0, nil PartType) + entry 3 (depth=1 text with content) survive
	// empty suppression. Entries 1 (thinking) and 2 (step-start, empty content) are
	// handled: entry 1 has content so survives; entry 2 is empty so suppressed.
	// Entry 1 (reasoning/thinking) has content → kept.
	// Total: 3 surviving turns (indices 0, 1, 3); entry 2 (step-start, empty) suppressed.

	// Assert turn count: entry 2 (step-start with empty ContentPreview and no tool calls)
	// must be suppressed by the empty-entry suppression logic in EntriesToTurns.
	if len(sess.Turns) != 3 {
		t.Errorf("len(sess.Turns): expected 3 (step-start suppressed), got %d", len(sess.Turns))
	}

	// Find each surviving turn by EntryIndex.
	byIndex := make(map[int]ingest.Turn)
	for _, turn := range sess.Turns {
		byIndex[turn.Index] = turn
	}

	// Entry 2 (step-start, empty ContentPreview) must be absent — suppressed.
	if _, present := byIndex[2]; present {
		t.Error("Turn[2] (step-start, empty content): should be suppressed by EntriesToTurns, but found in session turns")
	}

	// depth=0 turn: PartType must be nil.
	if t0, ok := byIndex[0]; ok {
		if t0.PartType != nil {
			t.Errorf("Turn[0].PartType: expected nil (depth=0), got %q", *t0.PartType)
		}
	} else {
		t.Error("Turn[0] (depth=0): missing from session turns")
	}

	// depth=1 reasoning turn: PartType must be "reasoning".
	if t1, ok := byIndex[1]; ok {
		if t1.PartType == nil {
			t.Error("Turn[1].PartType: expected \"reasoning\", got nil")
		} else if *t1.PartType != "reasoning" {
			t.Errorf("Turn[1].PartType: expected \"reasoning\", got %q", *t1.PartType)
		}
	} else {
		t.Error("Turn[1] (depth=1 reasoning): missing from session turns")
	}

	// depth=1 text turn: PartType must be "text".
	if t3, ok := byIndex[3]; ok {
		if t3.PartType == nil {
			t.Error("Turn[3].PartType: expected \"text\", got nil")
		} else if *t3.PartType != "text" {
			t.Errorf("Turn[3].PartType: expected \"text\", got %q", *t3.PartType)
		}
	} else {
		t.Error("Turn[3] (depth=1 text): missing from session turns")
	}
}

// ---------------------------------------------------------------------------
// Tool call enrichment: DurationMs, FilePath, ExitCode
// ---------------------------------------------------------------------------

func TestStoreDataProvider_SessionByID_ToolCallDuration(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-duration", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	entries := []schema.SessionEntry{
		// tool_use entry at ts=1000
		{
			SessionID:    sid,
			EntryIndex:   0,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000000000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Read"),
			ToolCallID:   strPtr("tool-dur-1"),
			ToolInput:    strPtr(`{"file_path":"/src/main.go"}`),
		},
		// tool_result entry at ts=1350 (350ms later)
		{
			SessionID:    sid,
			EntryIndex:   1,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000000350),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Read"),
			ToolCallID:   strPtr("tool-dur-1"),
			ToolOutput:   strPtr("file contents"),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}

	// The tool_use entry (index 0) should have a ToolCall with DurationMs=350.
	// The tool_result entry (index 1) also creates a ToolCall (both have ToolCallID)
	// but with dur=0; we only care about the tool_use entry's duration.
	var found bool
	for _, turn := range sess.Turns {
		if turn.Index != 0 {
			continue // skip the tool_result entry
		}
		for _, tc := range turn.ToolCalls {
			if tc.ID == "tool-dur-1" {
				found = true
				if tc.DurationMs == nil {
					t.Fatal("ToolCall.DurationMs: expected non-nil, got nil")
				}
				if *tc.DurationMs != 350 {
					t.Errorf("ToolCall.DurationMs: expected 350, got %d", *tc.DurationMs)
				}
			}
		}
	}
	if !found {
		t.Error("expected to find tool call tool-dur-1 on turn index 0")
	}
}

func TestStoreDataProvider_SessionByID_ToolCallFilePath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-filepath", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	entries := []schema.SessionEntry{
		// file_path key
		{
			SessionID:    sid,
			EntryIndex:   0,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000000000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Read"),
			ToolCallID:   strPtr("tool-fp-1"),
			ToolInput:    strPtr(`{"file_path":"/src/main.go"}`),
		},
		// notebook_path key
		{
			SessionID:    sid,
			EntryIndex:   1,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000001000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("NotebookEdit"),
			ToolCallID:   strPtr("tool-fp-2"),
			ToolInput:    strPtr(`{"notebook_path":"/notebooks/analysis.ipynb","new_source":"print(1)"}`),
		},
		// path key
		{
			SessionID:    sid,
			EntryIndex:   2,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000002000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Glob"),
			ToolCallID:   strPtr("tool-fp-3"),
			ToolInput:    strPtr(`{"path":"/workspace","pattern":"*.go"}`),
		},
		// no path keys (Bash with command)
		{
			SessionID:    sid,
			EntryIndex:   3,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000003000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Bash"),
			ToolCallID:   strPtr("tool-fp-4"),
			ToolInput:    strPtr(`{"command":"ls -la"}`),
		},
		// invalid JSON
		{
			SessionID:    sid,
			EntryIndex:   4,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000004000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Read"),
			ToolCallID:   strPtr("tool-fp-5"),
			ToolInput:    strPtr(`not json`),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}

	// Collect file paths by tool call ID.
	filePaths := make(map[string]string)
	for _, turn := range sess.Turns {
		for _, tc := range turn.ToolCalls {
			filePaths[tc.ID] = tc.FilePath
		}
	}

	cases := []struct {
		id       string
		expected string
	}{
		{"tool-fp-1", "/src/main.go"},
		{"tool-fp-2", "/notebooks/analysis.ipynb"},
		{"tool-fp-3", "/workspace"},
		{"tool-fp-4", ""}, // no path keys
		{"tool-fp-5", ""}, // invalid JSON
	}
	for _, c := range cases {
		if got := filePaths[c.id]; got != c.expected {
			t.Errorf("FilePath[%s]: expected %q, got %q", c.id, c.expected, got)
		}
	}
}

func TestStoreDataProvider_SessionByID_ToolCallExitCode(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "cccccccc-cccc-cccc-cccc-cccccccccccc", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-exitcode", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("cccccccc-cccc-cccc-cccc-cccccccccccc")
	entries := []schema.SessionEntry{
		// Bash with non-zero exit code
		{
			SessionID:    sid,
			EntryIndex:   0,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000000000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Bash"),
			ToolCallID:   strPtr("tool-ec-1"),
			ToolInput:    strPtr(`{"command":"false"}`),
			ToolOutput:   strPtr("Exit code 1\ncommand not found"),
		},
		// Bash with success (no exit code prefix)
		{
			SessionID:    sid,
			EntryIndex:   1,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000001000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Bash"),
			ToolCallID:   strPtr("tool-ec-2"),
			ToolInput:    strPtr(`{"command":"echo hello"}`),
			ToolOutput:   strPtr("hello"),
		},
		// Non-Bash tool — exit code should not be set
		{
			SessionID:    sid,
			EntryIndex:   2,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000002000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Read"),
			ToolCallID:   strPtr("tool-ec-3"),
			ToolInput:    strPtr(`{"file_path":"/src/main.go"}`),
			ToolOutput:   strPtr("Exit code 1\nfake"),
		},
		// Bash with exit code 127
		{
			SessionID:    sid,
			EntryIndex:   3,
			Harness:      defaults.HarnessClaudeCode,
			EntryType:    ingest.EntryTypeToolUse,
			Role:         ingest.RoleAssistant,
			TimestampMs:  int64Ptr(1700000003000),
			HasToolUse:   true,
			ToolNamesCSV: strPtr("Bash"),
			ToolCallID:   strPtr("tool-ec-4"),
			ToolInput:    strPtr(`{"command":"nonexistent"}`),
			ToolOutput:   strPtr("Exit code 127\ncommand not found"),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "cccccccc-cccc-cccc-cccc-cccccccccccc")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}

	// Collect exit codes by tool call ID.
	exitCodes := make(map[string]*int)
	for _, turn := range sess.Turns {
		for _, tc := range turn.ToolCalls {
			exitCodes[tc.ID] = tc.ExitCode
		}
	}

	// tool-ec-1: Bash "Exit code 1" → *int(1)
	if ec := exitCodes["tool-ec-1"]; ec == nil || *ec != 1 {
		t.Errorf("ExitCode[tool-ec-1]: expected *1, got %v", ec)
	}
	// tool-ec-2: Bash success → nil
	if ec := exitCodes["tool-ec-2"]; ec != nil {
		t.Errorf("ExitCode[tool-ec-2]: expected nil (success), got %d", *ec)
	}
	// tool-ec-3: Non-Bash tool → nil (exit code not parsed for non-Bash)
	if ec := exitCodes["tool-ec-3"]; ec != nil {
		t.Errorf("ExitCode[tool-ec-3]: expected nil (non-Bash), got %d", *ec)
	}
	// tool-ec-4: Bash "Exit code 127" → *int(127)
	if ec := exitCodes["tool-ec-4"]; ec == nil || *ec != 127 {
		t.Errorf("ExitCode[tool-ec-4]: expected *127, got %v", ec)
	}
}

// ---------------------------------------------------------------------------
// SessionByID: detail fields (project, model, gitBranch, gitRemote, projectPath)
// ---------------------------------------------------------------------------

func TestStoreDataProvider_SessionByID_DetailFields(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "dddddddd-dddd-dddd-dddd-dddddddddddd", hash1, "github.com-user-repo1",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-detail", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sess, err := provider.SessionByID(ctx, "dddddddd-dddd-dddd-dddd-dddddddddddd")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil {
		t.Fatal("SessionByID: expected non-nil session")
	}

	// Project comes from canonical_cwd = Project.FilePath (V23+: project_name removed).
	if sess.Project != "/home/test/project-detail" {
		t.Errorf("Project: expected %q, got %q", "/home/test/project-detail", sess.Project)
	}

	// Model comes from makeStoreEntry's default model.
	if sess.Model != "claude-opus-4-6" {
		t.Errorf("Model: expected %q, got %q", "claude-opus-4-6", sess.Model)
	}

	// ProjectPath comes from makeStoreEntry's project path.
	if sess.ProjectPath != "/home/test/project-detail" {
		t.Errorf("ProjectPath: expected %q, got %q", "/home/test/project-detail", sess.ProjectPath)
	}

	// PushedAt should be nil by default.
	if sess.PushedAt != nil {
		t.Errorf("PushedAt: expected nil, got %v", *sess.PushedAt)
	}
}

// ---------------------------------------------------------------------------
// Integration: SessionByID enriches Quality with the full session_metrics row
// (M-series + cost signals not present on the SessionRow detail columns) that
// the Highlights scorecard consumes.
// ---------------------------------------------------------------------------

func TestStoreDataProvider_SessionByID_ScorecardMetrics(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	const sidStr = "55555555-5555-5555-5555-555555555555"
	entry := makeStoreEntry(t, sidStr, hash1, "github.com-user-repo1",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-scorecard", 10, 5, 60000)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	// Save metrics carrying the M-series + cost signals that only live in
	// session_metrics (not on the detail row's quality columns).
	metrics := &ingest.SessionMetrics{
		SessionID: ingest.SessionID(sidStr),
		QualityMetrics: schema.QualityMetrics{
			Outcome:                 outcomePtr(ingest.OutcomeFailed),
			TotalTokens:             intPtr(1500),
			M2TokenOutcomeRatio:     float64Ptr(0.62),
			M5ContextUtilizationPct: float64Ptr(82.0),
			M6OutputSurvivalPct:     float64Ptr(41.0),
			RetryTokensWasted:       intPtr(420),
			CostTotalUSD:            float64Ptr(2.37),
			SpecQualityScore:        float64Ptr(33.0),
			SignalDensity:           float64Ptr(24.0),
			M7SpecHasExamples:       boolPtr(false),
			M7SpecHasConstraints:    boolPtr(true),
			M4ConsecutiveErrorMax:   intPtr(5),
			WithinSessionReverts:    intPtr(3),
			ComputeVersion:          intPtr(1),
		},
	}
	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	sess, err := provider.SessionByID(ctx, sidStr)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if sess == nil || sess.Metadata.Quality == nil {
		t.Fatal("SessionByID: expected non-nil session with quality metrics")
	}

	// The enriched quality must carry the M-series + cost signals from
	// session_metrics, which the SessionRow detail columns do not expose.
	q := sess.Metadata.Quality
	if q.M2TokenOutcomeRatio == nil || *q.M2TokenOutcomeRatio != 0.62 {
		t.Errorf("M2TokenOutcomeRatio: expected 0.62, got %v", q.M2TokenOutcomeRatio)
	}
	if q.M5ContextUtilizationPct == nil || *q.M5ContextUtilizationPct != 82.0 {
		t.Errorf("M5ContextUtilizationPct: expected 82.0, got %v", q.M5ContextUtilizationPct)
	}
	if q.M6OutputSurvivalPct == nil || *q.M6OutputSurvivalPct != 41.0 {
		t.Errorf("M6OutputSurvivalPct: expected 41.0, got %v", q.M6OutputSurvivalPct)
	}
	if q.M4ConsecutiveErrorMax == nil || *q.M4ConsecutiveErrorMax != 5 {
		t.Errorf("M4ConsecutiveErrorMax: expected 5, got %v", q.M4ConsecutiveErrorMax)
	}
	if q.CostTotalUSD == nil || *q.CostTotalUSD != 2.37 {
		t.Errorf("CostTotalUSD: expected 2.37, got %v", q.CostTotalUSD)
	}
	if q.M7SpecHasExamples == nil || *q.M7SpecHasExamples {
		t.Errorf("M7SpecHasExamples: expected false, got %v", q.M7SpecHasExamples)
	}
}

// ---------------------------------------------------------------------------
// Integration: SessionByID — full-content overlay for the main-turn truncation
// bodies truncated around defaults.ContentPreviewLimit)
// ---------------------------------------------------------------------------

// TestStoreDataProvider_SessionByID_ContentOverlay is the production-code-path
// regression test for the reported bug: the session_detail WS channel (fed by
// SessionByID) was rendering turns cut off mid-word at ~2000 chars, because
// EntriesToTurns uses the DB's bounded content_preview directly with no
// overlay. This seeds a Codex session (deliberately not Claude — Codex and
// Claude Code share source_format "jsonl", so this also guards the
// harness-vs-format dispatch fix in transcript.BuildContentOverlay) whose
// session_entries carry a truncated 2000-char preview, but whose ORIGINAL
// source file (injected via NewStoreDataProviderWithFS + MemFS) has the full
// 2500-char message. SessionByID must return the full 2500 chars.
func TestStoreDataProvider_SessionByID_ContentOverlay(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	const sidStr = "eeeeeeee-ffff-0000-1111-222233334444"
	sid := ingest.SessionID(sidStr)
	srcPath := ingest.ResolvedPath("/codex/session.jsonl")

	longContent := strings.Repeat("Q", 2500)
	codexLine := fmt.Sprintf(
		`{"timestamp":"2026-07-22T00:00:00.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`,
		longContent,
	)
	fs := testutil.NewMemFS()
	if err := fs.WriteFile(string(srcPath), []byte(codexLine+"\n"), 0644); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}

	// Seed the sessions row directly (bypassing makeStoreEntry, which hardcodes
	// a fixed non-existent source path) so source_path/source_format/harness
	// point at the MemFS fixture above.
	ph, err := ingest.NewProjectHash(hash1)
	if err != nil {
		t.Fatalf("NewProjectHash: %v", err)
	}
	hs, err := ingest.NewHostSlug("github.com-test")
	if err != nil {
		t.Fatalf("NewHostSlug: %v", err)
	}
	model, err := ingest.NewModelID("gpt-5.6-codex")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	ingested := day1Ms + 60000
	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessCodex,
			Model:         model,
			HostSlug:      hs,
			Timestamp: ingest.TimestampInfo{
				Start:    day1Ms,
				End:      day1Ms + 60000,
				Ingested: &ingested,
			},
			Source: ingest.SourceInfo{
				FilePath: string(srcPath),
				Format:   ingest.SourceFormatJSONL,
			},
			Project: ingest.ProjectInfo{
				Hash:     ph,
				Name:     "project-overlay",
				FilePath: "/home/test/project-overlay",
			},
			Stats: ingest.StatsInfo{
				TurnCount:     1,
				ToolCallCount: 0,
				DurationMs:    60000,
				TokensIn:      100,
				TokensOut:     50,
			},
			Version:     "1.0.0",
			Subagents:   []ingest.SubagentRef{},
			Diagnostics: ingest.DiagnosticsInfo{Warnings: []ingest.DiagnosticEntry{}},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      ingest.HarnessCodex,
			SourcePath:   srcPath,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
	provider := seedStoreWithFS(t, s, []ingest.StoreEntry{entry}, fs)

	// Index the TRUNCATED entry (2000 chars) — what a real ingest run stores.
	codexIndexer := ingest.NewCodexIndexer(fs)
	dsEntries, err := codexIndexer.IndexTranscript(ctx, entry.Session)
	if err != nil {
		t.Fatalf("index codex fixture (truncated): %v", err)
	}
	if len(dsEntries) != 1 {
		t.Fatalf("indexed entry count: got %d, want 1", len(dsEntries))
	}
	if dsEntries[0].ContentPreview == nil || len(*dsEntries[0].ContentPreview) != 2000 {
		t.Fatalf("indexed preview: expected exactly 2000 chars (truncated), got %v", dsEntries[0].ContentPreview)
	}
	if err := s.IndexSessionEntries(ctx, sid, dsEntries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, sidStr)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if len(sess.Turns) != 1 {
		t.Fatalf("Turns length: got %d, want 1", len(sess.Turns))
	}
	if len(sess.Turns[0].Content) != len(longContent) {
		t.Fatalf("Turns[0].Content length: got %d, want %d (full content) — a length of exactly 2000 means "+
			"SessionByID is still using the DB's truncated content_preview instead of overlaying the source re-index",
			len(sess.Turns[0].Content), len(longContent))
	}
	if sess.Turns[0].Content != longContent {
		t.Errorf("Turns[0].Content: mismatch beyond length")
	}
}

// TestStoreDataProvider_SessionByID_ContentOverlay_MissingSourceDegradesGracefully
// verifies that when the source file used for the content overlay is missing
// (a real, common case — e.g. the source transcript was moved/deleted since
// ingest, or NewStoreDataProvider's default real-OS filesystem simply has
// nothing at the recorded path in a test), SessionByID still returns the
// session with its EXISTING (DB-preview) content rather than failing the
// whole request. The overlay is strictly best-effort.
func TestStoreDataProvider_SessionByID_ContentOverlay_MissingSourceDegradesGracefully(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := makeStoreEntry(t, "ffffffff-0000-1111-2222-333344445555", hash1, "github.com-test",
		defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-missing-source", 1, 0, 60000)
	// makeStoreEntry's SourcePath ("/test/path/session.jsonl") is never written
	// to any filesystem — the default real-OS-backed provider from seedStore
	// will fail to read it, exercising the graceful-degradation path.
	provider := seedStore(t, s, []ingest.StoreEntry{entry})

	sid := ingest.SessionID("ffffffff-0000-1111-2222-333344445555")
	dbEntries := []schema.SessionEntry{
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("some preview content"),
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, dbEntries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, "ffffffff-0000-1111-2222-333344445555")
	if err != nil {
		t.Fatalf("SessionByID: expected no error even when the overlay source file is missing, got: %v", err)
	}
	if len(sess.Turns) != 1 {
		t.Fatalf("Turns length: got %d, want 1", len(sess.Turns))
	}
	if sess.Turns[0].Content != "some preview content" {
		t.Errorf("Turns[0].Content: expected the DB preview to survive a missing overlay source, got %q", sess.Turns[0].Content)
	}
}

// readFileSpyFS wraps a *testutil.MemFS and counts ReadFile calls, so a test
// can assert the content-overlay re-index was never attempted (the perf
// gate, transcript.AnyContentTruncated) rather than merely attempted and
// harmlessly failing/no-opping — those two outcomes are otherwise
// indistinguishable from the returned Content alone.
type readFileSpyFS struct {
	*testutil.MemFS
	readFileCalls int
}

func (s *readFileSpyFS) ReadFile(path string) ([]byte, error) {
	s.readFileCalls++
	return s.MemFS.ReadFile(path)
}

// TestStoreDataProvider_SessionByID_ContentOverlay_SkipsReindexWhenNothingTruncated
// is the perf-gate regression test:
// BuildContentOverlay does a full re-parse of the source transcript from
// disk, which SessionByID must NOT pay on every session view — only when at
// least one turn's content_preview actually hit defaults.ContentPreviewLimit.
// Proven with a ReadFile-counting spy FileSystem: the source file is present
// and perfectly readable (so a missing/degraded-gracefully result can't mask
// a gate failure the way it did in the sibling test above), but every DB
// entry is short, so the spy must see ZERO ReadFile calls.
func TestStoreDataProvider_SessionByID_ContentOverlay_SkipsReindexWhenNothingTruncated(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	const sidStr = "12121212-3434-5656-7878-909090909090"
	sid := ingest.SessionID(sidStr)
	srcPath := ingest.ResolvedPath("/short/session.jsonl")

	spy := &readFileSpyFS{MemFS: testutil.NewMemFS()}
	shortLine := `{"type":"user","message":{"role":"user","content":"a perfectly ordinary, short turn"}}`
	if err := spy.WriteFile(string(srcPath), []byte(shortLine+"\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         ingest.ModelID("claude-opus-4-6"),
			HostSlug:      ingest.HostSlug("github-com-test"),
			Timestamp:     ingest.TimestampInfo{Start: day1Ms, End: day1Ms + 60000, Ingested: int64Ptr(day1Ms + 60000)},
			Source:        ingest.SourceInfo{FilePath: string(srcPath), Format: ingest.SourceFormatJSONL},
			Project: ingest.ProjectInfo{
				Hash:     ingest.ProjectHash(hash1),
				Name:     "project-gate",
				FilePath: "/home/test/project-gate",
			},
			Stats:       ingest.StatsInfo{TurnCount: 1, DurationMs: 60000},
			Subagents:   []ingest.SubagentRef{},
			Diagnostics: ingest.DiagnosticsInfo{Warnings: []ingest.DiagnosticEntry{}},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      ingest.HarnessClaudeCode,
			SourcePath:   srcPath,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
	provider := seedStoreWithFS(t, s, []ingest.StoreEntry{entry}, spy)

	shortContent := "a perfectly ordinary, short turn"
	dbEntries := []schema.SessionEntry{
		{
			SessionID:      sid,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			ContentPreview: &shortContent,
		},
	}
	if err := s.IndexSessionEntries(ctx, sid, dbEntries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	sess, err := provider.SessionByID(ctx, sidStr)
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if len(sess.Turns) != 1 || sess.Turns[0].Content != shortContent {
		t.Fatalf("Turns[0].Content: got %+v, want the DB preview %q verbatim", sess.Turns, shortContent)
	}
	if spy.readFileCalls != 0 {
		t.Errorf("ReadFile call count: got %d, want 0 — the perf gate should have skipped the content-overlay re-index entirely because nothing in this session was truncated", spy.readFileCalls)
	}
}

// ---------------------------------------------------------------------------
// Unit: rowToQualityMetrics sentinel — TurnCount>0, zero quality signals
// ---------------------------------------------------------------------------

// TestStoreAdapter_rowToQualityMetrics_NonNilQualityWhenTurnCountPositive tests
// that rowToQualityMetrics returns a non-nil *schema.QualityMetrics when TurnCount>0
// even when all other quality signals are zero/nil.
//
// Tested via the observable outcome of StoreDataProvider.Sessions(): sessions
// with TurnCount>0 must expose a non-nil Metadata.Quality carrying the correct counts.
func TestStoreAdapter_rowToQualityMetrics_NonNilQualityWhenTurnCountPositive(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Insert a session with TurnCount=5, TokensTotal=1000 (tokensIn=600, tokensOut=400),
	// and no quality signals (all quality-signal fields remain zero).
	entry := makeStoreEntry(
		t,
		"88888888-8888-8888-8888-888888888888",
		hash1,
		"github.com-user-repo1",
		defaults.HarnessClaudeCode,
		day1Ms,
		600, // tokensIn
		400, // tokensOut
		"project-sentinel",
		5,     // turnCount
		2,     // toolCallCount
		30000, // durationMs (30s)
	)
	provider := seedStore(t, s, []ingest.StoreEntry{entry})
	ctx := context.Background()

	sessions, err := provider.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("Sessions: expected 1, got %d", len(sessions))
	}

	q := sessions[0].Metadata.Quality
	if q == nil {
		t.Fatal("Metadata.Quality: expected non-nil when TurnCount>0, got nil")
	}
	if q.TurnCount == nil {
		t.Fatal("Quality.TurnCount: expected non-nil pointer, got nil")
	}
	if *q.TurnCount != 5 {
		t.Errorf("Quality.TurnCount: expected 5, got %d", *q.TurnCount)
	}
	if q.TotalTokens == nil {
		t.Fatal("Quality.TotalTokens: expected non-nil pointer, got nil")
	}
	if *q.TotalTokens != 1000 {
		t.Errorf("Quality.TotalTokens: expected 1000, got %d", *q.TotalTokens)
	}
	// DurationMinutes must also be populated.
	if q.DurationMinutes == nil {
		t.Fatal("Quality.DurationMinutes: expected non-nil pointer, got nil")
	}
	expectedDurMins := 30000.0 / 60000.0 // 0.5 minutes
	if *q.DurationMinutes != expectedDurMins {
		t.Errorf("Quality.DurationMinutes: expected %f, got %f", expectedDurMins, *q.DurationMinutes)
	}
}

// ---------------------------------------------------------------------------
// EntriesToTurns post-processing: empty entry suppression and consecutive dedup
// ---------------------------------------------------------------------------

// TestEntriesToTurns_EmptyEntrySuppression verifies that turns with no content
// AND no tool calls are removed from the output. Entries that have either
// content or tools must be preserved.
func TestEntriesToTurns_EmptyEntrySuppression(t *testing.T) {
	t.Parallel()
	sid := ingest.SessionID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

	entries := []schema.SessionEntry{
		// Has content: keep.
		{SessionID: sid, EntryIndex: 0, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser,
			ContentPreview: strPtr("Hello world")},
		// Empty content, no tools: suppress.
		{SessionID: sid, EntryIndex: 1, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("")},
		// Whitespace-only content, no tools: suppress.
		{SessionID: sid, EntryIndex: 2, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("   ")},
		// nil content, no tools: suppress.
		{SessionID: sid, EntryIndex: 3, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser,
			ContentPreview: nil},
		// Has content: keep.
		{SessionID: sid, EntryIndex: 4, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("Response")},
	}

	turns := api.EntriesToTurns(entries)
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns after suppression, got %d", len(turns))
	}
	if turns[0].Content != "Hello world" {
		t.Errorf("turns[0].Content: got %q, want %q", turns[0].Content, "Hello world")
	}
	if turns[1].Content != "Response" {
		t.Errorf("turns[1].Content: got %q, want %q", turns[1].Content, "Response")
	}
}

// TestEntriesToTurns_EmptyContentButHasTools verifies that an entry with empty
// content but tool calls is NOT suppressed.
func TestEntriesToTurns_EmptyContentButHasTools(t *testing.T) {
	t.Parallel()
	sid := ingest.SessionID("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")

	callID := "call-1"
	entries := []schema.SessionEntry{
		// Depth=0 assistant with empty content but has a depth=1 tool_use child.
		{SessionID: sid, EntryIndex: 0, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr(""), HasToolUse: true},
		// Depth=1 tool_use child.
		{SessionID: sid, EntryIndex: 1, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant,
			Depth: 1, ParentIndex: intPtr(0), ToolCallID: &callID, ToolNamesCSV: strPtr("Bash"),
			ToolInput: strPtr(`{"command":"ls"}`)},
	}

	turns := api.EntriesToTurns(entries)
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn (empty content but has tools must be kept), got %d", len(turns))
	}
	if len(turns[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(turns[0].ToolCalls))
	}
}

// TestEntriesToTurns_SystemEntryShortContent verifies that legitimate short
// system entries are not suppressed.
func TestEntriesToTurns_SystemEntryShortContent(t *testing.T) {
	t.Parallel()
	sid := ingest.SessionID("cccccccc-cccc-cccc-cccc-cccccccccccc")

	entries := []schema.SessionEntry{
		// Short system content: must be preserved.
		{SessionID: sid, EntryIndex: 0, EntryType: ingest.EntryTypeText, Role: ingest.RoleSystem,
			ContentPreview: strPtr("ok")},
		// Another short system entry.
		{SessionID: sid, EntryIndex: 1, EntryType: ingest.EntryTypeText, Role: ingest.RoleSystem,
			ContentPreview: strPtr("go")},
	}

	turns := api.EntriesToTurns(entries)
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns (short system entries must not be suppressed), got %d", len(turns))
	}
}

// TestEntriesToTurns_ConsecutiveSameRoleDedup verifies that consecutive turns
// with the same role and identical non-empty content are deduplicated, keeping
// the first occurrence when neither has tools.
func TestEntriesToTurns_ConsecutiveSameRoleDedup(t *testing.T) {
	t.Parallel()
	sid := ingest.SessionID("dddddddd-dddd-dddd-dddd-dddddddddddd")

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("Analyzing…")},
		// Consecutive same role+content: suppress second.
		{SessionID: sid, EntryIndex: 1, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("Analyzing…")},
		// Different content: keep.
		{SessionID: sid, EntryIndex: 2, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("Done.")},
	}

	turns := api.EntriesToTurns(entries)
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns after dedup, got %d", len(turns))
	}
	if turns[0].Index != 0 {
		t.Errorf("turns[0].Index: got %d, want 0", turns[0].Index)
	}
	if turns[1].Content != "Done." {
		t.Errorf("turns[1].Content: got %q, want %q", turns[1].Content, "Done.")
	}
}

// TestEntriesToTurns_DedupPreferToolVersion verifies that when a consecutive
// duplicate pair exists and the later entry has tool calls while the earlier
// does not, the later one (with tools) replaces the earlier.
func TestEntriesToTurns_DedupPreferToolVersion(t *testing.T) {
	t.Parallel()
	sid := ingest.SessionID("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")

	callID := "call-x"
	entries := []schema.SessionEntry{
		// First: same content, no tools.
		{SessionID: sid, EntryIndex: 0, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("Reading file…"), HasToolUse: false},
		// Second: same content, has tool_use child — should replace first.
		{SessionID: sid, EntryIndex: 1, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("Reading file…"), HasToolUse: true},
		{SessionID: sid, EntryIndex: 2, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant,
			Depth: 1, ParentIndex: intPtr(1), ToolCallID: &callID, ToolNamesCSV: strPtr("Read"),
			ToolInput: strPtr(`{"file_path":"foo.go"}`)},
	}

	turns := api.EntriesToTurns(entries)
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn (dedup + prefer tool version), got %d", len(turns))
	}
	if turns[0].Index != 1 {
		t.Errorf("turns[0].Index: got %d, want 1 (tool version should win)", turns[0].Index)
	}
	if len(turns[0].ToolCalls) != 1 {
		t.Fatalf("turns[0].ToolCalls: expected 1, got %d", len(turns[0].ToolCalls))
	}
}

// TestEntriesToTurns_DedupDifferentRolesNotDeduped verifies that consecutive
// entries with the same content but different roles are NOT deduplicated.
func TestEntriesToTurns_DedupDifferentRolesNotDeduped(t *testing.T) {
	t.Parallel()
	sid := ingest.SessionID("ffffffff-ffff-ffff-ffff-ffffffffffff")

	entries := []schema.SessionEntry{
		{SessionID: sid, EntryIndex: 0, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser,
			ContentPreview: strPtr("same content")},
		// Different role — must not be deduplicated.
		{SessionID: sid, EntryIndex: 1, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant,
			ContentPreview: strPtr("same content")},
	}

	turns := api.EntriesToTurns(entries)
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns (different roles, same content must not dedup), got %d", len(turns))
	}
}

// ---------------------------------------------------------------------------
// DB round-trip: canonical roles through ClaudeIndexer → store → EntriesToTurns
// ---------------------------------------------------------------------------

// TestEntriesToTurns_CanonicalRoles_DBRoundTrip verifies that canonical roles
// written by the Claude indexer survive the full pipeline:
//
//	ClaudeIndexer.IndexTranscript → store.IndexSessionEntries → store.ListEntries → EntriesToTurns
//
// The test uses two separate JSONL transcripts to cover the two canonical role scenarios:
//
// Transcript A (pure tool_result wrapper): an assistant turn with one regular Read tool_use
// followed by a user message wrapping only that tool_result. The indexer (R2) reclassifies
// the depth-0 user wrapper to role=tool. EntriesToTurns must suppress it.
//
// Transcript B (mixed wrapper with AskUserQuestion): an assistant turn with two tool_use
// blocks (AskUserQuestion + Read) followed by a user message with both tool_results.
// The depth-1 tool_result for AskUserQuestion keeps role=user (R3); the depth-1
// tool_result for Read is reclassified to role=tool (R1). Both are verified at the DB
// level. The depth-0 mixed wrapper keeps role=user (R2: not reclassified when anyAskUser),
// and is suppressed at the Turn level via empty-entry suppression (content migrated to
// children by R6, wrapper has no tool calls).
//
// EntriesToTurns previously had read-time compensating logic (a role override
// plus AskUserQuestion exception detection) that masked DB inconsistencies. The
// indexer now writes canonical roles and EntriesToTurns trusts them directly.
func TestEntriesToTurns_CanonicalRoles_DBRoundTrip(t *testing.T) {
	t.Parallel()

	// -----------------------------------------------------------------------
	// Sub-test A: Pure tool_result wrapper reclassified to role=tool by R2.
	// The depth-0 wrapper must be suppressed by the new role=tool rule in
	// EntriesToTurns. The assistant turn must be emitted with a folded ToolCall.
	// -----------------------------------------------------------------------
	t.Run("pure_tool_result_wrapper_suppressed", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		ctx := context.Background()

		const sessionUUID = "e1e1e1e1-e1e1-e1e1-e1e1-e1e1e1e1e1e1"
		entry := makeStoreEntry(t, sessionUUID, hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-canonical-a", 3, 1, 60000)
		seedStore(t, s, []ingest.StoreEntry{entry})

		// Line 1 (assistant): one Read tool_use.
		// Line 2 (user): one tool_result for Read. The indexer reclassifies
		// the depth-0 user wrapper to role=tool (all children are tool_result, none AskUser).
		//
		// Indexer produces:
		//   [0] depth=0 role=assistant type=tool_use
		//   [1] depth=1 role=assistant type=tool_use toolCallID=toolu_r1
		//   [2] depth=0 role=tool      type=tool_result  ← R2: wrapper reclassified
		//   [3] depth=1 role=tool      type=tool_result toolCallID=toolu_r1 ← R1
		const assistantLine = `{"type":"assistant","uuid":"a1","timestamp":"2024-01-15T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_r1","name":"Read","input":{"file_path":"/src/main.go"}}],"usage":{"input_tokens":50,"output_tokens":20}}}`
		const userLine = `{"type":"user","uuid":"u2","timestamp":"2024-01-15T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_r1","content":"package main\n\nfunc main() {}"}]}}`

		fs := testutil.NewMemFS()
		const transcriptPath = "/transcripts/pure-wrapper-test.jsonl"
		if err := fs.WriteFile(transcriptPath, []byte(assistantLine+"\n"+userLine+"\n"), 0644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}

		sid, err := ingest.NewSessionID(sessionUUID)
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
		indexedEntries, err := indexer.IndexTranscript(ctx, ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      defaults.HarnessClaudeCode,
			SourcePath:   ingest.ResolvedPath(transcriptPath),
			SourceFormat: ingest.SourceFormatJSONL,
		})
		if err != nil {
			t.Fatalf("IndexTranscript: %v", err)
		}
		if len(indexedEntries) == 0 {
			t.Fatal("IndexTranscript returned no entries")
		}

		// Verify indexer wrote canonical roles before storing.
		var depth0Tool *schema.SessionEntry
		for i := range indexedEntries {
			if indexedEntries[i].Depth == 0 && indexedEntries[i].Role == schema.RoleTool {
				depth0Tool = &indexedEntries[i]
			}
		}
		if depth0Tool == nil {
			t.Fatal("indexer must reclassify depth-0 pure tool_result wrapper to role=tool (R2)")
		}

		if err := s.IndexSessionEntries(ctx, sid, indexedEntries); err != nil {
			t.Fatalf("IndexSessionEntries: %v", err)
		}

		dbEntries, err := s.ListEntries(ctx, sid)
		if err != nil {
			t.Fatalf("ListEntries: %v", err)
		}

		// Verify the DB preserved the role=tool on the depth-0 wrapper.
		var dbDepth0Tool *schema.SessionEntry
		for i := range dbEntries {
			if dbEntries[i].Depth == 0 && dbEntries[i].Role == schema.RoleTool {
				dbDepth0Tool = &dbEntries[i]
			}
		}
		if dbDepth0Tool == nil {
			t.Fatal("DB round-trip: depth-0 role=tool entry not found — canonical role was not persisted")
		}

		// Run EntriesToTurns and verify the depth-0 tool wrapper is suppressed.
		turns := api.EntriesToTurns(dbEntries)
		for _, turn := range turns {
			if turn.Index == dbDepth0Tool.EntryIndex {
				t.Errorf("depth-0 tool wrapper (index=%d) must be suppressed by EntriesToTurns but was emitted as a turn", dbDepth0Tool.EntryIndex)
			}
		}

		// The assistant turn must be present with folded ToolCalls.
		var assistantTurns []ingest.Turn
		for _, turn := range turns {
			if turn.Role == ingest.RoleAssistant {
				assistantTurns = append(assistantTurns, turn)
			}
		}
		if len(assistantTurns) == 0 {
			t.Fatal("expected assistant turn in output, got none")
		}
		if len(assistantTurns[0].ToolCalls) == 0 {
			t.Error("assistant turn: expected folded ToolCalls from depth=1 tool_use, got none")
		}
	})

	// -----------------------------------------------------------------------
	// Sub-test B: AskUserQuestion tool_result keeps role=user (R3); regular
	// tool_result is reclassified to role=tool (R1). Both are verified via
	// DB entry roles. The depth-0 mixed wrapper (role=user, cleared preview)
	// is not emitted as a turn (suppressed by empty-entry suppression).
	// -----------------------------------------------------------------------
	t.Run("canonical_roles_in_db_entries", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		ctx := context.Background()

		const sessionUUID = "e3e3e3e3-e3e3-e3e3-e3e3-e3e3e3e3e3e3"
		entry := makeStoreEntry(t, sessionUUID, hash1, "github.com-test",
			defaults.HarnessClaudeCode, day1Ms, 1000, 500, "project-canonical-b", 4, 2, 60000)
		seedStore(t, s, []ingest.StoreEntry{entry})

		// Line 1 (assistant): AskUserQuestion + Read tool_use blocks.
		// Line 2 (user): tool_result for AskUserQuestion (kept role=user by R3) +
		//                tool_result for Read (reclassified role=tool by R1).
		// The depth-0 user wrapper stays role=user (mixed wrapper: anyAskUser=true, R2 skipped).
		//
		// Indexer produces:
		//   [0] depth=0 role=assistant type=tool_use
		//   [1] depth=1 role=assistant type=tool_use toolCallID=toolu_askuser1 name=AskUserQuestion
		//   [2] depth=1 role=assistant type=tool_use toolCallID=toolu_r1 name=Read
		//   [3] depth=0 role=user      type=text      ← mixed wrapper (anyAskUser=true)
		//   [4] depth=1 role=user      type=tool_result toolCallID=toolu_askuser1 ← R3
		//   [5] depth=1 role=tool      type=tool_result toolCallID=toolu_r1       ← R1
		const assistantLine = `{"type":"assistant","uuid":"a1","timestamp":"2024-01-15T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_askuser1","name":"AskUserQuestion","input":{"question":"Should I continue?"}},{"type":"tool_use","id":"toolu_r1","name":"Read","input":{"file_path":"/src/main.go"}}],"usage":{"input_tokens":50,"output_tokens":20}}}`
		const userLine = `{"type":"user","uuid":"u2","timestamp":"2024-01-15T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_askuser1","content":"Yes, please continue."},{"type":"tool_result","tool_use_id":"toolu_r1","content":"package main\n\nfunc main() {}"}]}}`

		fs := testutil.NewMemFS()
		const transcriptPath = "/transcripts/mixed-wrapper-test.jsonl"
		if err := fs.WriteFile(transcriptPath, []byte(assistantLine+"\n"+userLine+"\n"), 0644); err != nil {
			t.Fatalf("write transcript: %v", err)
		}

		sid, err := ingest.NewSessionID(sessionUUID)
		if err != nil {
			t.Fatalf("NewSessionID: %v", err)
		}
		indexer := ingest.NewClaudeIndexer(fs, ingest.WithClaudeFullDepth(true))
		indexedEntries, err := indexer.IndexTranscript(ctx, ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      defaults.HarnessClaudeCode,
			SourcePath:   ingest.ResolvedPath(transcriptPath),
			SourceFormat: ingest.SourceFormatJSONL,
		})
		if err != nil {
			t.Fatalf("IndexTranscript: %v", err)
		}

		if err := s.IndexSessionEntries(ctx, sid, indexedEntries); err != nil {
			t.Fatalf("IndexSessionEntries: %v", err)
		}

		dbEntries, err := s.ListEntries(ctx, sid)
		if err != nil {
			t.Fatalf("ListEntries: %v", err)
		}

		// Verify canonical roles in DB entries:
		//   - depth=1 tool_result for toolu_askuser1 must have role=user
		//   - depth=1 tool_result for toolu_r1 must have role=tool
		var askUserEntry, regularEntry *schema.SessionEntry
		for i := range dbEntries {
			e := &dbEntries[i]
			if e.Depth == 1 && e.EntryType == schema.EntryTypeToolResult && e.ToolCallID != nil {
				switch *e.ToolCallID {
				case "toolu_askuser1":
					askUserEntry = e
				case "toolu_r1":
					regularEntry = e
				}
			}
		}

		if askUserEntry == nil {
			t.Fatal("DB: depth=1 tool_result for toolu_askuser1 not found")
		}
		if askUserEntry.Role != schema.RoleUser {
			t.Errorf("DB: AskUserQuestion tool_result role: got %q, want %q (R3: genuine user answer must keep role=user)", askUserEntry.Role, schema.RoleUser)
		}

		if regularEntry == nil {
			t.Fatal("DB: depth=1 tool_result for toolu_r1 not found")
		}
		if regularEntry.Role != schema.RoleTool {
			t.Errorf("DB: regular tool_result role: got %q, want %q (R1: tool_result must be reclassified to role=tool)", regularEntry.Role, schema.RoleTool)
		}

		// Verify EntriesToTurns does NOT override roles — it must trust the DB.
		// The depth-1 tool_results are folded into the assistant's ToolCalls (not
		// emitted as separate turns), so we assert no depth=0 tool wrapper turn
		// and that the assistant turn has folded tool calls.
		turns := api.EntriesToTurns(dbEntries)

		// No depth=0 turn with role=tool should appear (pure wrapper suppressed).
		for _, turn := range turns {
			if turn.Depth == 0 && turn.Role == ingest.RoleTool {
				t.Errorf("depth=0 role=tool turn must be suppressed; found Turn{Index:%d}", turn.Index)
			}
		}

		// Assistant turn must be emitted with folded ToolCalls.
		var assistantTurns []ingest.Turn
		for _, turn := range turns {
			if turn.Role == ingest.RoleAssistant {
				assistantTurns = append(assistantTurns, turn)
			}
		}
		if len(assistantTurns) == 0 {
			t.Fatal("expected assistant turn in output, got none")
		}
		if len(assistantTurns[0].ToolCalls) == 0 {
			t.Error("assistant turn: expected folded ToolCalls, got none")
		}
	})
}
