package ingest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/deferred_pair_repair.yaml
var deferredPairRepairFixtureData []byte

type deferredPairRepairDocument struct {
	RequiredNames     []string                 `yaml:"required_names"`
	NativeTranscript  string                   `yaml:"native_transcript"`
	DamagedTranscript string                   `yaml:"damaged_transcript"`
	Cases             []deferredPairRepairCase `yaml:"cases"`
	SelectionReads    []deferredSelectionRead  `yaml:"selection_reads"`
}

type deferredPairRepairCase struct {
	Name                string `yaml:"name"`
	Discovered          bool   `yaml:"discovered"`
	QueueReason         string `yaml:"queueReason"`
	PairState           string `yaml:"pairState"`
	NativeAvailable     bool   `yaml:"nativeAvailable"`
	StoredFingerprint   bool   `yaml:"storedFingerprint"`
	PublicationReady    bool   `yaml:"publicationReady"`
	StaleProducer       bool   `yaml:"staleProducer"`
	WantIndexed         bool   `yaml:"wantIndexed"`
	WantChecksumRefusal bool   `yaml:"wantChecksumRefusal"`
	WantPairUnchanged   bool   `yaml:"wantPairUnchanged"`
}

type deferredSelectionRead struct {
	Name                 string `yaml:"name"`
	Discovered           bool   `yaml:"discovered"`
	QueueReason          string `yaml:"queueReason"`
	PairState            string `yaml:"pairState"`
	NativeAvailable      bool   `yaml:"nativeAvailable"`
	StoredFingerprint    bool   `yaml:"storedFingerprint"`
	PublicationReady     bool   `yaml:"publicationReady"`
	StaleProducer        bool   `yaml:"staleProducer"`
	WantPairReads        *int   `yaml:"wantPairReads"`
	WantPairReadsAtLeast int    `yaml:"wantPairReadsAtLeast"`
}

