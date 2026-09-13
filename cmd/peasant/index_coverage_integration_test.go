package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// coverageFailIndexer refuses every parse. The pipeline records a failed index
// attempt for every targeted session while the entries the store already holds
// stay exactly as seeded, which is the shape the coverage report exists to
// describe: a failed attempt that retained its entries, or one that never had
// any.
type coverageFailIndexer struct{ err error }

func (f coverageFailIndexer) SourceKind() ingest.TranscriptSourceKind {
	return ingest.TranscriptSourceFile
}

func (f coverageFailIndexer) IndexTranscript(context.Context, ingest.DiscoveredSession) ([]schema.SessionEntry, error) {
	return nil, f.err
}

func (f coverageFailIndexer) IndexTranscriptBytes(context.Context, ingest.DiscoveredSession, []byte) ([]schema.SessionEntry, error) {
	return nil, f.err
}

var _ ingest.TranscriptIndexer = coverageFailIndexer{}

// coverageIntegrationFixture is one seeded session the integration run will
// fail to index, with the stored entries that decide which side of the split it
// lands on.
type coverageIntegrationFixture struct {
	id          ingest.SessionID
	retainEntry bool
}

// coverageIntegrationSessionID returns a distinct valid session UUID for the
// fixture at position i.
func coverageIntegrationSessionID(i int) ingest.SessionID {
	return ingest.SessionID(fmt.Sprintf("22222222-2222-4222-8222-%012x", i))
}

// coverageIntegrationMeta builds the retained metadata a completed harvest
// leaves for one seeded session.
func coverageIntegrationMeta(t *testing.T, session ingest.SessionID) ingest.UnifiedMetadata {
	t.Helper()
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = session
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source = schema.SourceInfo{FilePath: "/synthetic/source.jsonl", Format: ingest.SourceFormatJSONL}
	ingested := int64(1700000000000)
	meta.Timestamp = schema.TimestampInfo{Start: 1708300800000, End: 1708300860000, Ingested: &ingested}
	meta.HostSlug = schema.HostSlug(testutil.TestHostSlug)
	// A fixed but well-formed identity: production refuses metadata without a
	// project hash, and the value itself is irrelevant to coverage membership.
	meta.Project = schema.ProjectContext{
		Hash:     schema.ProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		Name:     "testproj",
		FilePath: "/testproj",
	}
	return meta
}

// seedCoverageIntegrationSession writes the retained pair for one fixture and,
// when the fixture is retained, one stored entry row. It returns nothing: the
// store is the fixture.
func seedCoverageIntegrationSession(t *testing.T, db *store.Store, filesystem *testutil.MemFS, output string, fixture coverageIntegrationFixture) {
	t.Helper()
	meta := coverageIntegrationMeta(t, fixture.id)
	storetest.SeedManagedArtifact(t, db, filesystem, output, meta, []byte(`{"sessionId":"synthetic"}`))
	if !fixture.retainEntry {
		return
	}
	preview := "retained entry content"
	entries := []schema.SessionEntry{{
		SessionID:      schema.SessionID(fixture.id),
		Harness:        schema.HarnessClaudeCode,
		EntryIndex:     0,
		EntryType:      schema.EntryTypeText,
		Role:           schema.RoleUser,
		ContentPreview: &preview,
	}}
	if err := db.IndexSessionEntries(context.Background(), schema.SessionID(fixture.id), entries); err != nil {
		t.Fatalf("seed retained entries for %s: %v", fixture.id, err)
	}
}

