package ingest_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestFallbackRunIndexesValidatedGeneration runs a forced harvest whose
// native acquisition fails while a writer installs the next generation
// between the fallback capture and the index capture. The run must index
// the generation the fallback validated, keep serving it from the stored
// row, and leave the row's artifact identity alone.
func TestFallbackRunIndexesValidatedGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture := loadPairSnapshotExtFixture(t)
	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	memfs := testutil.NewMemFS()
	metadataPath := ingest.SessionMetadataPath(testOutputDir, fixture.HostSlug, fixture.SessionID, "")
	transcriptPath := snapshotTranscriptPath(metadataPath, fixture.SessionID)
	firstJSON := buildPairSnapshotMetadata(t, fixture, fixture.FirstTranscript)
	secondJSON := buildPairSnapshotMetadata(t, fixture, fixture.SecondTranscript)
	firstArtifact, err := ingest.NewManagedArtifact(firstJSON, []byte(fixture.FirstTranscript))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(t.TempDir() + "/peasant.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := publishIndexInputFixture(ctx, database, memfs, testOutputDir, firstArtifact, metadataPath); err != nil {
		t.Fatal(err)
	}
	nativePath := "/sources/" + fixture.SessionID + ".jsonl"
	if err := memfs.WriteFile(nativePath, []byte(fixture.FirstTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	session := ingest.DiscoveredSession{
		SessionID:    sid,
		Harness:      ingest.HarnessClaudeCode,
		SourcePath:   ingest.ResolvedPath(nativePath),
		SourceFormat: ingest.SourceFormatJSONL,
		ModTime:      time.Now().Add(-2 * time.Hour),
	}
	adapter := &testutil.StubAdapter{
		ProviderValue: ingest.HarnessClaudeCode,
		Sessions:      []ingest.DiscoveredSession{session},
		ExtractErr:    errors.New("native source unavailable for refresh"),
	}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: func(ingest.FileSystem, ingest.GitResolver, salt.Salt) ingest.SourceAdapter { return adapter },
	}
	filesystem := &swapPairFS{
		MemFS:          memfs,
		transcriptPath: transcriptPath,
		metadataPath:   metadataPath,
		nextMetadata:   secondJSON,
		nextTranscript: []byte(fixture.SecondTranscript),
	}
	config := makePipelineConfig(testOutputDir)
	config.Force = true
	pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), adapters, config,
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	// Baseline: with no writer active, the fallback carries its validated
	// pair and the run indexes it. This proves the seed is healthy before
	// the concurrent write is introduced.
	baseline, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Summary.Indexed != 1 {
		t.Fatalf("the baseline harvest indexed %d sessions, want 1: summary=%+v diagnostics=%+v", baseline.Summary.Indexed, baseline.Summary, baseline.Diagnostics)
	}
	// The indexed row is no repair candidate now, so selection reads no
	// pair in the next run and the armed swap lands exactly between the
	// fallback capture and the index capture.
	needing, err := database.ListSessionsNeedingRepair(ctx, map[ingest.Harness]ingest.HarvesterVersions{
		ingest.HarnessClaudeCode: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode],
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(needing) != 0 {
		t.Fatalf("the indexed session is still a repair candidate, so selection would read its pair before the fallback: %v", needing)
	}
	filesystem.arm()
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !filesystem.fired() {
		t.Fatal("the concurrent writer never landed between the fallback capture and the index capture, so the case proves nothing")
	}
	if result.Summary.Indexed != 1 {
		t.Fatalf("the forced harvest indexed %d sessions, want 1: summary=%+v diagnostics=%+v", result.Summary.Indexed, result.Summary, result.Diagnostics)
	}
	entries, err := database.ListEntries(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the forced harvest stored no entries for the validated generation")
	}
	representsFirst, representsSecond := false, false
	for _, entry := range entries {
		if entry.ContentPreview == nil {
			continue
		}
		if strings.Contains(*entry.ContentPreview, "first generation alpha") {
			representsFirst = true
		}
		if strings.Contains(*entry.ContentPreview, "second generation beta") {
			representsSecond = true
		}
	}
	if !representsFirst || representsSecond {
		t.Fatalf("the stored entries do not represent the validated generation: first=%v second=%v entries=%+v", representsFirst, representsSecond, entries)
	}
	state, err := database.ReadIndexState(ctx, sid)
	if err != nil || state == nil || state.ArtifactHash == nil || *state.ArtifactHash != firstArtifact.ArtifactHash {
		t.Fatalf("the stored row no longer identifies the validated generation: state=%+v err=%v", state, err)
	}
}

