package ingest_test

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/source_cause_settling.yaml
var sourceCauseSettlingFixtureData []byte

type sourceCauseSettlingFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name           string `yaml:"name"`
		Kind           string `yaml:"kind"`
		FirstReason    string `yaml:"first_reason"`
		SecondReason   string `yaml:"second_reason"`
		FirstMentions  string `yaml:"first_mentions"`
		SecondMentions string `yaml:"second_mentions"`
		Fault          string `yaml:"fault"`
	} `yaml:"cases"`
}

func loadSourceCauseSettlingFixtures(t *testing.T) sourceCauseSettlingFixtures {
	t.Helper()
	var fixtures sourceCauseSettlingFixtures
	if err := testutil.DecodeFixtureYAML(sourceCauseSettlingFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate source-cause settling fixture %q", fixture.Name)
		}
		switch fixture.Kind {
		case "cause-change", "same-cause-one-run", "store-fault":
		default:
			t.Fatalf("fixture %s names unknown kind %q", fixture.Name, fixture.Kind)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("source-cause settling", "case", fixtures.Required, names); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestSourceCauseSettling proves a settled refusal names its cause, that two
// lanes seeing that cause in one harvest index report it once between them,
// and that a store fault is not recorded as an unavailable source.
func TestSourceCauseSettling(t *testing.T) {
	fixtures := loadSourceCauseSettlingFixtures(t)
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			switch fixture.Kind {
			case "cause-change":
				testSourceCauseChange(t, fixture.FirstReason, fixture.SecondReason, fixture.FirstMentions, fixture.SecondMentions)
			case "same-cause-one-run":
				testHarvestIndexReportsOneCause(t, fixture.FirstReason)
			case "store-fault":
				testPairLaneStoreFaultKeepsFailure(t, fixture.Fault)
			}
		})
	}
}