func loadDeferredPairRepairFixtures(t *testing.T) deferredPairRepairDocument {
	t.Helper()
	var document deferredPairRepairDocument
	decoder := yaml.NewDecoder(bytes.NewReader(deferredPairRepairFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	validate := func(name, pairState string, discovered bool, queueReason string) {
		if name == "" || names[name] {
			t.Fatalf("invalid deferred pair repair fixture %q", name)
		}
		switch pairState {
		case "healthy", "damaged", "missing-metadata":
		default:
			t.Fatalf("deferred pair repair fixture %s names unknown pair state %q", name, pairState)
		}
		switch queueReason {
		case "native-change", "index-readiness":
		default:
			if discovered {
				t.Fatalf("deferred pair repair fixture %s is discovered but names unknown queue reason %q", name, queueReason)
			}
			if queueReason != "" {
				t.Fatalf("deferred pair repair fixture %s names a queue reason while not discovered", name)
			}
		}
		names[name] = true
	}
	for _, fixture := range document.Cases {
		validate(fixture.Name, fixture.PairState, fixture.Discovered, fixture.QueueReason)
	}
	for _, fixture := range document.SelectionReads {
		validate(fixture.Name, fixture.PairState, fixture.Discovered, fixture.QueueReason)
	}
	if err := testutil.RequireFixtureNames("deferred pair repair", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document
}

// deferredPairSeed holds the locators a case needs to arrange its stored pair
// and its native source.
type deferredPairSeed struct {
	id               ingest.SessionID
	metadataPath     string
	transcriptPath   string
	nativePath       string
	metadataBefore   []byte
	transcriptBefore []byte
}

// deferredPairArrangement is the stored state a case needs before the run.
type deferredPairArrangement struct {
	PairState         string
	StoredFingerprint bool
	PublicationReady  bool
}

// seedDeferredPair stores one session with an intact pair, records the pair's
// identity, and then either leaves the intact transcript in place, replaces it
// with a transcript that no longer matches the recorded identity, or removes
// the metadata sidecar. The stored ingest clock is set one hour old, so a
// discovered source mod time in the future of it queues the session for a
// native content change, and a mod time before it leaves index readiness as the
// queue signal. A stored fingerprint and a publication-ready row model a source
// whose post-capture comparison can overrule that clock hint.
func seedDeferredPair(t *testing.T, ctx context.Context, database *store.Store, memfs *testutil.MemFS, fixture deferredPairRepairDocument, arrangement deferredPairArrangement) deferredPairSeed {
	t.Helper()
	id, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	nativeTranscript := []byte(fixture.NativeTranscript)
	nativePath := "/synthetic/native/" + id.String() + ".jsonl"
	if err := memfs.WriteFile(nativePath, nativeTranscript, 0o600); err != nil {
		t.Fatal(err)
	}
	meta := makeMinimalMeta(t, id.String())
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source.Format = ingest.SourceFormatJSONL
	meta.Source.FilePath = nativePath
	meta.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
	// A current producer and schema make the database-first classification
	// answer from the row, so the only pair read a selection can perform is the
	// repair detection this fixture measures.
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	adapterVersion := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].AdapterVersion
	meta.AdapterVersion = &adapterVersion
	ingested := time.Now().Add(-time.Hour).UnixMilli()
	meta.Timestamp.Ingested = &ingested
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
		t.Fatal(err)
	}
	metadataPath := ingest.SessionMetadataPath(testOutputDir, testutil.TestHostSlug, id.String(), "")
	transcriptPath := filepath.Join(filepath.Dir(metadataPath), id.String()+"--transcript.jsonl")
	// Install and record the intact pair, then optionally replace the
	// transcript so the row names bytes the pair no longer holds.
	artifact, err := ingest.NewManagedArtifact(encoded, nativeTranscript)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishIndexInputFixture(ctx, database, memfs, testOutputDir, artifact, metadataPath); err != nil {
		t.Fatal(err)
	}
	var fingerprint []byte
	if arrangement.StoredFingerprint {
		sum := sha256.Sum256(nativeTranscript)
		fingerprint = sum[:]
		if err := mirrorDeferredPair(ctx, database, artifact, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	if arrangement.PublicationReady {
		seedDeferredPublicationReady(t, ctx, database, meta, artifact, fingerprint)
	}
	switch arrangement.PairState {
	case "healthy":
		// The intact pair installed by publishIndexInputFixture stays.
	case "damaged":
		if err := memfs.WriteFile(transcriptPath, []byte(fixture.DamagedTranscript), 0o600); err != nil {
			t.Fatal(err)
		}
	case "missing-metadata":
		if err := memfs.Remove(metadataPath); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown pair state %q", arrangement.PairState)
	}
	if !arrangement.PublicationReady {
		// Make the row a pair-repair candidate: a stale index revision is what
		// the selection inventory enumerates. A publication-ready row is
		// already selected by the crash-repair predicate, and a legacy preview
		// write would clear its binding.
		seedStalePreviewCapture(t, ctx, database, id)
	}
	metadataBefore, metadataErr := memfs.ReadFile(metadataPath)
	if metadataErr != nil {
		metadataBefore = nil
	}
	transcriptBefore, err := memfs.ReadFile(transcriptPath)
	if err != nil {
		t.Fatal(err)
	}
	return deferredPairSeed{
		id: id, metadataPath: metadataPath, transcriptPath: transcriptPath, nativePath: nativePath,
		metadataBefore: metadataBefore, transcriptBefore: transcriptBefore,
	}
}

func mirrorDeferredPair(ctx context.Context, database *store.Store, artifact *ingest.ManagedArtifact, fingerprint []byte) error {
	results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: artifact, SourceFingerprint: fingerprint}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
		return fmt.Errorf("record the source fingerprint for %s: %+v", artifact.Metadata.SessionID, results)
	}
	return nil
}

// seedDeferredPublicationReady establishes the publication binding the store
// must see for a captured source: a source-inspected metadata capture, its
// indexed revision, and a complete content capture at that same revision. It
// leaves the pair on disk alone, so the caller can still damage it.
func seedDeferredPublicationReady(t *testing.T, ctx context.Context, database *store.Store, meta *ingest.UnifiedMetadata, artifact *ingest.ManagedArtifact, fingerprint []byte) {
	t.Helper()
	preview := "publication capture"
	entries := []schema.SessionEntry{{
		SessionID: meta.SessionID, Harness: ingest.HarnessClaudeCode, EntryIndex: 0,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview,
	}}
	content, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	ready := *meta
	ready.ContentHash = schema.ComputeTranscriptHash(content)
	ready.MetadataHash = schema.ComputeMetadataHash(&ready)
	kind := ingest.CWDSourceAbsent
	if ready.CWD != "" {
		kind = ingest.CWDSourceExact
	}
	artifactHash := artifact.ArtifactHash
	revisions, err := database.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{{
		Metadata: &ready, PublicationCapture: true, CWDProvenance: kind,
		ArtifactHash: &artifactHash, SourceFingerprint: fingerprint,
	}})
	if err != nil {
		t.Fatal(err)
	}
	versions := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode]
	writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: meta.SessionID, Result: indexformat.V1{Entries: entries},
		IndexVersion: versions.IndexVersion, RequireFullContent: true,
		CaptureRevision: revisions[meta.SessionID], IndexerVersion: versions.IndexerVersion,
		IndexedAtMs: ready.Timestamp.Start,
	}})
	if len(writes) != 1 || writes[0].Err != nil {
		t.Fatalf("index the publication-ready capture: %+v", writes)
	}
}

