package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type projectAnalyticsFixture struct {
	Types    []projectAnalyticsFixtureType `yaml:"types"`
	Sessions []analyticsFixtureSession     `yaml:"sessions"`
	Expected yaml.Node                     `yaml:"expected"`
}

type projectAnalyticsFixtureType struct {
	Name   string   `yaml:"name"`
	Fields []string `yaml:"fields"`
}

type analyticsFixtureSession struct {
	ID              string  `yaml:"id"`
	StartTime       string  `yaml:"startTime"`
	Project         string  `yaml:"project"`
	Outcome         string  `yaml:"outcome"`
	DurationMinutes float64 `yaml:"durationMins"`
	TotalTokens     int     `yaml:"totalTokens"`
	TurnCount       int     `yaml:"turnCount"`
	ToolCalls       int     `yaml:"toolCalls"`
}

func loadProjectAnalyticsFixture(t *testing.T) projectAnalyticsFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/project_analytics.yaml")
	if err != nil {
		t.Fatalf("read testdata/project_analytics.yaml: %v", err)
	}
	var fixture projectAnalyticsFixture
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode testdata/project_analytics.yaml: %v", err)
	}
	if len(fixture.Types) == 0 || len(fixture.Sessions) == 0 {
		t.Fatal("testdata/project_analytics.yaml carries no types or sessions")
	}
	return fixture
}

// expectedProjectAnalytics decodes the recorded bundle through JSON so the
// fixture uses the wire field names and an unknown field fails the test.
func expectedProjectAnalytics(t *testing.T, node yaml.Node) schema.ProjectAnalytics {
	t.Helper()
	var document any
	if err := node.Decode(&document); err != nil {
		t.Fatalf("decode expected ProjectAnalytics: %v", err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("marshal expected ProjectAnalytics: %v", err)
	}
	var expected schema.ProjectAnalytics
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&expected); err != nil {
		t.Fatalf("decode expected ProjectAnalytics wire shape: %v", err)
	}
	return expected
}

// analyticsTypes is the type registry the consumer manifest names. It is a
// registry, not a case table: the fixture owns the expected field sets.
var analyticsTypes = map[string]reflect.Type{
	"ProjectAnalytics":         reflect.TypeOf(schema.ProjectAnalytics{}),
	"WeekCount":                reflect.TypeOf(schema.WeekCount{}),
	"WeekContributors":         reflect.TypeOf(schema.WeekContributors{}),
	"ReturningContributorRate": reflect.TypeOf(schema.ReturningContributorRate{}),
	"LongestStreak":            reflect.TypeOf(schema.LongestStreak{}),
	"WeekNewContributors":      reflect.TypeOf(schema.WeekNewContributors{}),
	"SessionToCommitRate":      reflect.TypeOf(schema.SessionToCommitRate{}),
	"WeekAvgDuration":          reflect.TypeOf(schema.WeekAvgDuration{}),
	"OutcomeDistribution":      reflect.TypeOf(schema.OutcomeDistribution{}),
	"MedianP90":                reflect.TypeOf(schema.MedianP90{}),
	"AnalyticsSessionStats":    reflect.TypeOf(schema.AnalyticsSessionStats{}),
	"ContributorBreakdown":     reflect.TypeOf(schema.ContributorBreakdown{}),
}

func TestProjectAnalyticsFieldSetMatchesManifest(t *testing.T) {
	t.Parallel()
	fixture := loadProjectAnalyticsFixture(t)
	if len(fixture.Types) != len(analyticsTypes) {
		t.Fatalf("manifest declares %d types, want %d", len(fixture.Types), len(analyticsTypes))
	}
	seen := map[string]struct{}{}
	for _, declared := range fixture.Types {
		if _, duplicate := seen[declared.Name]; duplicate {
			t.Fatalf("manifest declares %s twice", declared.Name)
		}
		seen[declared.Name] = struct{}{}
		valueType, ok := analyticsTypes[declared.Name]
		if !ok {
			t.Fatalf("manifest type %s has no Go type in the analytics bundle", declared.Name)
		}
		assertAnalyticsFieldSet(t, declared.Name, analyticsJSONFields(t, valueType), declared.Fields)
	}
	for name := range analyticsTypes {
		if _, ok := seen[name]; !ok {
			t.Fatalf("manifest is missing the analytics type %s", name)
		}
	}
}

