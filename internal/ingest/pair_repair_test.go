package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pair_repair.yaml
var pairRepairFixtureData []byte

const pairRepairNoCause ingest.SourceUnavailabilityReason = ""

type pairRepairState string

const (
	pairRepairMissingMetadata         pairRepairState = "missing-metadata"
	pairRepairMissingTranscript       pairRepairState = "missing-transcript"
	pairRepairDamaged                 pairRepairState = "damaged"
	pairRepairMissingMetadataNoSource pairRepairState = "missing-metadata-source-unavailable"
)

func (v pairRepairState) Validate() error {
	return validateFixtureChoice("pairState", v, pairRepairMissingMetadata, pairRepairMissingTranscript, pairRepairDamaged, pairRepairMissingMetadataNoSource)
}

type pairRepairLocatorFault string

const (
	pairRepairNoLocatorFault  pairRepairLocatorFault = ""
	pairRepairSourceInfoFault pairRepairLocatorFault = "source-info"
	pairRepairLocationFault   pairRepairLocatorFault = "location"
)

func (v pairRepairLocatorFault) Validate() error {
	return validateFixtureChoice("locatorFault", v, pairRepairNoLocatorFault, pairRepairSourceInfoFault, pairRepairLocationFault)
}

type pairRepairDocument struct {
	RequiredNames     []string         `yaml:"required_names"`
	NativeTranscript  string           `yaml:"native_transcript"`
	DamagedTranscript string           `yaml:"damaged_transcript"`
	PriorPreview      string           `yaml:"prior_preview"`
	Cases             []pairRepairCase `yaml:"cases"`
}

type pairRepairCase struct {
	Name                string                 `yaml:"name"`
	PairState           pairRepairState        `yaml:"pairState"`
	Discovered          bool                   `yaml:"discovered"`
	Reindex             bool                   `yaml:"reindex"`
	Nested              bool                   `yaml:"nested"`
	TranscriptReadFault bool                   `yaml:"transcriptReadFault"`
	WantRepaired        bool                   `yaml:"wantRepaired"`
	WantSourceReport    bool                   `yaml:"wantSourceReport"`
	LocatorFault        pairRepairLocatorFault `yaml:"locatorFault"`
	// CapturedSeed stores the row with a source-proven publication capture,
	// the state of every row an ordinary harvest has already captured. Without
	// it the seed is a pre-capture row, which an ordinary harvest re-reads from
	// native input once in order to capture it.
	CapturedSeed bool `yaml:"capturedSeed"`
	// NativeSource names the bytes the native source holds: empty for the valid
	// fixture transcript, "malformed" for bytes that are present but cannot be
	// used. The second shape is how an acquisition failure reaches the pair
	// lane, which is the only place the re-ingest template is printed.
	NativeSource fixtureNativeSource `yaml:"nativeSource"`
	// WantCause is the source-unavailability reason the refused repair must
	// record. It is required with WantSourceReport, because the two causes are
	// different user situations and a wrong one is a wrong fix.
	WantCause ingest.SourceUnavailabilityReason `yaml:"wantCause"`
	// Each lane has its own message and remediation. A case with no content
	// fragment expects only the pair-repair lane to report.
	WantPairMessageContains    string `yaml:"wantPairMessageContains"`
	WantContentMessageContains string `yaml:"wantContentMessageContains"`
}

