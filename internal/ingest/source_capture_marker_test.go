package ingest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// A file-only harvest (no database) has only the owned marker beside the
// metadata to tell whether a source was already read. The marker is written
// with the artifact it describes and is bound to the source fingerprint: a
// run that finds it matching reads the source once to verify and publishes
// nothing, and a run that cannot find it re-extracts conservatively and
// writes it again.
func TestFileOnlyHarvestPersistsSourceCaptureMarker(t *testing.T) {
	mfs := testutil.NewCountingFS(testutil.NewMemFS())
	git := testutil.DefaultGitResolver()
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs.MemFS, sourcePath)
	mfs.ModTimes[sourcePath] = modTime
	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}),
	}
	cfg := makePipelineConfig(testOutputDir)
	run := func(label string) ingest.PipelineSummary {
		t.Helper()
		pipeline, err := newTestPipeline(mfs, git, adapters, cfg)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		result, err := pipeline.Run(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return result.Summary
	}
	if summary := run("first harvest"); summary.New != 1 {
		t.Fatalf("first harvest did not ingest the session: %+v", summary)
	}
	metadataPath := ingest.SessionMetadataPath(testOutputDir, string(meta.HostSlug), testSessionID, "")
	markerPath := strings.TrimSuffix(metadataPath, "--metadata.json") + "--source-capture"
	marker, err := mfs.ReadFile(markerPath)
	if err != nil || len(marker) == 0 {
		t.Fatalf("file-only harvest did not persist its source-capture marker at %s: %v", markerPath, err)
	}
	metadataBefore, err := mfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	mfs.ResetCounts()
	if summary := run("unchanged harvest"); summary.Unchanged != 1 || summary.Updated != 0 || summary.New != 0 {
		t.Fatalf("a present marker did not settle the unchanged source: %+v", summary)
	}
	// The marker is bound to the source fingerprint, so the unchanged decision
	// costs exactly one read of the source and no artifact publication.
	if reads := mfs.ReadCount(sourcePath); reads != 1 {
		t.Fatalf("unchanged source read %d times, want exactly the one fingerprint read", reads)
	}
	if metadataAfter, err := mfs.ReadFile(metadataPath); err != nil || string(metadataAfter) != string(metadataBefore) {
		t.Fatalf("an unchanged source was re-published: %v", err)
	}
	if err := mfs.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	mfs.ResetCounts()
	// An absent marker is unknown, not current: the source is re-read and the
	// marker is written again with the artifact.
	if summary := run("marker-less harvest"); summary.Updated != 1 || summary.Unchanged != 0 {
		t.Fatalf("an absent marker did not force a conservative re-check: %+v", summary)
	}
	if mfs.ReadCount(sourcePath) == 0 {
		t.Fatal("the marker-less run did not re-read the native source")
	}
	if again, err := mfs.ReadFile(markerPath); err != nil || len(again) == 0 {
		t.Fatalf("re-extraction did not restore the marker: %v", err)
	}
}

// TestFileOnlyHarvestRepairsAnArtifactBehindTheAdapterTarget covers the
// store-free counterpart of the row-level adapter repair. The first harvest
// writes the pair and its source-capture marker through the production path
// while the registered adapter target is one revision older; the second
// harvest, at the current target, must republish the identical source once and
// stamp the new revision. The third harvest settles without a write.
func TestFileOnlyHarvestRepairsAnArtifactBehindTheAdapterTarget(t *testing.T) {
	mfs := testutil.NewCountingFS(testutil.NewMemFS())
	git := testutil.DefaultGitResolver()
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs.MemFS, sourcePath)
	mfs.ModTimes[sourcePath] = modTime
	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta}),
	}
	cfg := makePipelineConfig(testOutputDir)
	run := func(label string, options ...ingest.PipelineOption) ingest.PipelineSummary {
		t.Helper()
		pipeline, err := newTestPipeline(mfs, git, adapters, cfg, options...)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		result, err := pipeline.Run(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return result.Summary
	}
	readMetadata := func() ingest.UnifiedMetadata {
		t.Helper()
		data, err := mfs.ReadFile(ingest.SessionMetadataPath(testOutputDir, string(meta.HostSlug), testSessionID, ""))
		if err != nil {
			t.Fatal(err)
		}
		var decoded ingest.UnifiedMetadata
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}

	target := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].AdapterVersion
	previous := maps.Clone(ingest.HarvesterVersionRegistry)
	stale := previous[ingest.HarnessClaudeCode]
	stale.AdapterVersion = target - 1
	previous[ingest.HarnessClaudeCode] = stale

	// The previous build writes the pair and the marker it binds, so the
	// evidence under test is the one production produces.
	if summary := run("previous-adapter harvest", ingest.WithHarvesterVersions(previous)); summary.New != 1 {
		t.Fatalf("previous-adapter harvest did not ingest the session: %+v", summary)
	}
	baseline := readMetadata()
	if baseline.AdapterVersion == nil || *baseline.AdapterVersion != target-1 {
		t.Fatalf("seeded adapter revision = %v, want %d", baseline.AdapterVersion, target-1)
	}

	if summary := run("behind-target harvest"); summary.Updated != 1 || summary.Unchanged != 0 {
		t.Fatalf("an artifact behind the adapter target was not republished: %+v", summary)
	}
	repaired := readMetadata()
	if repaired.AdapterVersion == nil || *repaired.AdapterVersion != target {
		t.Fatalf("repaired artifact adapter revision = %v, want %d", repaired.AdapterVersion, target)
	}
	if repaired.ContentHash != baseline.ContentHash {
		t.Fatalf("repair replaced the captured content: %q -> %q", baseline.ContentHash, repaired.ContentHash)
	}

	mfs.ResetCounts()
	if summary := run("settled harvest"); summary.Unchanged != 1 || summary.Updated != 0 || summary.New != 0 {
		t.Fatalf("a repaired artifact did not settle: %+v", summary)
	}
	if reads := mfs.ReadCount(sourcePath); reads != 1 {
		t.Fatalf("settled harvest read the source %d times, want exactly the one fingerprint read", reads)
	}
	if settled := readMetadata(); settled.AdapterVersion == nil || *settled.AdapterVersion != target {
		t.Fatalf("settled harvest changed the adapter revision: %v", settled.AdapterVersion)
	}
}