func TestComputeProjectAnalyticsMatchesRecordedBundle(t *testing.T) {
	t.Parallel()
	fixture := loadProjectAnalyticsFixture(t)
	expected := expectedProjectAnalytics(t, fixture.Expected)

	records := make([]store.AnalyticsSession, 0, len(fixture.Sessions))
	for _, session := range fixture.Sessions {
		records = append(records, store.AnalyticsSession{
			StartMs:         fixtureStartMs(t, session.StartTime),
			ProjectHash:     session.Project,
			Outcome:         session.Outcome,
			DurationMinutes: session.DurationMinutes,
			TotalTokens:     session.TotalTokens,
			TurnCount:       session.TurnCount,
			ToolCalls:       session.ToolCalls,
		})
	}
	if got := store.ComputeProjectAnalytics(records); !reflect.DeepEqual(got, expected) {
		t.Fatalf("pure aggregate mismatch:\n got: %+v\nwant: %+v", got, expected)
	}
}

func TestComputeProjectAnalyticsOnConnMatchesRecordedBundle(t *testing.T) {
	t.Parallel()
	fixture := loadProjectAnalyticsFixture(t)
	expected := expectedProjectAnalytics(t, fixture.Expected)
	database := storetest.Open(t)
	seedAnalyticsFixture(t, database, fixture.Sessions)

	analytics, medians, err := database.AnalyticsAggregates(context.Background(), store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("AnalyticsAggregates: %v", err)
	}
	if !reflect.DeepEqual(analytics, expected) {
		t.Fatalf("stored aggregate mismatch:\n got: %+v\nwant: %+v", analytics, expected)
	}
	// Every fixture session has tokens, no retry waste, and no reverts, so the
	// retry share and revert medians are zero; a zero spec score is excluded and
	// the score median stays absent.
	assertPersonalMedians(t, medians, schema.PersonalMedians{
		RetryShare:           floatPointer(0),
		WithinSessionReverts: floatPointer(0),
	})
}

func TestComputePersonalMediansMatchesExportedCorpus(t *testing.T) {
	t.Parallel()
	fixtures, err := schema.LoadQualityFixtures()
	if err != nil {
		t.Fatalf("schema.LoadQualityFixtures: %v", err)
	}
	if len(fixtures.Medians) == 0 {
		t.Fatal("schema medians corpus is empty")
	}
	if len(fixtures.RequiredMedianNames) != len(fixtures.Medians) {
		t.Fatalf("corpus declares %d required names but %d cases", len(fixtures.RequiredMedianNames), len(fixtures.Medians))
	}
	for _, medianCase := range fixtures.Medians {
		medianCase := medianCase
		t.Run(medianCase.Name, func(t *testing.T) {
			t.Parallel()
			inputs := make([]store.MedianInput, 0, len(medianCase.Sessions))
			for _, session := range medianCase.Sessions {
				inputs = append(inputs, store.MedianInput{
					TotalTokens:          session.TotalTokens,
					RetryTokensWasted:    session.RetryTokensWasted,
					SpecQualityScore:     session.SpecQualityScore,
					WithinSessionReverts: session.WithinSessionReverts,
				})
			}
			assertPersonalMedians(t, store.ComputePersonalMedians(inputs), medianCase.Expected)
		})
	}
}