func loadPairRepairFixtures(t *testing.T) pairRepairDocument {
	t.Helper()
	var document pairRepairDocument
	if err := testutil.DecodeFixtureYAML(pairRepairFixtureData, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid pair repair fixture %q", fixture.Name)
		}
		if err := fixture.PairState.Validate(); err != nil {
			t.Fatalf("pair repair fixture %s: %v", fixture.Name, err)
		}
		if err := fixture.LocatorFault.Validate(); err != nil {
			t.Fatalf("pair repair fixture %s: %v", fixture.Name, err)
		}
		if err := fixture.NativeSource.Validate(); err != nil {
			t.Fatalf("pair repair fixture %s: %v", fixture.Name, err)
		}
		if fixture.WantSourceReport && fixture.WantCause == pairRepairNoCause {
			t.Fatalf("pair repair fixture %s reports an unavailable source without naming its cause", fixture.Name)
		}
		if fixture.WantSourceReport && fixture.WantPairMessageContains == "" {
			t.Fatalf("pair repair fixture %s reports an unavailable source without specifying its pair message", fixture.Name)
		}
		if fixture.WantCause != pairRepairNoCause {
			if _, err := ingest.NewSourceUnavailabilityReason(string(fixture.WantCause)); err != nil {
				t.Fatalf("pair repair fixture %s: %v", fixture.Name, err)
			}
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("pair repair", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document
}

// pairRepairStoredExtractorAdapter gives the synthetic Claude adapter the
// stored-transcript extractor the production Claude adapter implements, so the
// stored-metadata lane is the one under test rather than a native-only stub.
type pairRepairStoredExtractorAdapter struct {
	ingest.SourceAdapter
}

func (a *pairRepairStoredExtractorAdapter) ExtractMetadataFromTranscript(_ context.Context, data []byte, original *ingest.UnifiedMetadata) (*ingest.UnifiedMetadata, error) {
	copy := *original
	copy.ContentHash = schema.ComputeTranscriptHash(data)
	return &copy, nil
}

// TestPairRepairReingestsFromNative drives the production pipeline over one
// stored session whose saved pair is missing or damaged and asserts that the
// pair is repaired by native re-ingestion without --force, or that the
// unavailable source is reported when no repair is possible.
func TestPairRepairReingestsFromNative(t *testing.T) {
	t.Parallel()
	document := loadPairRepairFixtures(t)
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := t.Context()
			memfs := testutil.NewMemFS()
			database, err := store.Open(storetest.CopyGoldenDB(t))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			id, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			nativeTranscript := []byte(document.NativeTranscript)
			nativePath := "/synthetic/native/" + id.String() + ".jsonl"
			nativeBytes := nativeTranscript
			if fixture.NativeSource == fixtureNativeSourceMalformed {
				// Present but unusable: the acquisition fails on a record, which
				// is a different cause from a source that is not there.
				nativeBytes = []byte(damagedSourceTranscript)
			}
			if err := memfs.WriteFile(nativePath, nativeBytes, 0600); err != nil {
				t.Fatal(err)
			}
			meta := makeMinimalMeta(t, id.String())
			meta.ModelHarness = ingest.HarnessClaudeCode
			meta.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
			meta.Source.FilePath = nativePath
			if fixture.PairState == pairRepairMissingMetadataNoSource {
				meta.Source.FilePath = "/synthetic/native/missing-" + id.String() + ".jsonl"
			}
			var parentID *ingest.SessionID
			if fixture.Nested {
				parent, err := ingest.NewSessionID(testutil.TestSessionUUID2)
				if err != nil {
					t.Fatal(err)
				}
				parentID = &parent
				meta.ParentUUID = parentID
			}
			if fixture.CapturedSeed {
				// The pair must carry the snapshot the capture records, so the
				// integrity digest is sealed before the pair is written.
				meta.SchemaVersion = ingest.CurrentSchemaVersion
				meta.MetadataHash = schema.ComputeMetadataHash(meta)
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
			if parentID != nil {
				dir = filepath.Join(testOutputDir, testutil.TestHostSlug, string(*parentID), "subagents", id.String())
			}
			metadataPath := filepath.Join(dir, id.String()+"--metadata.json")
			transcriptPath := filepath.Join(dir, id.String()+"--transcript.jsonl")
			switch fixture.PairState {
			case pairRepairMissingMetadata, pairRepairMissingMetadataNoSource:
				if err := memfs.WriteFile(transcriptPath, nativeTranscript, 0600); err != nil {
					t.Fatal(err)
				}
			case pairRepairMissingTranscript:
				if err := memfs.WriteFile(metadataPath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
			case pairRepairDamaged:
				if err := memfs.WriteFile(metadataPath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				if err := memfs.WriteFile(transcriptPath, nativeTranscript, 0600); err != nil {
					t.Fatal(err)
				}
			}
			storeEntries := []ingest.StoreEntry{}
			if parentID != nil {
				parentMeta := makeMinimalMeta(t, string(*parentID))
				parentMeta.ModelHarness = ingest.HarnessClaudeCode
				storeEntries = append(storeEntries, ingest.StoreEntry{Metadata: parentMeta})
			}
			entry := ingest.StoreEntry{Metadata: meta}
			if fixture.CapturedSeed {
				entry.PublicationCapture = true
				entry.CWDProvenance = ingest.CWDSourceAbsent
			}
			storeEntries = append(storeEntries, entry)
			if err := database.InsertSessions(ctx, storeEntries); err != nil {
				t.Fatal(err)
			}
			if fixture.PairState == pairRepairDamaged {
				// Record the identity of the whole pair, then replace the
				// transcript: the row now names bytes the pair no longer holds.
				artifact, err := ingest.NewManagedArtifact(encoded, nativeTranscript)
				if err != nil {
					t.Fatal(err)
				}
				results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: artifact}})
				if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
					t.Fatalf("seed pair identity: %+v", results)
				}
				if err := memfs.WriteFile(transcriptPath, []byte(document.DamagedTranscript), 0600); err != nil {
					t.Fatal(err)
				}
			}
			seedStalePreviewCapture(t, ctx, database, id)
			if fixture.WantSourceReport || fixture.LocatorFault != pairRepairNoLocatorFault {
				writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
					SessionID: id, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000000,
					Result: indexformat.V1{Entries: []schema.SessionEntry{{
						SessionID: id, EntryIndex: 0,
						EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &document.PriorPreview,
					}}},
				}})
				if len(writes) != 1 || writes[0].Err != nil {
					t.Fatalf("seed readable prior entries: %+v", writes)
				}
			}
			priorEntries, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}

			var fs ingest.FileSystem = memfs
			if fixture.TranscriptReadFault {
				fs = &pairRepairFaultFS{MemFS: memfs, failReadPath: transcriptPath}
			}

			// Discovery decides what the run is offered; extraction always has
			// the metadata a native re-ingest needs, so a reconstructed repair
			// is exercised independently of discovery.
			var discoveredSessions []ingest.DiscoveredSession
			if fixture.Discovered {
				discoveredSessions = []ingest.DiscoveredSession{{
					SessionID:    id,
					Harness:      ingest.HarnessClaudeCode,
					SourcePath:   ingest.ResolvedPath(nativePath),
					SourceFormat: ingest.SourceFormatJSONL,
				}}
			}
			fresh := makeMinimalMeta(t, id.String())
			fresh.ModelHarness = ingest.HarnessClaudeCode
			fresh.Source.FilePath = nativePath
			fresh.Source.Format = ingest.SourceFormatJSONL
			fresh.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
			fresh.ParentUUID = parentID
			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: func(fs ingest.FileSystem, git ingest.GitResolver, salt salt.Salt) ingest.SourceAdapter {
					stub := makeStubAdapter(discoveredSessions, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh})(fs, git, salt)
					return &pairRepairStoredExtractorAdapter{SourceAdapter: stub}
				},
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex = fixture.Reindex
			locatorStore := &pairRepairLocatorFaultStore{Store: database, sessionID: id, fault: fixture.LocatorFault}

			run := func() *ingest.PipelineResult {
				t.Helper()
				pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
					ingest.WithStore(database), ingest.WithMetricsStore(locatorStore), ingest.WithIndexLogger(database))
				if err != nil {
					t.Fatal(err)
				}
				result, err := pipeline.Run(ctx)
				if err != nil {
					t.Fatalf("harvest: %v", err)
				}
				return result
			}

			result := run()
			if fixture.LocatorFault != pairRepairNoLocatorFault {
				reason, err := database.ReadSourceUnavailability(ctx, id)
				if err != nil || reason != nil {
					t.Fatalf("locator failure recorded source unavailability: %v %v", reason, err)
				}
				if _, err := memfs.Stat(nativePath); err != nil {
					t.Fatalf("native source must remain present: %v", err)
				}
				kept, err := database.ListEntries(ctx, id)
				if err != nil || !reflect.DeepEqual(kept, priorEntries) {
					t.Fatalf("locator failure changed prior entries: %v %v", kept, err)
				}
				found := false
				for _, diagnostic := range result.Diagnostics {
					if diagnostic.ErrorType == "pair_repair_unavailable" {
						t.Fatalf("locator failure misreported source availability: %+v", diagnostic)
					}
					if strings.Contains(diagnostic.Message, "synthetic locator lookup failure") {
						found = true
					}
				}
				if !found {
					t.Fatalf("locator failure was not reported: %+v", result.Diagnostics)
				}
				if result.Summary.Indexed != 0 {
					t.Fatalf("locator failure indexed %d sessions", result.Summary.Indexed)
				}
				locatorStore.fault = pairRepairNoLocatorFault
				result = run()
			}
			if !fixture.WantRepaired {
				if result.Summary.Indexed != 0 {
					t.Fatalf("a refused repair indexed %d session(s)", result.Summary.Indexed)
				}
				if fixture.WantSourceReport {
					wantCause := fixture.WantCause
					reason, err := database.ReadSourceUnavailabilityForLane(ctx, id, ingest.SourceLanePairRepair)
					if err != nil || reason == nil || *reason != wantCause {
						t.Fatalf("pair-repair cause was not persisted: %v %v, want %s", reason, err, wantCause)
					}
					second := run()
					kept, err := database.ListEntries(ctx, id)
					if err != nil || !reflect.DeepEqual(kept, priorEntries) {
						t.Fatalf("unavailable source changed prior readable entries: %v %v", kept, err)
					}
					for _, diagnostic := range second.Diagnostics {
						if diagnostic.ErrorType == "pair_repair_unavailable" || diagnostic.ErrorType == "content_recovery_unavailable" {
							t.Fatalf("unchanged unavailable source warned again: %+v", diagnostic)
						}
					}
					pairReports := typedSessionDiagnostics(result, id, ingest.OutcomeReasonPairRepairUnavailable.Code())
					if len(pairReports) != 1 {
						t.Fatalf("pair repair reported %d times, want one: %+v", len(pairReports), result.Diagnostics)
					}
					assertPairRepairDiagnostic(t, id, pairReports[0], fixture.WantPairMessageContains)
					contentReports := typedSessionDiagnostics(result, id, ingest.OutcomeReasonContentRecoveryUnavailable.Code())
					wantContent := 0
					if fixture.WantContentMessageContains != "" {
						wantContent = 1
					}
					if len(contentReports) != wantContent {
						t.Fatalf("stored-content recovery reported %d times, want %d: %+v", len(contentReports), wantContent, result.Diagnostics)
					}
					if wantContent != 0 {
						assertPairRepairDiagnostic(t, id, contentReports[0], fixture.WantContentMessageContains)
						contentCause, err := database.ReadSourceUnavailabilityForLane(ctx, id, ingest.SourceLaneStoredContent)
						if err != nil || contentCause == nil || *contentCause != wantCause {
							t.Fatalf("stored-content cause = %v, %v; want %s", contentCause, err, wantCause)
						}
					}
				} else if len(result.Diagnostics) == 0 {
					t.Fatalf("a refused repair reported nothing")
				}
				state, err := database.ReadIndexState(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if state == nil || state.IndexerVersion != 15 || state.IndexedInputHash != nil {
					t.Fatalf("a refused repair changed the stored state: %+v", state)
				}
				// The pair must survive untouched. The fault case seeds a
				// damaged transcript that must still hold the damaged bytes.
				if fixture.PairState == pairRepairDamaged {
					kept, err := memfs.ReadFile(transcriptPath)
					if err != nil {
						t.Fatal(err)
					}
					if string(kept) != document.DamagedTranscript {
						t.Fatalf("a refused repair overwrote the unreadable transcript: %q", kept)
					}
				}
				if fixture.WantSourceReport {
					if err := memfs.WriteFile(meta.Source.FilePath, nativeTranscript, 0600); err != nil {
						t.Fatal(err)
					}
					recovered := run()
					if recovered.Summary.Indexed == 0 {
						t.Fatal("restored native source was not repaired")
					}
					reason, err := database.ReadSourceUnavailability(ctx, id)
					if err != nil || reason != nil {
						t.Fatalf("successful repair kept unavailable state: %v %v", reason, err)
					}
				}
				return
			}

			if _, err := memfs.Stat(metadataPath); err != nil {
				t.Fatalf("the metadata sidecar was not restored: %v", err)
			}
			if parentID != nil {
				topLevel := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String(), id.String()+"--metadata.json")
				if _, err := memfs.Stat(topLevel); err == nil {
					t.Fatal("a nested repair wrote the pair in the top-level layout")
				}
			}
			restored, err := memfs.ReadFile(transcriptPath)
			if err != nil {
				t.Fatalf("the transcript was not restored: %v", err)
			}
			if string(restored) != document.NativeTranscript {
				t.Fatalf("the restored transcript does not match the native source: %q", restored)
			}
			state, err := database.ReadIndexState(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			target := ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode]
			if state == nil || state.IndexerVersion != target.IndexerVersion {
				t.Fatalf("stored producer revision = %+v, want %d; the repair must index the re-ingested pair", state, target.IndexerVersion)
			}
			if state.IndexedInputHash == nil || state.ArtifactHash == nil {
				t.Fatalf("the repair recorded no input proof or artifact identity: %+v", state)
			}
			// The recorded identity must name the restored pair, not the
			// damaged bytes or an earlier copy.
			restoredMeta, err := memfs.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			restoredArtifact, err := ingest.NewManagedArtifact(restoredMeta, restored)
			if err != nil {
				t.Fatalf("the restored pair is not a valid artifact: %v", err)
			}
			if *state.ArtifactHash != restoredArtifact.ArtifactHash {
				t.Fatalf("stored artifact identity = %s, want the restored pair identity %s", *state.ArtifactHash, restoredArtifact.ArtifactHash)
			}
			entries, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) == 0 {
				t.Fatal("the repaired session stored no entries")
			}

			second := run()
			if second.Summary.Indexed != 0 {
				t.Fatalf("the second harvest re-indexed %d session(s); a repaired session settles", second.Summary.Indexed)
			}
		})
	}
}