// runCoverageIntegration drives the production pipeline finalize over a real
// SQLite store and returns the result. The pipeline is the reindex path the
// command uses: it scans the seeded output tree, reconciles the rows, and
// indexes every target, so the failed set and the stored membership both come
// from real components rather than a hand-built result.
func runCoverageIntegration(t *testing.T, db *store.Store, outputDir string, fixtures []coverageIntegrationFixture) *ingest.PipelineResult {
	t.Helper()
	filesystem := testutil.NewMemFS()
	for _, fixture := range fixtures {
		seedCoverageIntegrationSession(t, db, filesystem, outputDir, fixture)
	}
	pipeline, err := ingest.NewPipeline(
		filesystem,
		testutil.DefaultGitResolver(),
		map[ingest.Harness]ingest.AdapterFactory{
			ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter {
				return &testutil.StubAdapter{ProviderValue: ingest.HarnessClaudeCode}
			},
		},
		ingest.PipelineConfig{
			OutputDir:   ingest.ResolvedPath(outputDir),
			Reindex:     true,
			Parallelism: 1,
		},
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithIndexLogger(db),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{
			ingest.HarnessClaudeCode: coverageFailIndexer{err: errors.New("synthetic parse refusal")},
		}),
	)
	if err != nil {
		t.Fatalf("construct coverage integration pipeline: %v", err)
	}
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("run coverage integration pipeline: %v", err)
	}
	return result
}

