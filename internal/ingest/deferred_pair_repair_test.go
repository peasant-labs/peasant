package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	PairState           string `yaml:"pairState"`
	NativeAvailable     bool   `yaml:"nativeAvailable"`
	WantIndexed         bool   `yaml:"wantIndexed"`
	WantChecksumRefusal bool   `yaml:"wantChecksumRefusal"`
	WantPairUnchanged   bool   `yaml:"wantPairUnchanged"`
}

type deferredSelectionRead struct {
	Name                 string `yaml:"name"`
	Discovered           bool   `yaml:"discovered"`
	PairState            string `yaml:"pairState"`
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
	validate := func(name, pairState string) {
		if name == "" || names[name] {
			t.Fatalf("invalid deferred pair repair fixture %q", name)
		}
		switch pairState {
		case "healthy", "damaged":
		default:
			t.Fatalf("deferred pair repair fixture %s names unknown pair state %q", name, pairState)
		}
		names[name] = true
	}
	for _, fixture := range document.Cases {
		validate(fixture.Name, fixture.PairState)
	}
	for _, fixture := range document.SelectionReads {
		validate(fixture.Name, fixture.PairState)
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

// seedDeferredPair stores one session with an intact pair, records the pair's
// identity, and then either leaves the intact transcript in place or replaces
// it with a transcript that no longer matches the recorded identity. The
// stored ingest clock is set older than the discovered mod time so an ordinary
// diff queues the session.
func seedDeferredPair(t *testing.T, ctx context.Context, database *store.Store, memfs *testutil.MemFS, fixture deferredPairRepairDocument, pairState string) deferredPairSeed {
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
	if pairState == "damaged" {
		if err := memfs.WriteFile(transcriptPath, []byte(fixture.DamagedTranscript), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Make the row a pair-repair candidate: a stale index revision is what the
	// selection inventory enumerates.
	seedStalePreviewCapture(t, ctx, database, id)
	metadataBefore, err := memfs.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
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

func deferredPairDiscovered(seed deferredPairSeed) []ingest.DiscoveredSession {
	return []ingest.DiscoveredSession{{
		SessionID:    seed.id,
		Harness:      ingest.HarnessClaudeCode,
		SourcePath:   ingest.ResolvedPath(seed.nativePath),
		SourceFormat: ingest.SourceFormatJSONL,
		ModTime:      time.Now(),
	}}
}

func runDeferredPairPipeline(t *testing.T, ctx context.Context, filesystem ingest.FileSystem, database *store.Store, config ingest.PipelineConfig, adapters map[ingest.Harness]ingest.AdapterFactory) *ingest.PipelineResult {
	t.Helper()
	pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), adapters, config,
		ingest.WithIndexers(ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})),
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

// TestDeferredPairRepairDetection drives the production pipeline over stored
// sessions the ordinary diff has already queued and asserts that their saved
// pair decides the outcome at the point of use: a healthy pair indexes from
// the retained copy even when native acquisition fails, a damaged pair refuses
// and writes nothing, and an unqueued damaged candidate is still repaired.
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

			seed := seedDeferredPair(t, ctx, database, memfs, document, fixture.PairState)
			var discoveredSessions []ingest.DiscoveredSession
			if fixture.Discovered {
				discoveredSessions = deferredPairDiscovered(seed)
			}
			adapters := deferredPairAdapters(t, seed, []byte(document.NativeTranscript), discoveredSessions, fixture.NativeAvailable)
			config := makePipelineConfig(testOutputDir)

			result := runDeferredPairPipeline(t, ctx, memfs, database, config, adapters)

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
				if !bytes.Equal(transcriptAfter, []byte(document.DamagedTranscript)) {
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

func (counter *managedPairReadCounter) ReadFile(path string) ([]byte, error) {
	if counter.paths[path] {
		counter.reads++
	}
	return counter.MemFS.ReadFile(path)
}

// TestDeferredPairRepairSelectionReads measures how many times selection reads
// a stored pair. A dry run stops after the filter pass, so every pair read it
// performs belongs to selection. An already-queued healthy candidate must be
// read zero times; an unqueued damaged candidate must still be read, which
// proves the counter can see a selection read at all.
//
// Measured reduction before and after detection was deferred for the
// already-queued candidate: 2 managed-pair reads (metadata and transcript)
// during selection before, 0 after. The unqueued damaged candidate still reads
// its pair (2) to reach the appended repair entry.
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

			seed := seedDeferredPair(t, ctx, database, memfs, document, fixture.PairState)
			counter := &managedPairReadCounter{
				MemFS: memfs,
				paths: map[string]bool{seed.metadataPath: true, seed.transcriptPath: true},
			}
			var discoveredSessions []ingest.DiscoveredSession
			if fixture.Discovered {
				discoveredSessions = deferredPairDiscovered(seed)
			}
			adapters := deferredPairAdapters(t, seed, []byte(document.NativeTranscript), discoveredSessions, false)
			config := makePipelineConfig(testOutputDir)
			config.DryRun = true

			runDeferredPairPipeline(t, ctx, counter, database, config, adapters)

			if fixture.WantPairReads != nil && counter.reads != *fixture.WantPairReads {
				t.Fatalf("selection read the saved pair %d times, want %d", counter.reads, *fixture.WantPairReads)
			}
			if fixture.WantPairReadsAtLeast > 0 && counter.reads < fixture.WantPairReadsAtLeast {
				t.Fatalf("selection read the saved pair %d times, want at least %d", counter.reads, fixture.WantPairReadsAtLeast)
			}
		})
	}
}