func TestComputePersonalMediansOnConnMatchesExportedCorpus(t *testing.T) {
	t.Parallel()
	fixtures, err := schema.LoadQualityFixtures()
	if err != nil {
		t.Fatalf("schema.LoadQualityFixtures: %v", err)
	}
	for _, medianCase := range fixtures.Medians {
		medianCase := medianCase
		t.Run(medianCase.Name, func(t *testing.T) {
			t.Parallel()
			database := storetest.Open(t)
			ctx := context.Background()
			for index, session := range medianCase.Sessions {
				seedMediansSession(t, database, index, session)
			}
			conn, err := database.PoolForTest().Take(ctx)
			if err != nil {
				t.Fatalf("PoolForTest.Take: %v", err)
			}
			defer database.PoolForTest().Put(conn)
			got, err := store.ComputePersonalMediansOnConn(ctx, conn, store.AnalyticsFilter{})
			if err != nil {
				t.Fatalf("ComputePersonalMediansOnConn: %v", err)
			}
			assertPersonalMedians(t, got, medianCase.Expected)
		})
	}
}

// TestAnalyticsAggregateCompilesOneStatement proves the aggregate does not
// issue a query per session: the statement count SQLite compiles is identical
// for a small library and a ten-times-larger one. A per-row read would scale
// with the stored row count.
func TestAnalyticsAggregateCompilesOneStatement(t *testing.T) {
	t.Parallel()
	fixture := loadProjectAnalyticsFixture(t)
	small := storetest.Open(t)
	seedAnalyticsFixture(t, small, fixture.Sessions)
	large := storetest.Open(t)
	largeSessions := make([]analyticsFixtureSession, 0, len(fixture.Sessions)*10)
	for repeat := 0; repeat < 10; repeat++ {
		largeSessions = append(largeSessions, fixture.Sessions...)
	}
	seedAnalyticsFixture(t, large, largeSessions)

	smallSelects := countCompiledSelects(t, small, func(ctx context.Context, conn *sqlite.Conn) {
		if _, err := store.ComputeProjectAnalyticsOnConn(ctx, conn, store.AnalyticsFilter{}); err != nil {
			t.Fatalf("ComputeProjectAnalyticsOnConn(small): %v", err)
		}
	})
	largeSelects := countCompiledSelects(t, large, func(ctx context.Context, conn *sqlite.Conn) {
		if _, err := store.ComputeProjectAnalyticsOnConn(ctx, conn, store.AnalyticsFilter{}); err != nil {
			t.Fatalf("ComputeProjectAnalyticsOnConn(large): %v", err)
		}
	})
	if smallSelects == 0 {
		t.Fatal("the aggregate compiled no statement")
	}
	if smallSelects != largeSelects {
		t.Fatalf("aggregate compiled %d statements for %d sessions and %d for %d; a per-row read would scale with the library", smallSelects, len(fixture.Sessions), largeSelects, len(largeSessions))
	}
}

// TestQualityPageRowsReadsTwoPagesOnly proves a page read is bounded: the
// statement count is identical for a small library and a larger one, and one
// page read returns at most the requested page plus the next.
func TestQualityPageRowsReadsTwoPagesOnly(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	seedAnalyticsFixture(t, database, analyticsPageSessions(60))

	selects := countCompiledSelects(t, database, func(ctx context.Context, conn *sqlite.Conn) {
		read, err := store.QualityPageRowsOnConn(ctx, conn, 1, 25)
		if err != nil {
			t.Fatalf("QualityPageRowsOnConn: %v", err)
		}
		if len(read.Rows) != 50 {
			t.Fatalf("quality page read %d rows, want the page and the next page (50)", len(read.Rows))
		}
	})

	tiny := storetest.Open(t)
	seedAnalyticsFixture(t, tiny, analyticsPageSessions(3))
	tinySelects := countCompiledSelects(t, tiny, func(ctx context.Context, conn *sqlite.Conn) {
		if _, err := store.QualityPageRowsOnConn(ctx, conn, 1, 25); err != nil {
			t.Fatalf("QualityPageRowsOnConn(tiny): %v", err)
		}
	})
	if selects == 0 {
		t.Fatal("the page read compiled no statement")
	}
	if selects != tinySelects {
		t.Fatalf("quality page compiled %d statements for 60 sessions and %d for 3; a page read must not scale with the library", selects, tinySelects)
	}
}