// TestTornPairRefusesAndChangesNothing tears a saved pair from two valid
// generations: the metadata of the first, the parseable transcript of the
// second. Every reader must refuse it with the checksum error, and neither
// the files nor the stored entries may change.
func TestTornPairRefusesAndChangesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	fixture := loadPairSnapshotExtFixture(t)
	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	memfs := testutil.NewMemFS()
	metadataPath := ingest.SessionMetadataPath(testOutputDir, fixture.HostSlug, fixture.SessionID, "")
	transcriptPath := snapshotTranscriptPath(metadataPath, fixture.SessionID)
	firstJSON := buildPairSnapshotMetadata(t, fixture, fixture.FirstTranscript)
	secondJSON := buildPairSnapshotMetadata(t, fixture, fixture.SecondTranscript)
	if _, err := ingest.NewManagedArtifact(secondJSON, []byte(fixture.SecondTranscript)); err != nil {
		t.Fatalf("the second generation does not stand alone, so the torn pair would prove nothing: %v", err)
	}
	firstArtifact, err := ingest.NewManagedArtifact(firstJSON, []byte(fixture.FirstTranscript))
	if err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(t.TempDir() + "/peasant.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := publishIndexInputFixture(ctx, database, memfs, testOutputDir, firstArtifact, metadataPath); err != nil {
		t.Fatal(err)
	}
	preview := "last-good indexed content"
	writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:      sid,
		Result:         snapshotSeedResult(sid, preview),
		IndexVersion:   1,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
		IndexedAtMs:    1708300860000,
	}})
	if len(writes) != 1 || !writes[0].Written {
		t.Fatalf("seed index entries: %+v", writes)
	}
	// Tear the pair: the first generation's metadata beside the second
	// generation's transcript. Both halves are valid on their own.
	if err := memfs.WriteFile(transcriptPath, []byte(fixture.SecondTranscript), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeMetadata, err := memfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeTranscript, err := memfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeEntries, err := database.ListEntries(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ingest.ReadManagedPair(memfs, testOutputDir, metadataPath, sid)
	if err == nil || !strings.Contains(err.Error(), "transcript checksum does not match committed metadata") {
		t.Fatalf("a torn pair was not refused with the checksum error: %v", err)
	}
	afterMetadata, err := memfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	afterTranscript, err := memfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeMetadata, afterMetadata) || !reflect.DeepEqual(beforeTranscript, afterTranscript) {
		t.Fatal("refusing a torn pair changed the saved files")
	}
	config := makePipelineConfig(testOutputDir)
	config.Reindex = true
	pipeline, err := ingest.NewPipeline(memfs, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}, config,
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database),
		ingest.WithIndexers(ingest.NewIndexerRegistry(memfs, ingest.IndexerRegistryOptions{})))
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.Indexed != 0 {
		t.Fatalf("a torn pair indexed %d sessions, want 0: summary=%+v", result.Summary.Indexed, result.Summary)
	}
	afterEntries, err := database.ListEntries(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeEntries, afterEntries) {
		t.Fatalf("refusing a torn pair changed the stored entries: before=%+v after=%+v", beforeEntries, afterEntries)
	}
	finalMetadata, err := memfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	finalTranscript, err := memfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeMetadata, finalMetadata) || !reflect.DeepEqual(beforeTranscript, finalTranscript) {
		t.Fatal("a rebuild over a torn pair changed the saved files")
	}
}
