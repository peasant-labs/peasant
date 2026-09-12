package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/write_path_durability.yaml
var writePathDurabilityYAML []byte

// durabilityFixture is one crash-and-heal case. The behavior lives in the
// scenario handler; the fixture names it, carries the design's contract text
// where a case pins a diagnostic, and anchors the required-name manifest.
type durabilityFixture struct {
	Name             string `yaml:"name"`
	Scenario         string `yaml:"scenario"`
	Variant          string `yaml:"variant"`
	ExpectDiagnostic string `yaml:"expect_diagnostic"`
}

func loadDurabilityFixtures(t *testing.T) []durabilityFixture {
	t.Helper()
	var doc struct {
		Required []string            `yaml:"required_names"`
		Cases    []durabilityFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(writePathDurabilityYAML, &doc); err != nil {
		t.Fatalf("decode durability fixtures: %v", err)
	}
	names := make(map[string]bool, len(doc.Cases))
	for _, c := range doc.Cases {
		if c.Name == "" || c.Scenario == "" || names[c.Name] {
			t.Fatalf("empty or duplicate durability fixture %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, required := range doc.Required {
		if !names[required] {
			t.Fatalf("missing required durability fixture %s", required)
		}
	}
	return doc.Cases
}

// durabilityStubEntries is the canned index result for one ingested session.
// The write path's row and its entries are two separate commits; the fault is
// planted on one of them, so the entries themselves need only be well formed.
func durabilityStubEntries(sid ingest.SessionID) map[ingest.SessionID][]schema.SessionEntry {
	return map[ingest.SessionID][]schema.SessionEntry{
		sid: {
			{SessionID: sid, EntryIndex: 0, Role: ingest.RoleUser, EntryType: ingest.EntryTypeText},
			{SessionID: sid, EntryIndex: 1, Role: ingest.RoleAssistant, EntryType: ingest.EntryTypeText},
		},
	}
}

// newDurabilityPipeline builds a store-backed pipeline over ds whose indexer
// produces entries for every session in the adapter. The store is passed as
// both the session store and the metrics/index store, exactly as production
// wires one store into both roles.
func newDurabilityPipeline(t *testing.T, mfs *testutil.MemFS, ds *durabilityStore, adapters map[ingest.Harness]ingest.AdapterFactory, indexer ingest.TranscriptIndexer, cfg ingest.PipelineConfig) *ingest.Pipeline {
	t.Helper()
	pipeline, err := ingest.NewPipeline(mfs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithStore(ds), ingest.WithMetricsStore(ds),
		ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessClaudeCode: indexer}))
	if err != nil {
		t.Fatalf("build durability pipeline: %v", err)
	}
	return pipeline
}

// TestWritePathDurability proves the crash table of the design: a fault on one
// of the two write commits, then the next run's healing. It is the fixture
// counterpart to the write path's "database is the durability point" claim.
func TestWritePathDurability(t *testing.T) {
	for _, fixture := range loadDurabilityFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			switch fixture.Scenario {
			case "crash_install_mirror":
				runCrashInstallMirror(t)
			case "crash_mirror_entries":
				runCrashMirrorEntries(t, fixture.Variant)
			case "crash_mirror_entries_opencode_legacy":
				runCrashMirrorEntriesOpenCodeLegacy(t)
			case "torn_no_row":
				runTornNoRow(t)
			case "torn_row_present":
				runTornRowPresent(t, fixture.ExpectDiagnostic)
			case "parent_mirror_fails":
				runParentMirrorFails(t, fixture.ExpectDiagnostic)
			case "mixed_transcript_new_metadata_old":
				runMixedTranscriptNewMetadataOld(t, fixture.ExpectDiagnostic)
			case "mixed_torn_write":
				runMixedTornWrite(t, fixture.ExpectDiagnostic)
			default:
				t.Fatalf("unhandled durability scenario %q", fixture.Scenario)
			}
		})
	}
}

