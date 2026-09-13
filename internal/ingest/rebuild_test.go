package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"maps"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/rebuild.yaml
var rebuildFixtureData []byte

// A well-formed Strike transcript this build represents in full, so the rebuild
// can index it. Changing the delta gives a second transcript with a different
// content hash, which is what a stale saved copy looks like.
const rebuildTranscript = `{"type":"session.started","time":"2026-07-28T12:34:56Z","data":{"model":"synthetic-model","version":"0.1.0"}}
{"type":"user.message","time":"2026-07-28T12:34:57Z","data":{"turnId":"turn-1","content":"Inspect the retained fixture."}}
{"type":"turn.started","time":"2026-07-28T12:34:58Z","data":{"turnId":"turn-1"}}
{"type":"assistant.text.delta","time":"2026-07-28T12:35:00Z","data":{"turnId":"turn-1","providerRequestId":"request-1","delta":"I will inspect it now."}}
`

const rebuildChangedTranscript = `{"type":"session.started","time":"2026-07-28T12:34:56Z","data":{"model":"synthetic-model","version":"0.1.0"}}
{"type":"user.message","time":"2026-07-28T12:34:57Z","data":{"turnId":"turn-1","content":"Inspect the retained fixture."}}
{"type":"turn.started","time":"2026-07-28T12:34:58Z","data":{"turnId":"turn-1"}}
{"type":"assistant.text.delta","time":"2026-07-28T12:35:00Z","data":{"turnId":"turn-1","providerRequestId":"request-1","delta":"A different answer entirely."}}
`

// writeRebuildPair writes one Strike pair into the saved tree with the checksum
// every real metadata carries and returns its session id and the managed
// artifact it built, so a caller can seed the identical database row. It
// records no row itself: that is the rebuild's job.
func writeRebuildPair(t *testing.T, fs ingest.FileSystem, output, idStr, parentIDStr, transcript string) (ingest.SessionID, *ingest.ManagedArtifact) {
	t.Helper()
	id, err := ingest.NewSessionID(idStr)
	if err != nil {
		t.Fatal(err)
	}
	meta := makeMinimalMeta(t, idStr)
	meta.Project.Hash = testutil.TestProjectHash
	meta.ModelHarness = ingest.HarnessStrike
	meta.Source.FilePath = "/synthetic/strike-source.jsonl"
	meta.HostSlug = ingest.HostSlug(testutil.TestHostSlug)
	if parentIDStr != "" {
		parent := ingest.SessionID(parentIDStr)
		meta.ParentUUID = &parent
	}
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(transcript))
	meta.MetadataHash = schema.ComputeMetadataHash(meta)
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(encoded, []byte(transcript))
	if err != nil {
		t.Fatal(err)
	}
	dir := ingest.SessionDir(output, testutil.TestHostSlug, idStr, parentIDStr)
	if err := fs.WriteFile(filepath.Join(dir, idStr+"--transcript.jsonl"), []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(filepath.Join(dir, idStr+"--metadata.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return id, artifact
}

// TestHarvestRebuildFromFiles covers `harvest index --all`: it records rows the
// database is missing from the saved files, and as the one full hash pass over
// the tree it reports a saved copy whose bytes no longer match its row without
// overwriting the row. It also covers the empty-database hint predicate.
func TestHarvestRebuildFromFiles(t *testing.T) {
	var fixtures struct {
		Required []string `yaml:"required_names"`
		Cases    []struct {
			Name        string `yaml:"name"`
			Kind        string `yaml:"kind"`
			SeedRow     bool   `yaml:"seed_row"`
			ChangePair  bool   `yaml:"change_pair"`
			WithChild   bool   `yaml:"with_child"`
			WantRebuilt int    `yaml:"want_rebuilt"`
			WantSkipped int    `yaml:"want_skipped"`
			WantIndexed int    `yaml:"want_indexed"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(rebuildFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty or duplicate rebuild fixture %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}

	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewMemFS()

			if c.Kind == "hint" {
				// A fresh install has no saved tree, so the hint never fires.
				if ingest.RetainedTreeHoldsSessions(fs, "/output") {
					t.Fatal("hint predicate held on an empty tree")
				}
				writeRebuildPair(t, fs, "/output", "11111111-1111-4111-8111-111111111111", "", rebuildTranscript)
				if !ingest.RetainedTreeHoldsSessions(fs, "/output") {
					t.Fatal("hint predicate did not hold once the tree holds a session")
				}
				return
			}

			output := "/output"
			database, err := store.Open(filepath.Join(t.TempDir(), "rebuild.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			parentID, parentArtifact := writeRebuildPair(t, fs, output, "11111111-1111-4111-8111-111111111111", "", rebuildTranscript)
			parentHash := parentArtifact.ArtifactHash
			var childID ingest.SessionID
			if c.WithChild {
				childID, _ = writeRebuildPair(t, fs, output, "22222222-2222-4222-8222-222222222222", parentID.String(), rebuildTranscript)
			}

			if c.SeedRow {
				// The row already identifies the pair at hash H1 (the exact
				// artifact the saved pair was written from).
				results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: parentArtifact}})
				if len(results) != 1 || results[0].Err != nil {
					t.Fatalf("seed row: %+v", results)
				}
			}
			if c.ChangePair {
				// The saved copy now hashes to H2 while the row still holds H1.
				if err := fs.WriteFile(filepath.Join(ingest.SessionDir(output, testutil.TestHostSlug, parentID.String(), ""), parentID.String()+"--transcript.jsonl"), []byte(rebuildChangedTranscript), 0600); err != nil {
					t.Fatal(err)
				}
			}

			cfg := makePipelineConfig(output, func(c *ingest.PipelineConfig) {
				c.Reindex = true
				c.RebuildAll = true
				c.Force = true
			})
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessStrike: makeStubAdapter(nil, nil)}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
				ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
				ingest.WithHarvesterVersions(maps.Clone(ingest.HarvesterVersionRegistry)),
				ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("rebuild run: %v", err)
			}

			if result.Summary.RebuiltFromFiles != c.WantRebuilt {
				t.Errorf("rebuilt = %d, want %d", result.Summary.RebuiltFromFiles, c.WantRebuilt)
			}
			if len(result.Summary.RebuildSkipped) != c.WantSkipped {
				t.Errorf("skipped = %v, want %d", result.Summary.RebuildSkipped, c.WantSkipped)
			}
			if result.Summary.Indexed != c.WantIndexed {
				t.Errorf("indexed = %d, want %d", result.Summary.Indexed, c.WantIndexed)
			}

			if c.ChangePair {
				// The row and its entries are unchanged: the stale saved copy was
				// reported, never mirrored over the database.
				state, err := database.ReadIndexState(ctx, parentID)
				if err != nil || state == nil || state.ArtifactHash == nil {
					t.Fatalf("read index state after stale report: %+v, %v", state, err)
				}
				if *state.ArtifactHash != parentHash {
					t.Errorf("row hash = %q, want the unchanged H1 %q", *state.ArtifactHash, parentHash)
				}
				if len(result.Summary.RebuildSkipped) != 1 || result.Summary.RebuildSkipped[0] != parentID {
					t.Errorf("skipped = %v, want the stale session %s", result.Summary.RebuildSkipped, parentID)
				}
			}
			_ = childID
		})
	}
}