// TestHarvestIndexCoverage_RealStoreThroughSummaryAndJSON is the end-to-end
// regression: a real SQLite store, the production pipeline finalize, the text
// summary and the real JSON serialization, all in one run. The previous
// failure surfaced as a bare count of failed attempts described as empty
// sessions; this pins the number a user reads to the entries the store
// actually holds.
//
// Its acceptance test, stated as the production edits that must turn it red:
// dropping the store question from finalize (coverage would be nil and the
// summary would fall back to the bare attempt count); counting failed attempts
// as empty without asking the store; omitting indexCoverage from printJSON;
// and serializing an unavailable run as a zero object.
func TestHarvestIndexCoverage_RealStoreThroughSummaryAndJSON(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	fixtures := []coverageIntegrationFixture{
		{id: coverageIntegrationSessionID(1), retainEntry: true},
		{id: coverageIntegrationSessionID(2), retainEntry: false},
	}
	result := runCoverageIntegration(t, db, "/coverage-integration-output", fixtures)

	coverage := result.IndexCoverage
	if coverage == nil {
		t.Fatal("the production finalize left IndexCoverage nil for a store that answers stored-entry membership; nil means unavailable, never zero")
	}
	if coverage.FailedAttempts != 2 || coverage.Empty != 1 || coverage.FailedRetained != 1 {
		t.Fatalf("IndexCoverage = %+v, want {FailedAttempts:2 Empty:1 FailedRetained:1} from the real store's membership answer", coverage)
	}
	wantWithout := map[ingest.SessionID]bool{
		fixtures[0].id: false,
		fixtures[1].id: true,
	}
	gotWithout, err := db.SessionsWithoutEntries(context.Background(), []ingest.SessionID{fixtures[0].id, fixtures[1].id})
	if err != nil {
		t.Fatalf("read stored-entry membership: %v", err)
	}
	for id, want := range wantWithout {
		if got := gotWithout[id]; got != want {
			t.Errorf("stored-entry membership for %s = %v, want %v", id, got, want)
		}
	}

	var summary strings.Builder
	printSummary(&summary, result, false, false, "/coverage-integration-output", "", nil, 0)
	printed := summary.String()
	if want := fmt.Sprintf("%d session(s) have no stored entries", coverage.Empty); !strings.Contains(printed, want) {
		t.Errorf("the summary must name the %d empty session(s) from the measured coverage. got:\n%s", coverage.Empty, printed)
	}
	if want := fmt.Sprintf("%d session(s) failed to re-index but kept their previous entries", coverage.FailedRetained); !strings.Contains(printed, want) {
		t.Errorf("the summary must name the %d retained session(s) from the measured coverage. got:\n%s", coverage.FailedRetained, printed)
	}
	if strings.Contains(printed, "were imported but NOT indexed") {
		t.Errorf("measured coverage must replace the bare attempt count, not repeat it. got:\n%s", printed)
	}
	if !strings.Contains(printed, indexFailureRemedy) {
		t.Errorf("the coverage warning must carry the remedy %q. got:\n%s", indexFailureRemedy, printed)
	}

	var jsonOut strings.Builder
	if err := printJSON(&jsonOut, result); err != nil {
		t.Fatalf("printJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonOut.String()), &decoded); err != nil {
		t.Fatalf("printJSON output is not JSON: %v\n%s", err, jsonOut.String())
	}
	raw, ok := decoded["indexCoverage"]
	if !ok {
		t.Fatalf("measured coverage is missing from the real JSON output:\n%s", jsonOut.String())
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("indexCoverage is not an object:\n%s", jsonOut.String())
	}
	for field, want := range map[string]int{
		"failedAttempts": coverage.FailedAttempts,
		"empty":          coverage.Empty,
		"failedRetained": coverage.FailedRetained,
	} {
		got, ok := fields[field].(float64)
		if !ok || int(got) != want {
			t.Errorf("indexCoverage.%s = %v, want %d:\n%s", field, fields[field], want, jsonOut.String())
		}
	}
}

// TestHarvestIndexCoverage_AllRetainedIsAMeasuredZeroThroughJSON pins the other
// half of the measured-zero contract on the real path: a run whose failures all
// kept their entries reports Empty 0 as a real field, not as a missing one, so
// a user can tell "we checked and nothing is empty" from "we could not check".
func TestHarvestIndexCoverage_AllRetainedIsAMeasuredZeroThroughJSON(t *testing.T) {
	t.Parallel()
	db := storetest.Open(t)
	fixtures := []coverageIntegrationFixture{
		{id: coverageIntegrationSessionID(11), retainEntry: true},
		{id: coverageIntegrationSessionID(12), retainEntry: true},
	}
	result := runCoverageIntegration(t, db, "/coverage-integration-all-retained", fixtures)

	coverage := result.IndexCoverage
	if coverage == nil {
		t.Fatal("the production finalize left IndexCoverage nil for a store that answers stored-entry membership")
	}
	if coverage.FailedAttempts != 2 || coverage.Empty != 0 || coverage.FailedRetained != 2 {
		t.Fatalf("IndexCoverage = %+v, want {FailedAttempts:2 Empty:0 FailedRetained:2}", coverage)
	}

	var summary strings.Builder
	printSummary(&summary, result, false, false, "/coverage-integration-all-retained", "", nil, 0)
	printed := summary.String()
	if strings.Contains(printed, "have no stored entries") {
		t.Errorf("every failed session kept its entries, yet the summary warns about empty sessions:\n%s", printed)
	}
	if !strings.Contains(printed, "failed to re-index but kept their previous entries") {
		t.Errorf("the summary must name the retained sessions while omitting the empty sentence:\n%s", printed)
	}

	var jsonOut strings.Builder
	if err := printJSON(&jsonOut, result); err != nil {
		t.Fatalf("printJSON: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonOut.String()), &decoded); err != nil {
		t.Fatalf("printJSON output is not JSON: %v\n%s", err, jsonOut.String())
	}
	raw, ok := decoded["indexCoverage"]
	if !ok {
		t.Fatalf("a measured zero must be a carried field, not an absent one:\n%s", jsonOut.String())
	}
	fields, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("indexCoverage is not an object:\n%s", jsonOut.String())
	}
	empty, ok := fields["empty"].(float64)
	if !ok || int(empty) != 0 {
		t.Errorf("indexCoverage.empty = %v, want the measured zero 0:\n%s", fields["empty"], jsonOut.String())
	}

	// The contrast the field exists for: a run the store could not answer for
	// omits indexCoverage entirely, so a reader never confuses "we checked and
	// found none" with "we could not check".
	var unavailableOut strings.Builder
	if err := printJSON(&unavailableOut, &ingest.PipelineResult{}); err != nil {
		t.Fatalf("printJSON for an unavailable run: %v", err)
	}
	if strings.Contains(unavailableOut.String(), "indexCoverage") {
		t.Errorf("an unavailable run must omit indexCoverage so a measured zero stays distinguishable from an absent answer:\n%s", unavailableOut.String())
	}
	if jsonOut.String() == unavailableOut.String() {
		t.Error("a measured zero and an unavailable answer serialized identically; a reader cannot tell them apart")
	}
}