func deferredPairAdapters(t *testing.T, seed deferredPairSeed, nativeTranscript []byte, discoveredSessions []ingest.DiscoveredSession, nativeAvailable bool) map[ingest.Harness]ingest.AdapterFactory {
	t.Helper()
	if !nativeAvailable {
		return map[ingest.Harness]ingest.AdapterFactory{
			ingest.HarnessClaudeCode: makeStubAdapterWithErrors(discoveredSessions, nil, map[ingest.SessionID]error{
				seed.id: errors.New("native source unavailable for deferred pair fixture"),
			}),
		}
	}
	fresh := makeMinimalMeta(t, seed.id.String())
	fresh.ModelHarness = ingest.HarnessClaudeCode
	fresh.Source.Format = ingest.SourceFormatJSONL
	fresh.Source.FilePath = seed.nativePath
	fresh.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
	return map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessClaudeCode: makeStubAdapter(discoveredSessions, map[ingest.SessionID]*ingest.UnifiedMetadata{seed.id: fresh}),
	}
}

// deferredPairDiscovered offers the seeded session as discovery sees it. The
// discovered source mod time carries the queue reason: newer than the recorded
// ingest clock for a native content change, older for an index-readiness queue.
func deferredPairDiscovered(t *testing.T, seed deferredPairSeed, queueReason string) []ingest.DiscoveredSession {
	t.Helper()
	return []ingest.DiscoveredSession{{
		SessionID:    seed.id,
		Harness:      ingest.HarnessClaudeCode,
		SourcePath:   ingest.ResolvedPath(seed.nativePath),
		SourceFormat: ingest.SourceFormatJSONL,
		ModTime:      deferredPairModTime(t, queueReason),
	}}
}

func deferredPairModTime(t *testing.T, queueReason string) time.Time {
	t.Helper()
	switch queueReason {
	case "native-change":
		return time.Now()
	case "index-readiness":
		// The seeded ingest clock is one hour old; an older mod time leaves
		// index readiness as the only queue signal.
		return time.Now().Add(-2 * time.Hour)
	default:
		t.Fatalf("unknown deferred pair queue reason %q", queueReason)
		return time.Time{}
	}
}

// deferredPairStaleProducerOption raises this build's adapter target so the
// seeded row is selected by the stale-producer inventory as well. The stored
// producer revision stays one behind, matching the stale index candidate the
// repair selection enumerates.
func deferredPairStaleProducerOption() ingest.PipelineOption {
	versions := maps.Clone(ingest.HarvesterVersionRegistry)
	bumped := versions[ingest.HarnessClaudeCode]
	bumped.AdapterVersion++
	versions[ingest.HarnessClaudeCode] = bumped
	return ingest.WithHarvesterVersions(versions)
}

func deferredPairOptions(staleProducer bool) []ingest.PipelineOption {
	if !staleProducer {
		return nil
	}
	return []ingest.PipelineOption{deferredPairStaleProducerOption()}
}

func runDeferredPairPipeline(t *testing.T, ctx context.Context, filesystem ingest.FileSystem, database *store.Store, config ingest.PipelineConfig, adapters map[ingest.Harness]ingest.AdapterFactory, options ...ingest.PipelineOption) *ingest.PipelineResult {
	t.Helper()
	base := []ingest.PipelineOption{
		ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})),
		ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database),
	}
	pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), adapters, config, append(base, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("harvest: %v", err)
	}
	return result
}

