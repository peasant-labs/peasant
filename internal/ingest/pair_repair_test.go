package ingest_test

import (
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pair_repair.yaml
var pairRepairFixtureData []byte

type pairRepairDocument struct {
	RequiredNames     []string         `yaml:"required_names"`
	NativeTranscript  string           `yaml:"native_transcript"`
	DamagedTranscript string           `yaml:"damaged_transcript"`
	Cases             []pairRepairCase `yaml:"cases"`
}

type pairRepairCase struct {
	Name         string `yaml:"name"`
	PairState    string `yaml:"pairState"`
	Discovered   bool   `yaml:"discovered"`
	Reindex      bool   `yaml:"reindex"`
	WantRepaired bool   `yaml:"wantRepaired"`
}

func loadPairRepairFixtures(t *testing.T) pairRepairDocument {
	t.Helper()
	var document pairRepairDocument
	if err := yaml.Unmarshal(pairRepairFixtureData, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid pair repair fixture %q", fixture.Name)
		}
		switch fixture.PairState {
		case "missing-metadata", "missing-transcript", "damaged", "missing-metadata-source-unavailable":
		default:
			t.Fatalf("pair repair fixture %s names unknown pair state %q", fixture.Name, fixture.PairState)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("pair repair", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document
}

// TestPairRepairReingestsFromNative drives the production pipeline over one
// stored session whose saved pair is missing or damaged and asserts that the
// pair is repaired by native re-ingestion without --force, or that the
// unavailable source is reported when no repair is possible.
func TestPairRepairReingestsFromNative(t *testing.T) {
	document := loadPairRepairFixtures(t)
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := t.Context()
			fs := testutil.NewMemFS()
			database, err := store.Open(filepath.Join(t.TempDir(), "pair-repair.db"))
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
			if err := fs.WriteFile(nativePath, nativeTranscript, 0600); err != nil {
				t.Fatal(err)
			}
			meta := makeMinimalMeta(t, id.String())
			meta.ModelHarness = ingest.HarnessClaudeCode
			meta.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
			meta.Source.FilePath = nativePath
			if fixture.PairState == "missing-metadata-source-unavailable" {
				meta.Source.FilePath = "/synthetic/native/missing-" + id.String() + ".jsonl"
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
			metadataPath := filepath.Join(dir, id.String()+"--metadata.json")
			transcriptPath := filepath.Join(dir, id.String()+"--transcript.jsonl")
			switch fixture.PairState {
			case "missing-metadata", "missing-metadata-source-unavailable":
				if err := fs.WriteFile(transcriptPath, nativeTranscript, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-transcript":
				if err := fs.WriteFile(metadataPath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
			case "damaged":
				if err := fs.WriteFile(metadataPath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				if err := fs.WriteFile(transcriptPath, nativeTranscript, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			if fixture.PairState == "damaged" {
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
				if err := fs.WriteFile(transcriptPath, []byte(document.DamagedTranscript), 0600); err != nil {
					t.Fatal(err)
				}
			}
			seedStalePreviewCapture(t, ctx, database, id)

			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
			}
			if fixture.Discovered {
				session := ingest.DiscoveredSession{
					SessionID:    id,
					Harness:      ingest.HarnessClaudeCode,
					SourcePath:   ingest.ResolvedPath(nativePath),
					SourceFormat: ingest.SourceFormatJSONL,
				}
				fresh := makeMinimalMeta(t, id.String())
				fresh.ModelHarness = ingest.HarnessClaudeCode
				fresh.Source.FilePath = nativePath
				fresh.Source.Format = ingest.SourceFormatJSONL
				fresh.ContentHash = schema.ComputeTranscriptHash(nativeTranscript)
				adapters[ingest.HarnessClaudeCode] = makeStubAdapter(
					[]ingest.DiscoveredSession{session},
					map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh},
				)
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex = fixture.Reindex

			run := func() *ingest.PipelineResult {
				t.Helper()
				pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
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

			result := run()
			if !fixture.WantRepaired {
				found := false
				for _, diagnostic := range result.Diagnostics {
					if strings.Contains(diagnostic.Message, "could not be re-ingested") {
						found = true
					}
				}
				if !found {
					t.Fatalf("the unavailable source was not reported: %+v", result.Diagnostics)
				}
				state, err := database.ReadIndexState(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if state == nil || state.IndexerVersion != 15 || state.IndexedInputHash != nil {
					t.Fatalf("a refused repair changed the stored state: %+v", state)
				}
				return
			}

			if _, err := fs.Stat(metadataPath); err != nil {
				t.Fatalf("the metadata sidecar was not restored: %v", err)
			}
			restored, err := fs.ReadFile(transcriptPath)
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
