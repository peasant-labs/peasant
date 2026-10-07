package ingest_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

const missingPairNativeTranscript = `{"type":"user","message":{"role":"user","content":"inspect the native fixture"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"I will inspect it now."}]},"uuid":"a1"}
`

// A stored session whose saved pair is absent and whose input proof was cleared
// is selected by both maintenance inventories. The pair-repair pass owns it: it
// re-ingests from native input when the source is available, and reports the
// source unavailable once when it is not. The index inventory must not also
// reconstruct it and read the missing pair, which would repeat an acquisition
// failure on every harvest. When the source reappears, the repair completes and
// the input proof is restored.
func TestMissingPairSettlesWithoutRepeatedIndexAcquisition(t *testing.T) {
	ctx := t.Context()
	memfs := testutil.NewMemFS()
	database, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	nativeTranscript := []byte(missingPairNativeTranscript)
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	// The native source is absent for the first run; the pair is never written.
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source.Format = ingest.SourceFormatJSONL
	meta.Source.FilePath = nativePath
	meta.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	// Record the pair identity without installing the pair, so the row names a
	// pair it no longer has.
	pair, err := ingest.NewManagedArtifact(encoded, nativeTranscript)
	if err != nil {
		t.Fatal(err)
	}
	results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
		t.Fatalf("seed stored artifact identity: %+v", results)
	}
	seedStalePreviewCapture(t, ctx, database, id)
	seeded, err := database.ReadIndexState(ctx, id)
	if err != nil || seeded == nil || seeded.ArtifactHash == nil || seeded.IndexedInputHash != nil {
		t.Fatalf("seed must leave a recorded identity and no input proof: %+v, %v", seeded, err)
	}

	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	cfg := makePipelineConfig(testOutputDir)
	run := func() *ingest.PipelineResult {
		t.Helper()
		pipeline, err := newTestPipeline(memfs, testutil.DefaultGitResolver(), adapters, cfg,
			ingest.WithIndexers(ingest.NewIndexerRegistry(memfs, ingest.IndexerRegistryOptions{})),
			ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("harvest: %v", err)
		}
		return result
	}

	// Run 1: the source is unavailable. The run reports it once and does not
	// re-attempt the missing pair in the index inventory.
	first := run()
	if first.Summary.Indexed != 0 {
		t.Fatalf("a pair-less session was indexed: %+v", first.Summary)
	}
	if got := indexLogErrors(t, database, id); got != 0 {
		t.Fatalf("the index inventory repeated the missing-pair acquisition failure %d time(s)", got)
	}
	unavailable := false
	for _, diagnostic := range first.Diagnostics {
		if diagnostic.ErrorType == "pair_repair_unavailable" {
			unavailable = true
		}
	}
	if !unavailable {
		t.Fatalf("the unavailable source was not reported: %+v", first.Diagnostics)
	}
	afterFirst, err := database.ReadIndexState(ctx, id)
	if err != nil || afterFirst == nil || afterFirst.IndexedInputHash != nil {
		t.Fatalf("a declined repair changed the stored index state: %+v, %v", afterFirst, err)
	}

	// Run 2: the source reappears. The pair-repair pass re-ingests it and the
	// index write restores the input proof.
	if err := memfs.WriteFile(nativePath, nativeTranscript, 0600); err != nil {
		t.Fatal(err)
	}
	second := run()
	if second.Summary.Indexed != 1 {
		t.Fatalf("a reappearing source was not re-ingested: %+v", second.Summary)
	}
	afterSecond, err := database.ReadIndexState(ctx, id)
	if err != nil || afterSecond == nil || afterSecond.IndexedInputHash == nil {
		t.Fatalf("the repaired session recorded no input proof: %+v, %v", afterSecond, err)
	}
}

func indexLogErrors(t *testing.T, database *store.Store, sid ingest.SessionID) int {
	t.Helper()
	conn, err := database.Pool().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	count := 0
	if err := sqlitex.ExecuteTransient(conn, "SELECT COUNT(*) FROM index_log WHERE session_id = ? AND outcome = 'error'", &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return count
}
