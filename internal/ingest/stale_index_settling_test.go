package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/stale_index_settling.yaml
var staleIndexSettlingFixtureData []byte

type staleIndexSettlingDocument struct {
	RequiredNames []string                    `yaml:"required_names"`
	Transcripts   map[string]string           `yaml:"transcripts"`
	Cases         []staleIndexSettlingFixture `yaml:"cases"`
}

type staleIndexSettlingFixture struct {
	Name              string `yaml:"name"`
	Harness           string `yaml:"harness"`
	Transcript        string `yaml:"transcript"`
	StoredIdentity    bool   `yaml:"storedIdentity"`
	OmitContentHash   bool   `yaml:"omitContentHash"`
	Refused           bool   `yaml:"refused"`
	WantCaptureStatus string `yaml:"wantCaptureStatus"`
	WantCaptureFormat string `yaml:"wantCaptureFormat"`
	WantFailureCode   string `yaml:"wantFailureCode"`
	WantEntries       int    `yaml:"wantEntries"`
}

func loadStaleIndexSettlingFixtures(t *testing.T) staleIndexSettlingDocument {
	t.Helper()
	var document staleIndexSettlingDocument
	if err := yaml.Unmarshal(staleIndexSettlingFixtureData, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid stale index settling fixture %q", fixture.Name)
		}
		if _, ok := document.Transcripts[fixture.Transcript]; !ok {
			t.Fatalf("stale index settling fixture %s names unknown transcript %q", fixture.Name, fixture.Transcript)
		}
		if fixture.Refused {
			if fixture.WantCaptureStatus != "" || fixture.WantCaptureFormat != "" || fixture.WantFailureCode != "" || fixture.WantEntries != 0 {
				t.Fatalf("fixture %s is refused and must not declare a settled outcome", fixture.Name)
			}
			names[fixture.Name] = true
			continue
		}
		if _, err := ingest.NewContentCaptureStatus(fixture.WantCaptureStatus); err != nil {
			t.Fatalf("fixture %s: %v", fixture.Name, err)
		}
		if _, err := ingest.NewContentCaptureFormat(fixture.WantCaptureFormat); err != nil {
			t.Fatalf("fixture %s: %v", fixture.Name, err)
		}
		if _, err := ingest.NewContentCaptureFailureCode(fixture.WantFailureCode); err != nil {
			t.Fatalf("fixture %s: %v", fixture.Name, err)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("stale index settling", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document
}

// TestOrdinaryHarvestSettlesStaleIndexSessions drives the ORDINARY harvest
// (no reindex mode, no native discovery) over one seeded stored row per case
// and asserts the two-run contract the fixture declares: the first harvest
// advances the producer, records the input proof, establishes a missing
// artifact identity and stores the represented entries; the second reads no
// transcript and reports nothing.
func TestOrdinaryHarvestSettlesStaleIndexSessions(t *testing.T) {
	document := loadStaleIndexSettlingFixtures(t)
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			harness := ingest.Harness(fixture.Harness)
			if !harness.IsKnown() {
				t.Fatalf("fixture names unknown harness %q", fixture.Harness)
			}
			ctx := t.Context()
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			database, err := store.Open(filepath.Join(t.TempDir(), "settle.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			id, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			transcript := []byte(document.Transcripts[fixture.Transcript])
			meta := makeMinimalMeta(t, id.String())
			meta.ModelHarness = harness
			if !fixture.OmitContentHash {
				meta.ContentHash = schema.ComputeTranscriptHash(transcript)
			}
			dir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
			path := filepath.Join(dir, id.String()+"--transcript.jsonl")
			if err := fs.WriteFile(path, transcript, 0600); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(filepath.Join(dir, id.String()+"--metadata.json"), encoded, 0600); err != nil {
				t.Fatal(err)
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			pair, err := ingest.NewManagedArtifact(encoded, transcript)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.StoredIdentity {
				results := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
				if len(results) != 1 || results[0].Err != nil || !results[0].Mirrored {
					t.Fatalf("seed stored artifact identity: %+v", results)
				}
			}
			seedStalePreviewCapture(t, ctx, database, id)

			run := func() *ingest.PipelineResult {
				t.Helper()
				cfg := makePipelineConfig(testOutputDir)
				adapters := map[ingest.Harness]ingest.AdapterFactory{
					ingest.HarnessClaudeCode: makeStubAdapter(nil, nil),
				}
				pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
					ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
				if err != nil {
					t.Fatal(err)
				}
				fs.ResetCounts()
				result, err := pipeline.Run(ctx)
				if err != nil {
					t.Fatalf("harvest: %v", err)
				}
				return result
			}

			first := run()
			if fixture.Refused {
				// The refusal contract: nothing is indexed, the stale state is
				// untouched (no producer advance, no input proof, no established
				// artifact identity), and the user is told why.
				if first.Summary.Indexed != 0 {
					t.Fatalf("a corrupt or unidentifiable session was indexed; diagnostics: %+v", first.Diagnostics)
				}
				state, err := database.ReadIndexState(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if state == nil || state.IndexerVersion != 15 || state.IndexedInputHash != nil || state.ArtifactHash != nil {
					t.Fatalf("refused session changed its stored index state: %+v", state)
				}
				if namesSession(first, id) == 0 {
					t.Fatalf("the refusal was not reported: %+v", first.Diagnostics)
				}
				return
			}
			if first.Summary.Indexed == 0 {
				t.Fatalf("the first harvest indexed nothing; diagnostics: %+v", first.Diagnostics)
			}
			state, err := database.ReadIndexState(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if state == nil {
				t.Fatal("no stored index state after the first harvest")
			}
			target := ingest.HarvesterVersionRegistry[harness]
			if state.IndexerVersion != target.IndexerVersion {
				t.Fatalf("stored producer revision = %d, want %d; a stale session must advance to the current producer or it is re-selected forever", state.IndexerVersion, target.IndexerVersion)
			}
			if state.IndexedInputHash == nil {
				t.Fatal("the first harvest recorded no input proof; without it the content pass re-attempts the session every harvest")
			}
			if state.ArtifactHash == nil || *state.ArtifactHash != pair.ArtifactHash {
				t.Fatalf("stored artifact identity = %v, want the parsed pair identity %s; the index write must establish a missing identity and never alter a stored one", state.ArtifactHash, pair.ArtifactHash)
			}
			capture, found, err := database.GetSessionContentCapture(ctx, id)
			if err != nil || !found {
				t.Fatalf("the first harvest stored no content capture: found=%t err=%v", found, err)
			}
			if string(capture.Status) != fixture.WantCaptureStatus {
				t.Fatalf("capture status = %q, want %q", capture.Status, fixture.WantCaptureStatus)
			}
			if string(capture.CaptureFormat) != fixture.WantCaptureFormat {
				t.Fatalf("capture format = %q, want %q", capture.CaptureFormat, fixture.WantCaptureFormat)
			}
			if string(capture.FailureCode) != fixture.WantFailureCode {
				t.Fatalf("capture failure code = %q, want %q", capture.FailureCode, fixture.WantFailureCode)
			}
			entries, err := database.ListEntries(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != fixture.WantEntries {
				t.Fatalf("stored entries = %d, want %d; the represented entries must be stored", len(entries), fixture.WantEntries)
			}

			second := run()
			if second.Summary.Indexed != 0 {
				t.Fatalf("the second harvest indexed %d session(s); a settled session owes no parser work", second.Summary.Indexed)
			}
			if reads := fs.ReadCount(path); reads != 0 {
				t.Fatalf("the second harvest read the retained transcript %d time(s), want 0", reads)
			}
			if got := namesSession(second, id); got != 0 {
				t.Fatalf("the second harvest reported %d diagnostic(s) naming the session, want 0: %+v", got, second.Diagnostics)
			}
		})
	}
}

// seedStalePreviewCapture stores the exact state an older build leaves for the
// sessions this regression is about: entries at producer 15, an incomplete
// legacy-preview capture, and no index input proof.
func seedStalePreviewCapture(t *testing.T, ctx context.Context, database *store.Store, id ingest.SessionID) {
	t.Helper()
	results := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{}, IndexVersion: 1, IndexedAtMs: 1700000000000,
		IndexerVersion: 15,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourcePeasantSnapshot,
			CaptureFormat: ingest.ContentCaptureFormatLegacyPreviewOnly, CapturedAtMs: 1700000000000,
			FailureCode: ingest.ContentCaptureLegacyPreviewOnly,
		},
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed stale preview capture: %+v", results)
	}
}
