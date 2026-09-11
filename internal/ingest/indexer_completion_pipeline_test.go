package ingest_test

import (
	"bytes"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
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
			seedCompletionPeer(t, filesystem, database, fixture.Harness, badID, fixture.TranscriptIdentity.expandSessionPlaceholder(fixture.Transcript, badID), beforeInputs)
			seedCompletionPeer(t, filesystem, database, fixture.Harness, goodID, fixture.TranscriptIdentity.expandSessionPlaceholder(fixture.HealthyTranscript, goodID), beforeInputs)
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
			if result.Summary.Indexed != 1 || result.Summary.Errors != 0 || len(result.IndexLog) != 2 {
				t.Fatalf("malformed session blocked its healthy sibling or hid its failure: %+v, diagnostics=%+v logs=%+v", result.Summary, result.Diagnostics, result.IndexLog)
			}
			assertCompletionDiagnostics(t, "first run", result.Diagnostics, map[schema.SessionID][]completionDiagnosticCode{
				badID:  {diagnosticIndexRefused, diagnosticContentRecoveryUnavailable},
				goodID: {diagnosticContentRecoveryUnavailable},
			})
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
			if retry.Summary.Indexed != 0 || retry.Summary.Errors != 0 || len(retry.IndexLog) != 1 || retry.IndexLog[0].SessionID != badID {
				t.Fatalf("next invocation did not retry only the incomplete parser: %+v diagnostics=%+v logs=%+v", retry.Summary, retry.Diagnostics, retry.IndexLog)
			}
			assertCompletionDiagnostics(t, "retry", retry.Diagnostics, map[schema.SessionID][]completionDiagnosticCode{
				badID: {diagnosticIndexRefused, diagnosticContentRecoveryUnavailable},
			})
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

// completionDiagnosticCode is the closed set of diagnostic codes a mixed
// healthy/malformed run may report. A code outside this set is a change in
// what the run tells the user and must be read before it is accepted, so the
// assertion below refuses an unknown code rather than ignoring it.
type completionDiagnosticCode string

const (
	// diagnosticIndexRefused reports the refused parse of the malformed peer.
	diagnosticIndexRefused completionDiagnosticCode = "index_refused"
	// diagnosticContentRecoveryUnavailable reports that complete content could
	// not be recovered for a session whose stored index holds previews only.
	// Both peers are seeded in exactly that state, and the malformed peer's
	// transcript is the very thing the run must refuse, so this code is what
	// the seed implies for both of them.
	diagnosticContentRecoveryUnavailable completionDiagnosticCode = "content_recovery_unavailable"
)

// assertCompletionDiagnostics pins the exact SET of diagnostic codes each
// session carries, which says more than a total count: it names which session
// is refused, proves the healthy peer is never refused, and fails on any
// diagnostic that names neither peer or names an unknown code.
func assertCompletionDiagnostics(t *testing.T, stage string, diagnostics []schema.DiagnosticEntry, want map[schema.SessionID][]completionDiagnosticCode) {
	t.Helper()
	got := make(map[schema.SessionID][]completionDiagnosticCode, len(want))
	for _, diagnostic := range diagnostics {
		code := completionDiagnosticCode(diagnostic.ErrorType)
		if code != diagnosticIndexRefused && code != diagnosticContentRecoveryUnavailable {
			t.Fatalf("%s reported the unrecognised diagnostic code %q for %q; read the new diagnostic and either name it in completionDiagnosticCode or fix what produces it", stage, diagnostic.ErrorType, diagnostic.Location)
		}
		named := schema.SessionID("")
		for sessionID := range want {
			if strings.Contains(diagnostic.Location, string(sessionID)) {
				named = sessionID
			}
		}
		if named == "" {
			t.Fatalf("%s reported %q at %q, which names neither peer under test; a diagnostic a user cannot attribute to a session is not usable", stage, diagnostic.ErrorType, diagnostic.Location)
		}
		got[named] = append(got[named], code)
	}
	for sessionID, expected := range want {
		actual := append([]completionDiagnosticCode(nil), got[sessionID]...)
		sorted := append([]completionDiagnosticCode(nil), expected...)
		slices.Sort(actual)
		slices.Sort(sorted)
		if !slices.Equal(actual, sorted) {
			t.Fatalf("%s reported %v for session %s, want exactly %v; the run must tell the user about this session precisely once for each cause", stage, actual, sessionID, sorted)
		}
	}
	for sessionID := range got {
		if _, expected := want[sessionID]; !expected {
			t.Fatalf("%s reported %v for session %s, which was expected to carry no diagnostic at all", stage, got[sessionID], sessionID)
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
	storetest.MirrorRetainedPair(t, database, filesystem, testOutputDir, metadataPath, sessionID)
	data, err := filesystem.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	before[metadataPath] = data
	previous := "last-good indexed content"
	entry := schema.SessionEntry{SessionID: sessionID, Harness: harness, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &previous}
	if harness == ingest.HarnessPi {
		// A Pi row carries typed evidence or the store refuses to decode it, so
		// the last-good seed must be a row the harness could really have written.
		extra, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraCarrier, Harness: schema.HarnessPi})
		if err != nil {
			t.Fatal(err)
		}
		entry.Extra = extra
	}
	entries := []schema.SessionEntry{entry}
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
