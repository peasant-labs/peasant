package ingest_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestIntactPairMalformedSourceSettles holds the shape where the stored pair is
// still the one the row recorded, so no repair is owed, but its transcript
// cannot give the session complete content.
//
// Both lanes used to report it on every harvest. It must report the cause once
// from each lane, keep the stored copy serving, stay quiet afterwards, and take
// the session back into the index lane once the bytes are repaired.
func TestIntactPairMalformedSourceSettles(t *testing.T) {
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
	valid := []byte(missingPairNativeTranscript)
	malformed := []byte(damagedSourceTranscript)
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	if err := memfs.WriteFile(nativePath, malformed, 0600); err != nil {
		t.Fatal(err)
	}

	// The pair still matches the identity the row records: nothing to repair.
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source.Format = ingest.SourceFormatJSONL
	meta.Source.FilePath = nativePath
	meta.ContentHash = schema.ComputeTranscriptHash(malformed)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
	transcriptPath := filepath.Join(sessionDir, id.String()+"--transcript.jsonl")
	if err := memfs.WriteFile(transcriptPath, malformed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := memfs.WriteFile(filepath.Join(sessionDir, id.String()+"--metadata.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	pair, err := ingest.NewManagedArtifact(encoded, malformed)
	if err != nil {
		t.Fatal(err)
	}
	mirrored := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
	if len(mirrored) != 1 || mirrored[0].Err != nil || !mirrored[0].Mirrored {
		t.Fatalf("seed pair identity: %+v", mirrored)
	}
	// Incomplete capture, so stored-content recovery owns the session too.
	seedStalePreviewCapture(t, ctx, database, id)

	fresh := makeMinimalMeta(t, id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.FilePath = nativePath
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.ContentHash = schema.ComputeTranscriptHash(malformed)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
	}
	run := func() *ingest.PipelineResult {
		t.Helper()
		pipeline, err := newTestPipeline(memfs, testutil.DefaultGitResolver(), adapters, makePipelineConfig(testOutputDir),
			ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database), ingest.WithLogger(database),
			ingest.WithIndexers(ingest.NewIndexerRegistry(memfs, ingest.IndexerRegistryOptions{})))
		if err != nil {
			t.Fatal(err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			t.Fatalf("harvest: %v", err)
		}
		return result
	}
	ours := func(result *ingest.PipelineResult) []ingest.DiagnosticEntry {
		t.Helper()
		var found []ingest.DiagnosticEntry
		for _, diagnostic := range result.Diagnostics {
			if strings.Contains(diagnostic.Location, id.String()) {
				found = append(found, diagnostic)
			}
		}
		return found
	}
	count := func(diagnostics []ingest.DiagnosticEntry, errorType string) int {
		n := 0
		for _, diagnostic := range diagnostics {
			if diagnostic.ErrorType == errorType {
				n++
			}
		}
		return n
	}
	storedUnchanged := func() {
		t.Helper()
		kept, err := memfs.ReadFile(transcriptPath)
		if err != nil || !bytes.Equal(kept, malformed) {
			t.Fatalf("the stored transcript changed: %q, %v", kept, err)
		}
	}

	first := run()
	firstOurs := ours(first)
	if count(firstOurs, "content_recovery_unavailable") != 1 || count(firstOurs, "index_refused") != 1 {
		t.Fatalf("the first harvest did not report each cause once: %+v", first.Diagnostics)
	}
	for _, diagnostic := range firstOurs {
		if diagnostic.ErrorType != "content_recovery_unavailable" && diagnostic.ErrorType != "index_refused" {
			continue
		}
		if !strings.Contains(diagnostic.Message, "malformed") || !strings.Contains(diagnostic.Message, "record 2") {
			t.Fatalf("the diagnostic does not name the record: %q", diagnostic.Message)
		}
	}
	if first.Summary.Errors != 0 {
		t.Fatalf("a readable stored copy was counted as a failed ingest: %+v", first.Summary)
	}
	storedUnchanged()
	// The in-memory index log is not the settled record. The store row is.
	firstReason, err := database.ReadSourceUnavailability(ctx, id)
	if err != nil || firstReason == nil || *firstReason != ingest.SourceUnavailableRecordUnusable {
		t.Fatalf("settled cause = %v, %v; want %s", firstReason, err, ingest.SourceUnavailableRecordUnusable)
	}

	// The stored copy still serves; a second harvest owes the user nothing.
	// The index attempt is still audited, so a repaired source is not the
	// only thing that proves the parse kept running.
	second := run()
	for _, diagnostic := range ours(second) {
		if diagnostic.ErrorType == "content_recovery_unavailable" || diagnostic.ErrorType == "index_refused" {
			t.Fatalf("the settled refusal was reported again: %+v", diagnostic)
		}
	}
	if second.Summary.Errors != 0 {
		t.Fatalf("a settled session failed: %+v", second.Summary)
	}
	storedUnchanged()
	audited := false
	for _, entry := range second.IndexLog {
		if entry.SessionID == id && entry.Outcome == ingest.IndexOutcomeError {
			audited = true
		}
	}
	if !audited {
		t.Fatalf("the quiet harvest dropped the index audit row: %+v", second.IndexLog)
	}
	quietReason, err := database.ReadSourceUnavailability(ctx, id)
	if err != nil || quietReason == nil || *quietReason != *firstReason {
		t.Fatalf("the quiet harvest changed the stored cause: %v, %v; want %s", quietReason, err, *firstReason)
	}

	// Repairing the bytes returns the session to the index lane.
	if err := memfs.WriteFile(nativePath, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if err := memfs.WriteFile(transcriptPath, valid, 0600); err != nil {
		t.Fatal(err)
	}
	third := run()
	if third.Summary.Errors != 0 {
		t.Fatalf("a repaired source failed: %+v, diagnostics=%+v", third.Summary, third.Diagnostics)
	}
	if third.Summary.Indexed != 1 {
		t.Fatalf("a repaired source did not return the session to the index lane: %+v", third.Summary)
	}
}
