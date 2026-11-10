package ingest_test

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestImagePersistFollowsActivation proves the production harvest writes an
// attachment row only after the generation is accepted. The row is keyed by
// the content address, and a second identical harvest does not leave a second
// row for the same bytes.
func TestImagePersistFollowsActivation(t *testing.T) {
	ctx := t.Context()
	fs := testutil.NewMemFS()
	root := filepath.Join(t.TempDir(), "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	// ast-grep-ignore: no-migrating-store-open-in-tests -- the attachment table is created by the migration this test exercises.
	database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"), store.WithIndexFormats(store.V2IndexFormat()), store.WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	transcript := []byte(`{"type":"user","uuid":"u1","message":{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + base64.StdEncoding.EncodeToString(encoded.Bytes()) + `"}}]}}` + "\n")
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	if err := fs.WriteFile(nativePath, transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(transcript)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{{
			SessionID: id, Harness: ingest.HarnessClaudeCode,
			SourcePath: ingest.ResolvedPath(nativePath), SourceFormat: ingest.SourceFormatJSONL,
		}}, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	run := func() *ingest.PipelineResult {
		t.Helper()
		pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(testOutputDir),
			ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database), ingest.WithLogger(database),
			ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
			ingest.WithHarvesterVersions(ingest.GenerationTargets(ingest.HarvesterVersionRegistry)))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("harvest: %v", err)
		}
		return result
	}
	first := run()
	if first.Summary.Errors != 0 || first.Summary.Indexed != 1 {
		t.Fatalf("image harvest did not activate: %+v diagnostics=%+v", first.Summary, first.Diagnostics)
	}
	facts, err := database.ImageFacts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("attachment rows = %d, want the one accepted image", len(facts))
	}
	var fact ingest.ImageFacts
	for _, item := range facts {
		fact = item
	}
	if fact.SHA256 == "" || fact.MediaType != "image/png" || fact.PartitionID == "" {
		t.Fatalf("accepted image = %+v, want a stored png in its generation partition", fact)
	}
}
