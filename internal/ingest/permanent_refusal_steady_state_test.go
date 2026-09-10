package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/permanent_refusal_steady_state.yaml
var permanentRefusalSteadyStateFixtureData []byte

type permanentRefusalFixtures struct {
	Required   []string `yaml:"required_names"`
	Transcript string   `yaml:"transcript"`
	Record     string   `yaml:"unrepresented_record"`
	Cases      []struct {
		Name         string `yaml:"name"`
		BumpIndexer  bool   `yaml:"bump_indexer"`
		AppendRecord bool   `yaml:"append_record"`
		Legacy       bool   `yaml:"legacy_preview_capture"`
		Omitted      bool   `yaml:"omitted_record"`
		Diagnostics  int    `yaml:"second_harvest_diagnostics"`
		Reads        int    `yaml:"second_harvest_retained_reads"`
	} `yaml:"cases"`
}

// TestPermanentRefusalReachesASteadyState holds what a refusal this build cannot
// lift costs on the harvest after the one that recorded it.
//
// A Strike transcript carrying one well-formed event kind this build does not
// represent is captured incomplete, with the strict refusal recorded against
// the producer that refused it. Nothing about that can change until a newer
// indexer ships or the bytes move, so the next harvest must do no parser work
// and owe the user no new warning. Both are asserted: a run that stopped
// warning while still re-reading, and one that stopped working while still
// warning, are each wrong in a way the other assertion cannot see.
func TestPermanentRefusalReachesASteadyState(t *testing.T) {
	var fixtures permanentRefusalFixtures
	if err := yaml.Unmarshal(permanentRefusalSteadyStateFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate permanent refusal fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing permanent refusal fixture %s", name)
		}
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			fs := testutil.NewCountingFS(testutil.NewMemFS())
			database, err := store.Open(filepath.Join(t.TempDir(), "steady.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			id, err := ingest.NewSessionID("11111111-1111-4111-8111-111111111111")
			if err != nil {
				t.Fatal(err)
			}
			meta := makeMinimalMeta(t, id.String())
			meta.Project.Hash = testutil.TestProjectHash
			meta.ModelHarness = ingest.HarnessStrike
			if fixture.Omitted {
				// The state ingest leaves when it removes a record longer than the
				// scanner's line limit: the artifact is short that record and the
				// metadata says so. captureContentOmitted reads exactly this.
				meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, schema.DiagnosticEntry{
					ErrorType: "record_too_large", Location: "line 5",
					Message:     "a source record exceeded the scanner line limit and was removed before redaction",
					Remediation: "rerun peasant ingest from a source that keeps records this long",
				})
			}
			dir := filepath.Join(testOutputDir, testutil.TestHostSlug, id.String())
			path := filepath.Join(dir, id.String()+"--transcript.jsonl")
			meta.Source.FilePath = "/synthetic/strike-source.jsonl"
			// A legacy preview-only capture is a transcript this build CAN
			// certify, stored incomplete by an older build that never tried. It
			// carries no failure code, which is exactly what separates it from a
			// refusal: it must still be certified, once.
			transcript := fixtures.Transcript
			if fixture.Omitted {
				// The transcript itself is well formed: what makes it incomplete is
				// the record that is NOT in it.
			} else if !fixture.Legacy {
				transcript += fixtures.Record + "\n"
			}
			if err := fs.WriteFile(path, []byte(transcript), 0600); err != nil {
				t.Fatal(err)
			}
			metadata, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile(filepath.Join(dir, id.String()+"--metadata.json"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}

			versions := maps.Clone(ingest.HarvesterVersionRegistry)
			run := func() *ingest.PipelineResult {
				t.Helper()
				cfg := makePipelineConfig(testOutputDir)
				cfg.Reindex = true
				adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessStrike: makeStubAdapter(nil, nil)}
				pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
					ingest.WithHarvesterVersions(versions),
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

			if fixture.Legacy {
				seedPreviewOnlyCapture(t, ctx, database, id)
			}
			first := run()
			capture, found, err := database.GetSessionContentCapture(ctx, id)
			if err != nil || !found {
				t.Fatalf("the first harvest stored no capture: found=%t %v", found, err)
			}
			if fixture.Legacy {
				// The whole reason the failure code exists: an incomplete capture
				// with no recorded refusal has never been tried by a build that
				// could certify it, so it is pending work, not a steady state.
				if capture.Status != ingest.ContentCaptureComplete {
					t.Fatalf("a capture no build ever refused was left uncertified: %+v; diagnostics=%+v", capture, first.Diagnostics)
				}
				return
			}
			if namesSession(first, id) == 0 {
				t.Fatalf("the first harvest must tell the user this transcript could not be certified: %+v", first.Diagnostics)
			}
			if capture.Status == ingest.ContentCaptureComplete {
				t.Fatalf("a refused transcript was certified complete: %+v", capture)
			}
			wantCode := ingest.ContentCaptureStrictRefused
			if fixture.Omitted {
				wantCode = ingest.ContentCaptureSourceRecordsOmitted
			}
			if capture.FailureCode != wantCode {
				t.Fatalf("the refusal was stored as %q, want %q; nothing later can tell a refusal apart from a capture that was never certified unless its cause is recorded", capture.FailureCode, wantCode)
			}

			// What changed between the harvests, if anything.
			if fixture.BumpIndexer {
				bumped := versions[ingest.HarnessStrike]
				bumped.IndexerVersion++
				versions[ingest.HarnessStrike] = bumped
			}
			if fixture.AppendRecord {
				if err := fs.WriteFile(path, []byte(transcript+fixtures.Record+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}

			second := run()
			reads := fs.ReadCount(path)
			if got := namesSession(second, id); got != fixture.Diagnostics {
				t.Fatalf("the second harvest reported %d diagnostics naming the session, want %d; a permanent condition must not be reported again, and new evidence must not be reported as if nothing happened: %+v", got, fixture.Diagnostics, second.Diagnostics)
			}
			if reads != fixture.Reads {
				t.Fatalf("the second harvest read the retained transcript %d times, want %d; the declared count is the parser work the run owes, and each reader that skips or repeats moves it", reads, fixture.Reads)
			}
		})
	}
}

// seedPreviewOnlyCapture stores the state an older build left behind: entries
// it could read, marked incomplete, with NO failure code because nothing
// refused them. A build that can certify them owes exactly one attempt.
func seedPreviewOnlyCapture(t *testing.T, ctx context.Context, database *store.Store, id ingest.SessionID) {
	t.Helper()
	results := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{}, IndexVersion: 1, IndexedAtMs: 1700000001000,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessStrike].IndexerVersion,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourcePeasantSnapshot,
			CaptureFormat: ingest.ContentCaptureFormatPreviewOnly, CapturedAtMs: 1700000001000,
		},
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("seed preview-only capture: %+v", results)
	}
}

// namesSession counts the diagnostics a run reported about one session.
func namesSession(result *ingest.PipelineResult, id ingest.SessionID) int {
	count := 0
	for _, diagnostic := range result.Diagnostics {
		if strings.Contains(diagnostic.Location, id.String()) || strings.Contains(diagnostic.Message, id.String()) {
			count++
		}
	}
	return count
}