func testSourceCauseChange(t *testing.T, firstReason, secondReason, firstMentions, secondMentions string) {
	t.Helper()
	memfs := testutil.NewMemFS()
	database, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	id := seedRepairableSession(t, database, memfs, false)
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	fresh := freshRepairMeta(t, id, nativePath, []byte(missingPairNativeTranscript))
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	run := func() *ingest.PipelineResult {
		t.Helper()
		return runCauseHarvest(t, memfs, adapters, false, database)
	}
	first := run()
	assertSourceReports(t, first, id, 1, firstMentions)
	assertStoredCause(t, database, id, firstReason)
	if first.Summary.Errors != 0 {
		t.Fatalf("a readable stored copy was counted as a failed ingest: %+v", first.Summary)
	}
	if err := memfs.WriteFile(nativePath, []byte(damagedSourceTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	second := run()
	assertSourceReports(t, second, id, 1, secondMentions)
	assertStoredCause(t, database, id, secondReason)
	third := run()
	assertSourceReports(t, third, id, 0, "")
	assertStoredCause(t, database, id, secondReason)
	if third.Summary.Errors != 0 {
		t.Fatalf("a settled session failed: %+v diagnostics=%+v", third.Summary, third.Diagnostics)
	}
}

func testHarvestIndexReportsOneCause(t *testing.T, wantReason string) {
	t.Helper()
	memfs := testutil.NewMemFS()
	database, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	id := seedRepairableSession(t, database, memfs, true)
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	fresh := freshRepairMeta(t, id, nativePath, []byte(missingPairNativeTranscript))
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	result := runCauseHarvest(t, memfs, adapters, true, database)
	if contentAndPair := len(sourceUnavailableDiagnostics(result, id)); contentAndPair != 1 {
		t.Fatalf("content and pair lanes reported the same cause %d times, want 1: %+v", contentAndPair, sessionDiagnostics(result, id))
	}
	if indexReports := len(typedSessionDiagnostics(result, id, "index_refused")); indexReports != 1 {
		t.Fatalf("index lane report changed: got %d index_refused, want 1: %+v", indexReports, sessionDiagnostics(result, id))
	}
	assertStoredCause(t, database, id, wantReason)
}

func testPairLaneStoreFaultKeepsFailure(t *testing.T, fault string) {
	t.Helper()
	ctx := t.Context()
	memfs := testutil.NewMemFS()
	database, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	id := seedRepairableSession(t, database, memfs, false)
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	if err := memfs.WriteFile(nativePath, []byte(missingPairNativeTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapterWithErrors(nil, nil, map[ingest.SessionID]error{
			id: fmt.Errorf("%s", fault),
		}),
	}
	result := runCauseHarvest(t, memfs, adapters, false, database)
	if result.Summary.Errors == 0 {
		t.Fatalf("a store fault with a completed index was not a failed session: %+v diagnostics=%+v", result.Summary, result.Diagnostics)
	}
	failed := false
	for _, session := range result.Sessions {
		if session.SessionID == id && session.Error != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("the store fault cleared the session error: %+v", result.Sessions)
	}
	reason, err := database.ReadSourceUnavailability(ctx, id)
	if err != nil || reason != nil {
		t.Fatalf("store fault wrote a settle row: %v %v", reason, err)
	}
	reported := false
	for _, diagnostic := range sessionDiagnostics(result, id) {
		if strings.Contains(diagnostic.Message, fault) {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("store fault was not reported: %+v", result.Diagnostics)
	}
}

func seedRepairableSession(t *testing.T, database *store.Store, memfs *testutil.MemFS, incomplete bool) ingest.SessionID {
	t.Helper()
	ctx := t.Context()
	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source.Format = ingest.SourceFormatJSONL
	meta.Source.FilePath = nativePath
	seedStoredPair(t, database, meta, []byte(missingPairNativeTranscript))
	if incomplete {
		seedStalePreviewCapture(t, ctx, database, id)
	} else {
		writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
			SessionID: id, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000000,
			Result: indexformat.V1{Entries: []schema.SessionEntry{{
				SessionID: id, EntryIndex: 0,
				EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: ptrTo(priorPreview),
			}}},
		}})
		if len(writes) != 1 || writes[0].Err != nil {
			t.Fatalf("seed a completed index: %+v", writes)
		}
	}
	// The metadata sidecar is absent, so the saved pair needs repair. The
	// transcript is present so a fallback index has bytes to refuse.
	storedPath := filepath.Join(ingest.SessionDir(testOutputDir, testutil.TestHostSlug, id.String(), ""), id.String()+"--transcript.jsonl")
	if err := memfs.WriteFile(storedPath, []byte(missingPairNativeTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	return id
}

func freshRepairMeta(t *testing.T, id ingest.SessionID, nativePath string, transcript []byte) *ingest.UnifiedMetadata {
	t.Helper()
	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(transcript)
	return fresh
}

func runCauseHarvest(t *testing.T, memfs *testutil.MemFS, adapters map[ingest.Harness]ingest.AdapterFactory, reindex bool, database *store.Store) *ingest.PipelineResult {
	t.Helper()
	cfg := makePipelineConfig(testOutputDir)
	cfg.Reindex = reindex
	pipeline, err := newTestPipeline(memfs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithIndexers(ingest.NewIndexerRegistry(memfs, ingest.IndexerRegistryOptions{})),
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	return result
}

func assertSourceReports(t *testing.T, result *ingest.PipelineResult, id ingest.SessionID, want int, mentions string) {
	t.Helper()
	matched := sourceUnavailableDiagnostics(result, id)
	if len(matched) != want {
		t.Fatalf("want %d source diagnostic(s), got %+v", want, sessionDiagnostics(result, id))
	}
	if want == 0 {
		return
	}
	if !strings.Contains(matched[0].Message, mentions) {
		t.Fatalf("diagnostic %q does not mention %q", matched[0].Message, mentions)
	}
}

func assertStoredCause(t *testing.T, database *store.Store, id ingest.SessionID, want string) {
	t.Helper()
	reason, err := ingest.NewSourceUnavailabilityReason(want)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.ReadSourceUnavailability(t.Context(), id)
	if err != nil || stored == nil || *stored != reason {
		t.Fatalf("stored cause = %v, err %v, want %s", stored, err, reason)
	}
}