func seedAnalyticsFixture(t *testing.T, database *store.Store, sessions []analyticsFixtureSession) {
	t.Helper()
	ctx := context.Background()
	projectHashes := map[string]string{
		"proj-a": strings.Repeat("a", 64),
		"proj-b": strings.Repeat("b", 64),
	}
	for index, session := range sessions {
		projectHash, ok := projectHashes[session.Project]
		if !ok {
			projectHash = strings.Repeat("c", 64)
		}
		startMs := fixtureStartMs(t, session.StartTime)
		sessionID := analyticsSessionID(index)
		entry := makeStoreEntry(t, sessionID, projectHash, "quality-host", defaults.HarnessClaudeCode, startMs, session.TotalTokens/2, session.TotalTokens-session.TotalTokens/2)
		if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("InsertSessions(%s): %v", sessionID, err)
		}
		outcome := schema.SessionOutcome(session.Outcome)
		var outcomePtr *schema.SessionOutcome
		if session.Outcome != "" {
			outcomePtr = &outcome
		}
		metrics := &ingest.SessionMetrics{
			SessionID: ingest.SessionID(sessionID),
			QualityMetrics: schema.QualityMetrics{
				TurnCount:       intPointer(session.TurnCount),
				ToolCalls:       intPointer(session.ToolCalls),
				DurationMinutes: floatPointer(session.DurationMinutes),
				InputTokens:     intPointer(session.TotalTokens / 2),
				OutputTokens:    intPointer(session.TotalTokens - session.TotalTokens/2),
				TotalTokens:     intPointer(session.TotalTokens),
				Outcome:         outcomePtr,
				ComputeVersion:  intPointer(1),
			},
		}
		if err := database.SaveMetrics(ctx, metrics); err != nil {
			t.Fatalf("SaveMetrics(%s): %v", sessionID, err)
		}
		turnCount := session.TurnCount
		if _, err := database.UpsertCapturedStats(ctx, store.CapturedStats{
			SessionID:             schema.SessionID(sessionID),
			ConversationTurnCount: &turnCount,
			Source:                store.StatsSourceHarness,
			UpdatedAtMs:           1,
		}); err != nil {
			t.Fatalf("seed conversation turn count(%s): %v", sessionID, err)
		}
	}
}

func seedMediansSession(t *testing.T, database *store.Store, index int, session schema.QualityMedianSession) {
	t.Helper()
	ctx := context.Background()
	sessionID := analyticsSessionID(index)
	entry := makeStoreEntry(t, sessionID, strings.Repeat("d", 64), "medians-host", defaults.HarnessClaudeCode, 1700000000000, session.TotalTokens/2, session.TotalTokens-session.TotalTokens/2)
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions(%s): %v", sessionID, err)
	}
	score := session.SpecQualityScore
	metrics := &ingest.SessionMetrics{
		SessionID: ingest.SessionID(sessionID),
		QualityMetrics: schema.QualityMetrics{
			InputTokens:          intPointer(session.TotalTokens / 2),
			OutputTokens:         intPointer(session.TotalTokens - session.TotalTokens/2),
			TotalTokens:          intPointer(session.TotalTokens),
			RetryTokensWasted:    intPointer(session.RetryTokensWasted),
			SpecQualityScore:     &score,
			WithinSessionReverts: intPointer(session.WithinSessionReverts),
			ComputeVersion:       intPointer(1),
		},
	}
	if err := database.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics(%s): %v", sessionID, err)
	}
}

