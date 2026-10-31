package ingest_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// runStaleStoredSession seeds one stale stored session from the settling
// fixture, as an older build left it, and runs the ordinary harvest over it.
func runStaleStoredSession(t *testing.T, configure func(*ingest.PipelineConfig)) (*store.Store, ingest.SessionID, *ingest.PipelineResult) {
	t.Helper()
	document := loadStaleIndexSettlingFixtures(t)
	var transcript []byte
	for index := range document.Cases {
		if document.Cases[index].Name == "claude-stored-identity-strict-refused" {
			transcript = []byte(document.Transcripts[document.Cases[index].Transcript])
		}
	}
	if transcript == nil {
		t.Fatal("fixture declares no claude-stored-identity-strict-refused case")
	}
	ctx := t.Context()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	database := storetest.OpenWith(t)
	t.Cleanup(func() { database.Close() })
	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.ContentHash = schema.ComputeTranscriptHash(transcript)
	dir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
	if err := fs.WriteFile(filepath.Join(dir, id.String()+"--transcript.jsonl"), transcript, 0600); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(dir, id.String()+"--metadata.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	pair, err := ingest.NewManagedArtifact(encoded, transcript)
	if err != nil {
		t.Fatal(err)
	}
	results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
		t.Fatalf("seed stored artifact identity: %+v", results)
	}
	seedStalePreviewCapture(t, ctx, database, id)

	cfg := makePipelineConfig(testOutputDir, configure)
	adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	return database, id, result
}

// TestOrdinaryHarvestReportsStaleSessionsOutsideTheAgeWindow proves `harvest
// --since` says what it left behind. A session older than the window keeps its
// refused capture, which is the whole of what makes it unexportable, so the
// run must name it instead of leaving the next export to discover it.
func TestOrdinaryHarvestReportsStaleSessionsOutsideTheAgeWindow(t *testing.T) {
	t.Parallel()
	since := time.Now().Add(time.Hour)
	database, id, result := runStaleStoredSession(t, func(cfg *ingest.PipelineConfig) { cfg.Since = &since })
	state, err := database.ReadIndexState(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil || state.IndexerVersion != 15 {
		t.Fatalf("a session outside the age window changed its stored index state: %+v", state)
	}
	var messages []string
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.ErrorType == "stale_index_outside_selection" {
			messages = append(messages, diagnostic.Message)
		}
	}
	if len(messages) != 1 || !strings.Contains(messages[0], "1 stored session") {
		t.Fatalf("selection diagnostics = %q, want exactly one naming one session; all: %+v", messages, result.Diagnostics)
	}
}

// TestStaleSessionRepairedInTheSameRunIsNotReportedUnrecoverable proves content
// recovery does not tell the user a capture "stays incomplete" when the index
// stage of the same run replaces it. The transcript carries a record the
// content-only path cannot certify; the stale producer sends the session to the
// index stage of `harvest index`, which stores it whole.
func TestStaleSessionRepairedInTheSameRunIsNotReportedUnrecoverable(t *testing.T) {
	t.Parallel()
	database, id, result := runStaleStoredSession(t, func(cfg *ingest.PipelineConfig) { cfg.Reindex = true })
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.ErrorType == "content_recovery_unavailable" {
			t.Fatalf("the run reported a repair it went on to make: %+v", diagnostic)
		}
	}
	state, err := database.ReadIndexState(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	target := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion
	if state == nil || state.IndexerVersion != target {
		t.Fatalf("the session was not re-indexed to producer %d: %+v", target, state)
	}
}
