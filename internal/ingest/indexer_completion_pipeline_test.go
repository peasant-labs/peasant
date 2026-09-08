package ingest_test

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

func TestConcreteParserFailurePreservesOtherSessions(t *testing.T) {
	covered := make(map[ingest.Harness]bool)
	for _, fixture := range loadIndexFormatOutputFixtures(t) {
		if fixture.HealthyTranscript == "" {
			continue
		}
		if covered[fixture.Harness] {
			t.Fatalf("duplicate healthy-sibling fixture for %s", fixture.Harness)
		}
		covered[fixture.Harness] = true
		t.Run(fixture.Name, func(t *testing.T) {
			filesystem := testutil.NewMemFS()
			database, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			badID, goodID := schema.SessionID(testutil.TestSessionUUID), schema.SessionID(testutil.TestSessionUUID2)
			beforeInputs := make(map[string][]byte)
			seedCompletionPeer(t, filesystem, database, fixture.Harness, badID, fixture.Transcript, beforeInputs)
			seedCompletionPeer(t, filesystem, database, fixture.Harness, goodID, fixture.HealthyTranscript, beforeInputs)
			seedCompletionSourceFiles(t, filesystem, fixture.SourceRoot, badID, fixture.SourceFiles, beforeInputs)
			seedCompletionSourceFiles(t, filesystem, fixture.SourceRoot, goodID, fixture.HealthySourceFiles, beforeInputs)
			beforeEntries, err := database.ListEntries(t.Context(), badID)
			if err != nil {
				t.Fatal(err)
			}
			beforeState := readMetadataPolicyIndexState(t, database, badID)
			config := makePipelineConfig(testOutputDir)
			config.Reindex = true
			if fixture.SourceRoot != "" {
				config.Sources = map[ingest.Harness]ingest.SourceConfig{fixture.Harness: {Enabled: true, Paths: []ingest.ResolvedPath{fixture.SourceRoot}}}
			}
			indexer := ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})[fixture.Harness]
			pipeline, err := ingest.NewPipeline(filesystem, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{fixture.Harness: makeStubAdapter(nil, nil)}, config,
				ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database),
				ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{fixture.Harness: indexer}))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Indexed != 1 || result.Summary.Errors != 0 || len(result.Diagnostics) != 1 || len(result.IndexLog) != 2 {
				t.Fatalf("malformed session blocked its healthy sibling or hid its failure: %+v, diagnostics=%+v logs=%+v", result.Summary, result.Diagnostics, result.IndexLog)
			}
			afterEntries, err := database.ListEntries(t.Context(), badID)
			if err != nil || !reflect.DeepEqual(beforeEntries, afterEntries) {
				t.Fatalf("partial parser output replaced last-good entries: %v", err)
			}
			if after := readMetadataPolicyIndexState(t, database, badID); after != beforeState {
				t.Fatalf("failed parser advanced producer/hash/time: before=%+v after=%+v", beforeState, after)
			}
			goodEntries, err := database.ListEntries(t.Context(), goodID)
			if err != nil || len(goodEntries) == 0 || goodEntries[0].ContentPreview == nil || *goodEntries[0].ContentPreview != "healthy session" {
				t.Fatalf("healthy session not indexed: %+v %v", goodEntries, err)
			}
			goodState := readMetadataPolicyIndexState(t, database, goodID)
			if goodState.IndexerVersion != ingest.HarvesterVersionRegistry[fixture.Harness].IndexerVersion {
				t.Fatal("healthy session did not commit the actual parser revision")
			}
			retry, err := pipeline.Run(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if retry.Summary.Indexed != 0 || retry.Summary.Errors != 0 || len(retry.Diagnostics) != 1 || len(retry.IndexLog) != 1 || retry.IndexLog[0].SessionID != badID {
				t.Fatalf("next invocation did not retry only the incomplete parser: %+v diagnostics=%+v logs=%+v", retry.Summary, retry.Diagnostics, retry.IndexLog)
			}
			if after := readMetadataPolicyIndexState(t, database, goodID); after != goodState {
				t.Fatal("retry unnecessarily re-indexed healthy session")
			}
			if after := readMetadataPolicyIndexState(t, database, badID); after != beforeState {
				t.Fatal("retry certified incomplete input")
			}
			for path, before := range beforeInputs {
				after, err := filesystem.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("input %s changed: %v", path, err)
				}
			}
		})
	}
	registry := ingest.NewIndexerRegistry(nil, ingest.IndexerRegistryOptions{})
	for harness := range registry {
		if !covered[harness] {
			t.Errorf("required concrete healthy-sibling proof missing for %s", harness)
		}
	}
	for harness := range covered {
		if _, ok := registry[harness]; !ok {
			t.Errorf("unexpected fixture harness %s", harness)
		}
	}
}

func seedCompletionPeer(t *testing.T, filesystem *testutil.MemFS, database *store.Store, harness ingest.Harness, sessionID schema.SessionID, transcript string, before map[string][]byte) {
	t.Helper()
	metadata := makeReindexMeta(t, string(sessionID), "/synthetic/original.jsonl")
	metadata.ModelHarness = harness
	metadata.Project.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if harness == ingest.HarnessOpenCode {
		metadata.Source.Format = ingest.SourceFormatJSON
	}
	metadataPath, transcriptPath := setupPeasantSyncSession(t, filesystem, testOutputDir, testutil.TestHostSlug, string(sessionID), metadata)
	if err := filesystem.WriteFile(transcriptPath, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	before[transcriptPath] = []byte(transcript)
	if err := database.InsertSessions(t.Context(), []ingest.StoreEntry{{Metadata: metadata}}); err != nil {
		t.Fatal(err)
	}
	// Establish the real retained-artifact mirror before recording an immutable
	// baseline. Its first reconciliation legitimately adds the DerivedAt cache.
	publisher, err := ingest.NewArtifactPublisher(filesystem, testOutputDir, ingest.ArtifactPublisherOptions{Mirror: database})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := publisher.ReconcileStored(t.Context(), sessionID, metadataPath, nil); err != nil {
		t.Fatal(err)
	}
	data, err := filesystem.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	before[metadataPath] = data
	previous := "last-good indexed content"
	entries := []schema.SessionEntry{{SessionID: sessionID, Harness: harness, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &previous}}
	result := database.IndexSessionEntryBatch(t.Context(), []ingest.SessionEntryWrite{{SessionID: sessionID, Result: indexformat.V1{Entries: entries}, IndexVersion: 1, IndexerVersion: ingest.HarvesterVersionRegistry[harness].IndexerVersion - 1, IndexedAtMs: 1700000000000}})
	if !result[0].Written {
		t.Fatalf("seed prior index: %v", result[0].Err)
	}
}

func seedCompletionSourceFiles(t *testing.T, filesystem *testutil.MemFS, root ingest.ResolvedPath, sessionID schema.SessionID, files map[string]string, before map[string][]byte) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root.String(), strings.ReplaceAll(name, "SESSION_ID", string(sessionID)))
		if err := filesystem.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		before[path] = []byte(content)
	}
}