// runCrashInstallMirror plants a mirror failure so run 1 installs the pair but
// records no row, then proves a restart re-ingests the session as new.
func runCrashInstallMirror(t *testing.T) {
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	sid, _ := ingest.NewSessionID(testSessionID)
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime
	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta}),
	}
	indexer := &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile, Entries: durabilityStubEntries(sid)}
	dbPath := storetest.CopyGoldenDB(t)

	// Run 1: the crash. The worker installs the pair; the row never commits.
	crashed := newDurabilityStore(t, dbPath)
	crashed.FailMirrorFor(sid, errors.New("simulated crash before the row committed"))
	if _, err := newDurabilityPipeline(t, mfs, crashed, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx); err != nil {
		t.Fatalf("crash run: %v", err)
	}
	metadataPath := ingest.SessionMetadataPath(testOutputDir, string(meta.HostSlug), testSessionID, "")
	if _, err := mfs.ReadFile(metadataPath); err != nil {
		t.Fatalf("the interrupted run should have installed the metadata file: %v", err)
	}
	if state, err := crashed.ReadIndexState(ctx, sid); err != nil {
		t.Fatalf("read index state after crash: %v", err)
	} else if state != nil {
		t.Fatalf("a failed mirror must leave no row; got %+v", state)
	}
	crashed.Shutdown()

	// Run 2: the restart re-ingests from the source because there is no row.
	restarted := newDurabilityStore(t, dbPath)
	result, err := newDurabilityPipeline(t, mfs, restarted, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx)
	if err != nil {
		t.Fatalf("restart run: %v", err)
	}
	// The saved copy survived the crash (the install ran; only the row did
	// not), so the store-less classification fallback returns UPDATED, as the
	// design's crash table states. What matters is that the session is
	// re-ingested rather than skipped as unchanged, and ends up healed.
	if result.Summary.Updated != 1 || result.Summary.Unchanged != 0 || result.Summary.Errors != 0 {
		t.Fatalf("restart summary = %+v, want the session re-ingested (the fallback returns updated), not skipped", result.Summary)
	}
	state, err := restarted.ReadIndexState(ctx, sid)
	if err != nil || state == nil || state.ArtifactHash == nil {
		t.Fatalf("restart must record the row and its pair hash; got %+v, %v", state, err)
	}
}