// TestDeferredPairRepairDetection drives the production pipeline over stored
// sessions the ordinary diff has already queued and asserts that their saved
// pair decides the outcome at the point of use: a healthy pair indexes from
// the retained copy even when native acquisition fails, a damaged pair on a
// native-change queue refuses and writes nothing, a damaged pair on an
// index-readiness queue is still repaired natively, and an unqueued damaged
// candidate is still repaired.
func TestDeferredPairRepairDetection(t *testing.T) {
	document := loadDeferredPairRepairFixtures(t)
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := t.Context()
			memfs := testutil.NewMemFS()
			database, err := store.Open(filepath.Join(t.TempDir(), "deferred-pair.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			seed := seedDeferredPair(t, ctx, database, memfs, document, deferredPairArrangement{
				PairState:         fixture.PairState,
				StoredFingerprint: fixture.StoredFingerprint,
				PublicationReady:  fixture.PublicationReady,
			})
			var discoveredSessions []ingest.DiscoveredSession
			if fixture.Discovered {
				discoveredSessions = deferredPairDiscovered(t, seed, fixture.QueueReason)
			}
			adapters := deferredPairAdapters(t, seed, []byte(document.NativeTranscript), discoveredSessions, fixture.NativeAvailable)
			config := makePipelineConfig(testOutputDir)

			result := runDeferredPairPipeline(t, ctx, memfs, database, config, adapters, deferredPairOptions(fixture.StaleProducer)...)

			indexed := result.Summary.Indexed != 0
			if indexed != fixture.WantIndexed {
				t.Fatalf("indexed = %v, want %v: summary=%+v diagnostics=%+v", indexed, fixture.WantIndexed, result.Summary, result.Diagnostics)
			}
			refused := deferredChecksumRefusal(result)
			if refused != fixture.WantChecksumRefusal {
				t.Fatalf("checksum refusal = %v, want %v: diagnostics=%+v", refused, fixture.WantChecksumRefusal, result.Diagnostics)
			}
			metadataAfter, err := memfs.ReadFile(seed.metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			transcriptAfter, err := memfs.ReadFile(seed.transcriptPath)
			if err != nil {
				t.Fatal(err)
			}
			unchanged := bytes.Equal(metadataAfter, seed.metadataBefore) && bytes.Equal(transcriptAfter, seed.transcriptBefore)
			if unchanged != fixture.WantPairUnchanged {
				t.Fatalf("saved pair unchanged = %v, want %v", unchanged, fixture.WantPairUnchanged)
			}
			state, err := database.ReadIndexState(ctx, seed.id)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.WantIndexed && (state == nil || state.IndexedInputHash == nil) {
				t.Fatalf("an indexed session recorded no input proof: %+v", state)
			}
			if !fixture.WantIndexed {
				if state == nil || state.IndexedInputHash != nil {
					t.Fatalf("a refused session changed the stored index state: %+v", state)
				}
				if fixture.PairState == "damaged" && !bytes.Equal(transcriptAfter, []byte(document.DamagedTranscript)) {
					t.Fatalf("a refused session overwrote the damaged transcript: %q", transcriptAfter)
				}
			}
		})
	}
}

func deferredChecksumRefusal(result *ingest.PipelineResult) bool {
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.ErrorType == "metadata_refused" &&
			strings.Contains(diagnostic.Message, "transcript checksum does not match committed metadata") {
			return true
		}
	}
	return false
}

// managedPairReadCounter counts reads of one saved pair while a pipeline runs,
// so a run that must not read the pair during selection is measured rather
// than inferred.
type managedPairReadCounter struct {
	*testutil.MemFS
	paths map[string]bool
	reads int
}

var _ ingest.FileSystem = (*managedPairReadCounter)(nil)

func (counter *managedPairReadCounter) ReadFile(path string) ([]byte, error) {
	if counter.paths[path] {
		counter.reads++
	}
	return counter.MemFS.ReadFile(path)
}

// TestDeferredPairRepairSelectionReads measures how many times selection reads
// a stored pair. A dry run stops after the filter pass, so every pair read it
// performs belongs to selection. An already-queued candidate whose queue reason
// is a native content change must be read zero times; a candidate queued only
// for index readiness and an unqueued damaged candidate must still be read,
// which proves the counter can see a selection read at all.
//
// Measured reduction for the deferred class: the already-queued native-change
// candidate went from 2 managed-pair reads (metadata and transcript) during
// selection before the change to 0 after. The index-readiness candidate and the
// unqueued damaged candidate still read their pair to reach a verdict.
func TestDeferredPairRepairSelectionReads(t *testing.T) {
	document := loadDeferredPairRepairFixtures(t)
	for _, fixture := range document.SelectionReads {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := t.Context()
			memfs := testutil.NewMemFS()
			database, err := store.Open(filepath.Join(t.TempDir(), "deferred-pair-reads.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			seed := seedDeferredPair(t, ctx, database, memfs, document, deferredPairArrangement{
				PairState:         fixture.PairState,
				StoredFingerprint: fixture.StoredFingerprint,
				PublicationReady:  fixture.PublicationReady,
			})
			counter := &managedPairReadCounter{
				MemFS: memfs,
				paths: map[string]bool{seed.metadataPath: true, seed.transcriptPath: true},
			}
			var discoveredSessions []ingest.DiscoveredSession
			if fixture.Discovered {
				discoveredSessions = deferredPairDiscovered(t, seed, fixture.QueueReason)
			}
			adapters := deferredPairAdapters(t, seed, []byte(document.NativeTranscript), discoveredSessions, fixture.NativeAvailable)
			config := makePipelineConfig(testOutputDir)
			config.DryRun = true

			runDeferredPairPipeline(t, ctx, counter, database, config, adapters, deferredPairOptions(fixture.StaleProducer)...)

			if fixture.WantPairReads != nil && counter.reads != *fixture.WantPairReads {
				t.Fatalf("selection read the saved pair %d times, want %d", counter.reads, *fixture.WantPairReads)
			}
			if fixture.WantPairReadsAtLeast > 0 && counter.reads < fixture.WantPairReadsAtLeast {
				t.Fatalf("selection read the saved pair %d times, want at least %d", counter.reads, fixture.WantPairReadsAtLeast)
			}
		})
	}
}