func analyticsPageSessions(count int) []analyticsFixtureSession {
	sessions := make([]analyticsFixtureSession, 0, count)
	for index := 0; index < count; index++ {
		sessions = append(sessions, analyticsFixtureSession{
			ID:              fmt.Sprintf("page-sess-%03d", index),
			StartTime:       "2026-02-02T00:00:00Z",
			Project:         "proj-a",
			Outcome:         "resolved",
			DurationMinutes: 10,
			TotalTokens:     1000,
			TurnCount:       4,
			ToolCalls:       2,
		})
	}
	return sessions
}

// countCompiledSelects runs one operation on a pooled connection and reports
// how many SELECT statements SQLite compiled for it.
func countCompiledSelects(t *testing.T, database *store.Store, run func(context.Context, *sqlite.Conn)) int {
	t.Helper()
	ctx := context.Background()
	conn, err := database.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("PoolForTest.Take: %v", err)
	}
	defer database.PoolForTest().Put(conn)
	selects := 0
	if err := conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
		if action.Type() == sqlite.OpSelect {
			selects++
		}
		return sqlite.AuthResultOK
	})); err != nil {
		t.Fatalf("SetAuthorizer: %v", err)
	}
	defer func() { _ = conn.SetAuthorizer(nil) }()
	run(ctx, conn)
	return selects
}

// analyticsSessionID derives a valid stored session identifier from a
// deterministic index so the fixture keeps its readable ids.
func analyticsSessionID(index int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", index+1)
}

func fixtureStartMs(t *testing.T, raw string) int64 {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse fixture startTime %q: %v", raw, err)
	}
	return parsed.UnixMilli()
}

func assertPersonalMedians(t *testing.T, got, want schema.PersonalMedians) {
	t.Helper()
	assertOptionalFloat(t, "retryShare", got.RetryShare, want.RetryShare)
	assertOptionalFloat(t, "specQualityScore", got.SpecQualityScore, want.SpecQualityScore)
	assertOptionalFloat(t, "withinSessionReverts", got.WithinSessionReverts, want.WithinSessionReverts)
}

func assertOptionalFloat(t *testing.T, label string, got, want *float64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
		return
	case got == nil || want == nil:
		t.Fatalf("%s = %v, want %v", label, got, want)
	case math.Abs(*got-*want) > 1e-9:
		t.Fatalf("%s = %v, want %v", label, *got, *want)
	}
}

// analyticsJSONFields returns the wire property names a value type emits. It
// marshals a zero value through encoding/json instead of reading struct tags,
// so the parity assertion checks the names a client actually receives.
func analyticsJSONFields(t *testing.T, valueType reflect.Type) []string {
	t.Helper()
	raw, err := json.Marshal(reflect.New(valueType).Interface())
	if err != nil {
		t.Fatalf("marshal %s: %v", valueType, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode %s wire object: %v", valueType, err)
	}
	fields := make([]string, 0, len(object))
	for name := range object {
		fields = append(fields, name)
	}
	return fields
}

func assertAnalyticsFieldSet(t *testing.T, label string, actual, expected []string) {
	t.Helper()
	actualSet := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if _, duplicate := actualSet[name]; duplicate {
			t.Fatalf("%s repeats field %q", label, name)
		}
		actualSet[name] = struct{}{}
	}
	expectedSet := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		expectedSet[name] = struct{}{}
	}
	if len(actualSet) != len(expectedSet) {
		t.Fatalf("%s has %d fields, want %d: %v", label, len(actualSet), len(expectedSet), expected)
	}
	for name := range expectedSet {
		if _, ok := actualSet[name]; !ok {
			t.Fatalf("%s is missing field %q", label, name)
		}
	}
	for name := range actualSet {
		if _, ok := expectedSet[name]; !ok {
			t.Fatalf("%s carries extra field %q", label, name)
		}
	}
}

func intPointer(value int) *int { return &value }

func floatPointer(value float64) *float64 { return &value }