// runCrashMirrorEntries plants an entry-batch failure so run 1 records the row
// but no entries, then proves the repair predicate selects the session on the
// next run and not on the one after: one harvest heals it. The variant controls
// the row state the crash lands on, proving the predicate is independent of it:
// a fresh row, or a complete row with provenance replaced by changed content.
func runCrashMirrorEntries(t *testing.T, variant string) {
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	sid, _ := ingest.NewSessionID(testSessionID)
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime
	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta}),
	}
	indexer := &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile, Entries: durabilityStubEntries(sid)}
	dbPath := storetest.CopyGoldenDB(t)

	switch variant {
	case "", "fresh":
		// The crash lands on a fresh row: nothing was stored before.
	case "replaced_provenance":
		// Establish a complete row with provenance first, then change the source
		// so the crash run replaces the pair. The mirror clears the input proof
		// when the pair changes, so the crash lands on an already-provenanced row.
		established := newDurabilityStore(t, dbPath)
		if _, err := newDurabilityPipeline(t, mfs, established, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx); err != nil {
			t.Fatalf("establish run: %v", err)
		}
		prior, err := established.ReadIndexState(ctx, sid)
		if err != nil || prior == nil || prior.ArtifactHash == nil || prior.IndexedInputHash == nil {
			t.Fatalf("the establish run must leave a complete, provenanced row; got %+v, %v", prior, err)
		}
		established.Shutdown()
		if err := mfs.WriteFile(sourcePath, []byte(`{"sessionId":"test","type":"user","message":{"role":"user","content":"changed"},"timestamp":"2024-02-19T02:00:00Z"}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		newer := time.Now().Add(-time.Hour)
		mfs.ModTimes[sourcePath] = newer
		session.ModTime = newer
		adapters = map[ingest.Harness]ingest.AdapterFactory{
			ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: makeMinimalMeta(t, testSessionID)}),
		}
	default:
		t.Fatalf("unknown crash_mirror_entries variant %q", variant)
	}

	// Run 1: the crash after the row committed. The entries never commit.
	crashed := newDurabilityStore(t, dbPath)
	crashed.FailEntries(errors.New("simulated crash before the entries committed"))
	if _, err := newDurabilityPipeline(t, mfs, crashed, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx); err != nil {
		t.Fatalf("crash run: %v", err)
	}
	state, err := crashed.ReadIndexState(ctx, sid)
	if err != nil || state == nil || state.ArtifactHash == nil {
		t.Fatalf("the row and its pair hash should be recorded after the crash; got %+v, %v", state, err)
	}
	if state.IndexedInputHash != nil {
		t.Fatalf("the entries never committed, so the indexed-input hash must be null; got %q", *state.IndexedInputHash)
	}
	needing, err := crashed.ListSessionsNeedingRepair(ctx, ingest.HarvesterVersionRegistry)
	if err != nil {
		t.Fatalf("list repair sessions: %v", err)
	}
	if !containsSession(needing, sid) {
		t.Fatalf("the repair predicate must select the crashed session; got %v", needing)
	}
	crashed.Shutdown()

	// Run 2: the restart heals it. The repair predicate selected it, so the
	// entries are recovered and the row's indexed-input hash is set.
	restarted := newDurabilityStore(t, dbPath)
	result, err := newDurabilityPipeline(t, mfs, restarted, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx)
	if err != nil {
		t.Fatalf("restart run: %v", err)
	}
	if result.Summary.Indexed != 1 {
		t.Fatalf("the restart should index the recovered session once; summary = %+v", result.Summary)
	}
	healed, err := restarted.ReadIndexState(ctx, sid)
	if err != nil || healed == nil || healed.IndexedInputHash == nil {
		t.Fatalf("the restart must set the indexed-input hash; got %+v, %v", healed, err)
	}
	// Run 3: the steady state. The repair predicate selects nothing.
	settled, err := restarted.ListSessionsNeedingRepair(ctx, ingest.HarvesterVersionRegistry)
	if err != nil {
		t.Fatalf("list repair sessions after heal: %v", err)
	}
	if containsSession(settled, sid) {
		t.Fatalf("the healed session must not be selected again; got %v", settled)
	}
}

// runCrashMirrorEntriesOpenCodeLegacy proves the repair predicate is
// harness- and kind-independent: it heals an OpenCode legacy directory session,
// whose row is a different kind than a JSONL harness, exactly as it heals a
// claude-code row. The crash fails the entry commit; the next harvest selects
// the row and recovers its entries; the one after selects nothing.
func runCrashMirrorEntriesOpenCodeLegacy(t *testing.T) {
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	const projectHash = "openprojecthash"
	session := setupOpenCodeFixture(t, mfs, testutil.TestSessionUUID, projectHash)
	addOpenCodeMessage(t, mfs, testutil.TestSessionUUID, "msg_one", string(ingest.RoleUser), 10, 0)
	addOpenCodePart(t, mfs, "msg_one", "prt_one")
	addOpenCodeMessage(t, mfs, testutil.TestSessionUUID, "msg_two", string(ingest.RoleAssistant), 0, 20)
	addOpenCodePart(t, mfs, "msg_two", "prt_two")
	sid := session.SessionID
	session.ModTime = time.Now().Add(-2 * time.Hour)

	meta := makeMinimalMeta(t, string(sid))
	meta.Source.FilePath = session.SourcePath.String()
	meta.Source.Format = ingest.SourceFormatJSON
	meta.ModelHarness = ingest.HarnessOpenCode
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessOpenCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta}),
	}
	cfg := makePipelineConfig(testOutputDir)
	cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
		ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{session.OriginalRoot}},
	}
	newOpenCodePipeline := func(ds *durabilityStore) *ingest.Pipeline {
		pipeline, err := ingest.NewPipeline(mfs, testutil.DefaultGitResolver(), adapters, cfg,
			ingest.WithStore(ds), ingest.WithMetricsStore(ds),
			ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{ingest.HarnessOpenCode: ingest.NewOpenCodeIndexer(mfs)}))
		if err != nil {
			t.Fatalf("build opencode pipeline: %v", err)
		}
		return pipeline
	}
	dbPath := storetest.CopyGoldenDB(t)

	// Run 1: the crash after the row committed. The entries never commit.
	crashed := newDurabilityStore(t, dbPath)
	crashed.FailEntries(errors.New("simulated crash before the entries committed"))
	if _, err := newOpenCodePipeline(crashed).Run(ctx); err != nil {
		t.Fatalf("crash run: %v", err)
	}
	state, err := crashed.ReadIndexState(ctx, sid)
	if err != nil || state == nil || state.ArtifactHash == nil {
		t.Fatalf("the opencode row and its pair hash should be recorded after the crash; got %+v, %v", state, err)
	}
	if state.IndexedInputHash != nil {
		t.Fatalf("the entries never committed, so the indexed-input hash must be null; got %q", *state.IndexedInputHash)
	}
	needing, err := crashed.ListSessionsNeedingRepair(ctx, ingest.HarvesterVersionRegistry)
	if err != nil {
		t.Fatalf("list repair sessions: %v", err)
	}
	if !containsSession(needing, sid) {
		t.Fatalf("the repair predicate must select the crashed opencode-legacy row; got %v", needing)
	}
	crashed.Shutdown()

	// Run 2: the restart heals it, recovering the entries and setting the
	// indexed-input hash the crash left null.
	restarted := newDurabilityStore(t, dbPath)
	healResult, err := newOpenCodePipeline(restarted).Run(ctx)
	if err != nil {
		t.Fatalf("restart run: %v", err)
	}
	if healResult.Summary.Indexed != 1 {
		t.Fatalf("the restart should index the recovered opencode session once; summary = %+v", healResult.Summary)
	}
	healed, err := restarted.ReadIndexState(ctx, sid)
	if err != nil || healed == nil || healed.IndexedInputHash == nil {
		t.Fatalf("the restart must set the indexed-input hash for the opencode row; got %+v, %v", healed, err)
	}
	restarted.Shutdown()

	// Run 3: the steady state does no indexing work. A legacy directory kind is
	// unbindable, so the database repair query still lists it on its publication
	// half; the pipeline's own pending-work check filters that out and re-indexes
	// nothing, which is the observable "settled" for every kind.
	steady := newDurabilityStore(t, dbPath)
	steadyResult, err := newOpenCodePipeline(steady).Run(ctx)
	if err != nil {
		t.Fatalf("steady run: %v", err)
	}
	if steadyResult.Summary.Indexed != 0 {
		t.Fatalf("the healed opencode row must not be indexed again; summary = %+v", steadyResult.Summary)
	}
}

// runTornNoRow proves a torn saved pair with no row does not block re-ingest:
// the harvest is database-first, reads the source, and leaves a valid pair.
func runTornNoRow(t *testing.T) {
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	sid, _ := ingest.NewSessionID(testSessionID)
	sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs, sourcePath)
	mfs.ModTimes[sourcePath] = modTime
	session := makeDiscoveredSession(t, testSessionID, sourcePath, modTime)
	meta := makeMinimalMeta(t, testSessionID)

	// Leave a torn saved copy behind: a metadata file naming a hash its
	// transcript bytes cannot produce, with no database row.
	sessionDir := ingest.SessionDir(testOutputDir, string(meta.HostSlug), testSessionID, "")
	seedColumnsPair(t, mfs, sessionDir, testSessionID, meta)
	transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", sessionDir, testSessionID)
	if err := mfs.WriteFile(transcriptPath, []byte("torn write, truncated bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}

	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{sid: meta}),
	}
	indexer := &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile, Entries: durabilityStubEntries(sid)}
	ds := newDurabilityStore(t, storetest.CopyGoldenDB(t))
	result, err := newDurabilityPipeline(t, mfs, ds, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx)
	if err != nil {
		t.Fatalf("harvest over a torn no-row pair: %v", err)
	}
	// A leftover metadata file (torn transcript) with no row makes the
	// store-less fallback return UPDATED; the point is that the unusable saved
	// copy does not block re-ingest.
	if result.Summary.Updated != 1 || result.Summary.Unchanged != 0 || result.Summary.Errors != 0 {
		t.Fatalf("a torn pair with no row must be re-ingested, not skipped; summary = %+v", result.Summary)
	}
	state, err := ds.ReadIndexState(ctx, sid)
	if err != nil || state == nil || state.ArtifactHash == nil {
		t.Fatalf("the row and pair hash should be recorded after re-ingest; got %+v, %v", state, err)
	}
	// The saved pair is valid again: it reads back and matches the row.
	metadataPath := ingest.SessionMetadataPath(testOutputDir, string(meta.HostSlug), testSessionID, "")
	pair, err := ingest.ReadManagedPair(mfs, testOutputDir, metadataPath, sid)
	if err != nil {
		t.Fatalf("the re-ingested pair must be valid; got %v", err)
	}
	if pair.ArtifactHash != *state.ArtifactHash {
		t.Fatalf("saved pair hash %q does not match the recorded row %q", pair.ArtifactHash, *state.ArtifactHash)
	}
}

// runTornRowPresent proves the crash-row-3 state: a torn saved pair whose row
// is present. A plain harvest is database-first and leaves it untouched; the
// rebuild read reports the damaged copy by its exact diagnostic and never
// overwrites the row.
func runTornRowPresent(t *testing.T, expectDiagnostic string) {
	if expectDiagnostic == "" {
		t.Fatal("torn_row_present fixture must carry the expected diagnostic text")
	}
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	output := testOutputDir

	// A valid saved pair whose row is recorded at that pair's hash.
	sid, artifact := writeRebuildPair(t, mfs, output, "11111111-1111-4111-8111-111111111111", "", rebuildTranscript)
	rowHash := artifact.ArtifactHash
	ds := newDurabilityStore(t, storetest.CopyGoldenDB(t))
	if results := ds.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: artifact}}); len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed row: %+v", results)
	}

	// Power loss leaves the transcript torn while the row stays durable.
	transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", ingest.SessionDir(output, testutil.TestHostSlug, sid.String(), ""), sid.String())
	if err := mfs.WriteFile(transcriptPath, []byte(rebuildChangedTranscript), 0600); err != nil {
		t.Fatal(err)
	}

	// A plain harvest is database-first: it never reads the saved copy, never
	// reports it, and never re-ingests (no source is discovered here).
	plain := makePipelineConfig(output)
	plainResult, err := newDurabilityPipeline(t, mfs, ds, map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}, &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile}, plain).Run(ctx)
	if err != nil {
		t.Fatalf("plain harvest: %v", err)
	}
	if len(plainResult.Summary.RebuildSkipped) != 0 {
		t.Fatalf("a plain harvest must not touch the damaged saved copy; skipped = %v", plainResult.Summary.RebuildSkipped)
	}
	if state, err := ds.ReadIndexState(ctx, sid); err != nil || state == nil || state.ArtifactHash == nil || *state.ArtifactHash != rowHash {
		t.Fatalf("the row must be unchanged after a plain harvest; got %+v, %v", state, err)
	}

	// The rebuild read reports the damaged copy by its exact diagnostic and
	// leaves the row and its entries alone.
	rebuild := makePipelineConfig(output, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.RebuildAll = true
		c.Force = true
	})
	rebuildResult, err := newDurabilityPipeline(t, mfs, ds, map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}, &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile}, rebuild).Run(ctx)
	if err != nil {
		t.Fatalf("rebuild read: %v", err)
	}
	if len(rebuildResult.Summary.RebuildSkipped) != 1 || rebuildResult.Summary.RebuildSkipped[0] != sid {
		t.Fatalf("the rebuild must report the damaged saved copy; skipped = %v", rebuildResult.Summary.RebuildSkipped)
	}
	if !hasDiagnosticContaining(rebuildResult.Diagnostics, expectDiagnostic) {
		t.Fatalf("the rebuild must print the damaged-pair diagnostic %q; got %+v", expectDiagnostic, rebuildResult.Diagnostics)
	}
	if state, err := ds.ReadIndexState(ctx, sid); err != nil || state == nil || state.ArtifactHash == nil || *state.ArtifactHash != rowHash {
		t.Fatalf("the damaged copy must never overwrite the row; got %+v, %v", state, err)
	}
}

// runParentMirrorFails plants a mirror failure on the parent only. The child
// is held until the parent commits, then drained in a later transaction where
// the store refuses it because the parent is not stored. Neither row is
// recorded and a restart re-ingests both.
func runParentMirrorFails(t *testing.T, expectDiagnostic string) {
	if expectDiagnostic == "" {
		t.Fatal("parent_mirror_fails fixture must carry the expected diagnostic text")
	}
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	parentID, _ := ingest.NewSessionID(testSessionID)
	childID, _ := ingest.NewSessionID(testSessionID2)
	parentSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
	childSource := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID2)
	modTime := time.Now().Add(-2 * time.Hour)
	setupSourceFile(t, mfs, parentSource)
	setupSourceFile(t, mfs, childSource)
	mfs.ModTimes[parentSource], mfs.ModTimes[childSource] = modTime, modTime

	parent := makeDiscoveredSession(t, testSessionID, parentSource, modTime)
	child := makeDiscoveredSession(t, testSessionID2, childSource, modTime)
	child.ParentUUID = &parentID
	parentMeta := makeMinimalMeta(t, testSessionID)
	childMeta := makeMinimalMeta(t, testSessionID2)
	childMeta.ParentUUID = &parentID
	metas := map[ingest.SessionID]*ingest.UnifiedMetadata{parentID: parentMeta, childID: childMeta}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{parent, child}, metas),
	}
	indexerEntries := durabilityStubEntries(parentID)
	for id, entries := range durabilityStubEntries(childID) {
		indexerEntries[id] = entries
	}
	indexer := &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile, Entries: indexerEntries}
	dbPath := storetest.CopyGoldenDB(t)

	// Run 1: the parent's row fails. The child is refused as its parent is not
	// stored, in a later transaction than the parent's own failed mirror.
	crashed := newDurabilityStore(t, dbPath)
	crashed.FailMirrorFor(parentID, errors.New("simulated crash before the parent row committed"))
	result, err := newDurabilityPipeline(t, mfs, crashed, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx)
	if err != nil {
		t.Fatalf("parent-fail run: %v", err)
	}
	if !hasDiagnosticContaining(result.Diagnostics, expectDiagnostic) {
		t.Fatalf("the child must be refused because its parent is not stored; diagnostics = %+v", result.Diagnostics)
	}
	if state, err := crashed.ReadIndexState(ctx, parentID); err != nil || state != nil {
		t.Fatalf("the failed parent must leave no row; got %+v, %v", state, err)
	}
	if state, err := crashed.ReadIndexState(ctx, childID); err != nil || state != nil {
		t.Fatalf("the refused child must leave no row; got %+v, %v", state, err)
	}
	crashed.Shutdown()

	// Run 2: the restart re-ingests both, parent before child.
	restarted := newDurabilityStore(t, dbPath)
	if _, err := newDurabilityPipeline(t, mfs, restarted, adapters, indexer, makePipelineConfig(testOutputDir)).Run(ctx); err != nil {
		t.Fatalf("restart run: %v", err)
	}
	for _, id := range []ingest.SessionID{parentID, childID} {
		state, err := restarted.ReadIndexState(ctx, id)
		if err != nil || state == nil || state.ArtifactHash == nil {
			t.Fatalf("the restart must record %s; got %+v, %v", id, state, err)
		}
	}
}

// seedValidPairWithRow writes a valid saved pair and records its row at that
// pair's hash, the starting point for a power-loss case where the row is
// durable but the saved copy is later left torn or mixed.
func seedValidPairWithRow(t *testing.T, ctx context.Context, mfs *testutil.MemFS, ds *durabilityStore, output, idStr string) (ingest.SessionID, string) {
	t.Helper()
	sid, artifact := writeRebuildPair(t, mfs, output, idStr, "", rebuildTranscript)
	if results := ds.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: artifact}}); len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed row: %+v", results)
	}
	return sid, artifact.ArtifactHash
}

// rebuildReportsDamaged runs the rebuild read and asserts it reports the saved
// copy as damaged by its exact diagnostic and never overwrites the row.
func rebuildReportsDamaged(t *testing.T, ctx context.Context, mfs *testutil.MemFS, ds *durabilityStore, output string, sid ingest.SessionID, rowHash, expectDiagnostic string) {
	t.Helper()
	rebuild := makePipelineConfig(output, func(c *ingest.PipelineConfig) {
		c.Reindex = true
		c.RebuildAll = true
		c.Force = true
	})
	result, err := newDurabilityPipeline(t, mfs, ds, map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
	}, &testutil.StubIndexer{Kind: ingest.TranscriptSourceFile}, rebuild).Run(ctx)
	if err != nil {
		t.Fatalf("rebuild read: %v", err)
	}
	if len(result.Summary.RebuildSkipped) != 1 || result.Summary.RebuildSkipped[0] != sid {
		t.Fatalf("the rebuild must report the damaged saved copy; skipped = %v", result.Summary.RebuildSkipped)
	}
	if !hasDiagnosticContaining(result.Diagnostics, expectDiagnostic) {
		t.Fatalf("the rebuild must print the damaged-pair diagnostic %q; got %+v", expectDiagnostic, result.Diagnostics)
	}
	if state, err := ds.ReadIndexState(ctx, sid); err != nil || state == nil || state.ArtifactHash == nil || *state.ArtifactHash != rowHash {
		t.Fatalf("the damaged copy must never overwrite the row; got %+v, %v", state, err)
	}
}

// runMixedTranscriptNewMetadataOld builds the crash-reachable mixed ordering:
// the transcript is the new content while the metadata is still the old file
// naming the old transcript hash. The pair is refused by hash on a direct read
// and reported as damaged by the rebuild.
func runMixedTranscriptNewMetadataOld(t *testing.T, expectDiagnostic string) {
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	output := testOutputDir
	ds := newDurabilityStore(t, storetest.CopyGoldenDB(t))
	sid, rowHash := seedValidPairWithRow(t, ctx, mfs, ds, output, "11111111-1111-4111-8111-111111111111")

	sessionDir := ingest.SessionDir(output, testutil.TestHostSlug, sid.String(), "")
	metadataPath := ingest.SessionMetadataPath(output, testutil.TestHostSlug, sid.String(), "")
	transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", sessionDir, sid.String())
	// The metadata is the old file that names the old transcript hash: capture
	// it, then install the new transcript without rewriting the metadata, which
	// is exactly the ordering a crash before the metadata-last rename leaves.
	oldMetadata, err := mfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := mfs.WriteFile(transcriptPath, []byte(rebuildChangedTranscript), 0600); err != nil {
		t.Fatal(err)
	}
	if current, err := mfs.ReadFile(metadataPath); err != nil || !bytes.Equal(current, oldMetadata) {
		t.Fatalf("the metadata must still be the old file after the transcript changed; err=%v", err)
	}
	// A direct read refuses the mixed pair by its hash: it is never served.
	if _, err := ingest.ReadManagedPair(mfs, output, metadataPath, sid); err == nil {
		t.Fatal("a mixed pair whose metadata names a different hash must be refused on read")
	}
	rebuildReportsDamaged(t, ctx, mfs, ds, output, sid, rowHash, expectDiagnostic)
}

// runMixedTornWrite leaves the transcript truncated mid-write while the row and
// metadata name a whole-file hash. The pair is refused by hash on a direct read
// and reported as damaged by the rebuild.
func runMixedTornWrite(t *testing.T, expectDiagnostic string) {
	ctx := context.Background()
	mfs := testutil.NewMemFS()
	output := testOutputDir
	ds := newDurabilityStore(t, storetest.CopyGoldenDB(t))
	sid, rowHash := seedValidPairWithRow(t, ctx, mfs, ds, output, "11111111-1111-4111-8111-111111111111")

	sessionDir := ingest.SessionDir(output, testutil.TestHostSlug, sid.String(), "")
	metadataPath := ingest.SessionMetadataPath(output, testutil.TestHostSlug, sid.String(), "")
	transcriptPath := fmt.Sprintf("%s/%s--transcript.jsonl", sessionDir, sid.String())
	// A torn write: the transcript is truncated to a partial line.
	if err := mfs.WriteFile(transcriptPath, []byte(rebuildTranscript[:len(rebuildTranscript)/2]), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.ReadManagedPair(mfs, output, metadataPath, sid); err == nil {
		t.Fatal("a torn transcript must be refused on read against the metadata hash")
	}
	rebuildReportsDamaged(t, ctx, mfs, ds, output, sid, rowHash, expectDiagnostic)
}

func containsSession(ids []ingest.SessionID, want ingest.SessionID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func hasDiagnosticContaining(diagnostics []ingest.DiagnosticEntry, needle string) bool {
	for _, d := range diagnostics {
		if strings.Contains(d.Message, needle) {
			return true
		}
	}
	return false
}