func assertPairRepairDiagnostic(t *testing.T, id ingest.SessionID, diagnostic ingest.DiagnosticEntry, wantMessage string) {
	t.Helper()
	if diagnostic.Remediation == "" || !strings.Contains(diagnostic.Message, id.String()) {
		t.Fatalf("diagnostic must name session %s and carry a remediation: %+v", id, diagnostic)
	}
	if !strings.Contains(diagnostic.Message, wantMessage) {
		t.Fatalf("diagnostic does not say %q: %q", wantMessage, diagnostic.Message)
	}
}

// pairRepairLocatorFaultStore leaves the real stored source intact while a
// locator query fails. Clearing the fault models database access recovering.
type pairRepairLocatorFaultStore struct {
	*store.Store
	sessionID ingest.SessionID
	fault     pairRepairLocatorFault
}

var _ ingest.MetricsStore = (*pairRepairLocatorFaultStore)(nil)

func (s *pairRepairLocatorFaultStore) LookupSourceInfo(ctx context.Context, sid ingest.SessionID) (string, ingest.SourceFormat, string, error) {
	if sid == s.sessionID && s.fault == pairRepairSourceInfoFault {
		return "", "", "", fmt.Errorf("synthetic locator lookup failure: source info")
	}
	return s.Store.LookupSourceInfo(ctx, sid)
}

func (s *pairRepairLocatorFaultStore) LookupSessionLocation(ctx context.Context, sid ingest.SessionID) (string, string, error) {
	if sid == s.sessionID && s.fault == pairRepairLocationFault {
		return "", "", fmt.Errorf("synthetic locator lookup failure: location")
	}
	return s.Store.LookupSessionLocation(ctx, sid)
}

// pairRepairFaultFS fails reads of one path, modelling a transient I/O fault on
// an otherwise present pair half.
type pairRepairFaultFS struct {
	*testutil.MemFS
	failReadPath string
}

func (faults *pairRepairFaultFS) ReadFile(path string) ([]byte, error) {
	if path == faults.failReadPath {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	}
	return faults.MemFS.ReadFile(path)
}
