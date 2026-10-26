package ingest_test

import (
	_ "embed"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/pair_repair_settling.yaml
var pairRepairSettlingFixtureData []byte

type pairRepairSettlingFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name               string `yaml:"name"`
		StoredTranscript   string `yaml:"stored_transcript"`
		NativeSource       string `yaml:"native_source"`
		Force              bool   `yaml:"force"`
		WantRepaired       bool   `yaml:"want_repaired"`
		DiagnosticMentions string `yaml:"diagnostic_mentions"`
	} `yaml:"cases"`
}

// damagedSourceTranscript is one good record followed by a record that is not
// complete JSON. Claude Code stores both lines, so a source carrying it cannot
// be rebuilt without repairing the record itself.
const damagedSourceTranscript = `{"type":"user","message":{"role":"user","content":"inspect"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":
`

const priorPreview = "the last good entries"

// TestUnrepairableSourceSettles holds what a stored session costs while its
// saved pair cannot be rebuilt.
//
// A repaired session indexes once and the next harvest reports nothing. An
// unrepairable one reports its cause once, keeps serving the stored copy, and
// reports nothing on the next harvest. Both runs are asserted: a run that
// stopped warning while still failing and one that stayed silent while still
// working are each wrong in a way the other cannot see.
func TestUnrepairableSourceSettles(t *testing.T) {
	var fixtures pairRepairSettlingFixtures
	if err := testutil.DecodeFixtureYAML(pairRepairSettlingFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate pair-repair settling fixture %q", fixture.Name)
		}
		switch fixture.StoredTranscript {
		case "valid", "malformed":
		default:
			t.Fatalf("fixture %s names unknown stored transcript %q", fixture.Name, fixture.StoredTranscript)
		}
		switch fixture.NativeSource {
		case "valid", "missing", "malformed":
		default:
			t.Fatalf("fixture %s names unknown native source %q", fixture.Name, fixture.NativeSource)
		}
		if !fixture.WantRepaired && fixture.DiagnosticMentions == "" {
			t.Fatalf("fixture %s asserts no diagnostic but names none to match", fixture.Name)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("pair-repair settling", "case", fixtures.Required, names); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := t.Context()
			memfs := testutil.NewMemFS()
			database, err := store.Open(storetest.CopyGoldenDB(t), store.WithSkipMigrations())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()

			id, err := ingest.NewSessionID(testutil.TestSessionUUID)
			if err != nil {
				t.Fatal(err)
			}
			valid := []byte(missingPairNativeTranscript)
			malformed := []byte(damagedSourceTranscript)
			pick := func(kind string) []byte {
				if kind == "malformed" {
					return malformed
				}
				return valid
			}
			nativeBytes := pick(fixture.NativeSource)
			nativePath := "/synthetic/native/" + id.String() + ".jsonl"
			if fixture.NativeSource != "missing" {
				if err := memfs.WriteFile(nativePath, nativeBytes, 0600); err != nil {
					t.Fatal(err)
				}
			}

			meta := makeMinimalMeta(t, id.String())
			meta.ModelHarness = ingest.HarnessClaudeCode
			meta.Source.Format = ingest.SourceFormatJSONL
			meta.Source.FilePath = nativePath
			meta.ContentHash = schema.ComputeTranscriptHash(valid)
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta}}); err != nil {
				t.Fatal(err)
			}
			// The row records the identity of the last usable pair, so the pair
			// on disk is what decides whether a repair is owed.
			pair, err := ingest.NewManagedArtifact(encoded, valid)
			if err != nil {
				t.Fatal(err)
			}
			mirrored := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
			if len(mirrored) != 1 || mirrored[0].Err != nil || !mirrored[0].Mirrored {
				t.Fatalf("seed stored artifact identity: %+v", mirrored)
			}
			writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
				SessionID: id, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000000,
				Result: indexformat.V1{Entries: []schema.SessionEntry{{
					SessionID: id, EntryIndex: 0,
					EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: ptrTo(priorPreview),
				}}},
			}})
			if len(writes) != 1 || writes[0].Err != nil {
				t.Fatalf("seed a last-good entry: %+v", writes)
			}
			beforeEntries, err := database.ListEntries(ctx, id)
			if err != nil || len(beforeEntries) == 0 {
				t.Fatalf("seed readable prior entries: %+v, %v", beforeEntries, err)
			}

			storedBytes := pick(fixture.StoredTranscript)
			storedPath := filepath.Join(ingest.SessionDir(testOutputDir, testutil.TestHostSlug, id.String(), ""), id.String()+"--transcript.jsonl")
			if err := memfs.WriteFile(storedPath, storedBytes, 0600); err != nil {
				t.Fatal(err)
			}

			fresh := makeMinimalMeta(t, id.String())
			fresh.ModelHarness = ingest.HarnessClaudeCode
			fresh.Source.FilePath = nativePath
			fresh.Source.Format = ingest.SourceFormatJSONL
			fresh.ContentHash = schema.ComputeTranscriptHash(nativeBytes)
			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: makeStubAdapter(nil, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Force = fixture.Force
			run := func() *ingest.PipelineResult {
				t.Helper()
				pipeline, err := newTestPipeline(memfs, testutil.DefaultGitResolver(), adapters, cfg,
					ingest.WithIndexers(ingest.NewIndexerRegistry(memfs, ingest.IndexerRegistryOptions{})),
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
			// Only this session's diagnostics count: the golden database carries
			// other rows this case says nothing about.
			reported := func(result *ingest.PipelineResult, errorType string) []ingest.DiagnosticEntry {
				t.Helper()
				var found []ingest.DiagnosticEntry
				for _, diagnostic := range result.Diagnostics {
					if diagnostic.ErrorType == errorType && strings.Contains(diagnostic.Location, id.String()) {
						found = append(found, diagnostic)
					}
				}
				return found
			}

			first := run()

			if fixture.WantRepaired {
				if first.Summary.Indexed != 1 || first.Summary.Errors != 0 {
					t.Fatalf("an available source was not rebuilt: %+v, diagnostics=%+v", first.Summary, first.Diagnostics)
				}
				if _, err := memfs.ReadFile(ingest.SessionMetadataPath(testOutputDir, testutil.TestHostSlug, id.String(), "")); err != nil {
					t.Fatalf("the rebuilt pair has no metadata sidecar: %v", err)
				}
				second := run()
				if second.Summary.Indexed != 0 || second.Summary.Errors != 0 {
					t.Fatalf("a rebuilt pair was rebuilt again: %+v", second.Summary)
				}
				if repeated := reported(second, "pair_repair_unavailable"); len(repeated) != 0 {
					t.Fatalf("a rebuilt pair still reported a repair: %+v", repeated)
				}
				return
			}

			// The stored bytes are what the case says they are: a decorative
			// column would let a case pass without exercising its shape.
			kept, err := memfs.ReadFile(storedPath)
			if err != nil || !reflect.DeepEqual(kept, storedBytes) {
				t.Fatalf("the stored transcript changed: %q, %v", kept, err)
			}

			firstReports := reported(first, "pair_repair_unavailable")
			if len(firstReports) != 1 {
				t.Fatalf("want exactly one pair_repair_unavailable for this session, got %+v", first.Diagnostics)
			}
			if !strings.Contains(firstReports[0].Message, fixture.DiagnosticMentions) {
				t.Fatalf("diagnostic %q does not name %q", firstReports[0].Message, fixture.DiagnosticMentions)
			}
			if firstReports[0].Remediation == "" {
				t.Fatal("diagnostic carries no remediation")
			}
			// An explicit retry is the remediation the diagnostic names, so it
			// must still fail when the repair did not take.
			if fixture.Force && first.Summary.Errors == 0 {
				t.Fatalf("an explicit forced repair that did not take looked successful: %+v", first.Summary)
			}
			if !fixture.Force && first.Summary.Errors != 0 {
				t.Fatalf("a readable stored copy was counted as a failed ingest: %+v", first.Summary)
			}

			second := run()
			if fixture.Force {
				// An explicit retry is never settled: it fails every time until
				// the source is repaired, and the last-good entries still hold.
				if second.Summary.Errors == 0 || len(reported(second, "pair_repair_unavailable")) != 1 {
					t.Fatalf("a forced retry fell silent while the repair still failed: %+v", second.Summary)
				}
				if kept, err := database.ListEntries(ctx, id); err != nil || !reflect.DeepEqual(beforeEntries, kept) {
					t.Fatalf("a forced retry changed the last-good entries: %+v, %v", kept, err)
				}
				return
			}
			if second.Summary.Errors != 0 {
				t.Fatalf("the settled session failed a second ingest: %+v", second.Summary)
			}
			if repeated := reported(second, "pair_repair_unavailable"); len(repeated) != 0 {
				t.Fatalf("the settled refusal was reported again: %+v", repeated)
			}
			if repeated := reported(second, "content_recovery_unavailable"); len(repeated) != 0 {
				t.Fatalf("content recovery repeated the settled refusal: %+v", repeated)
			}
			if got := indexLogErrors(t, database, id); got != 0 {
				t.Fatalf("the settled session added %d index error row(s)", got)
			}
			afterEntries, err := database.ListEntries(ctx, id)
			if err != nil || !reflect.DeepEqual(beforeEntries, afterEntries) {
				t.Fatalf("the last-good entries changed: %+v, %v", afterEntries, err)
			}
			state, err := database.ReadIndexState(ctx, id)
			if err != nil || state == nil {
				t.Fatalf("read settled index state: %+v, %v", state, err)
			}
			if state.IndexerVersion != 15 || state.IndexedInputHash != nil {
				t.Fatalf("a settled refusal changed the stored state: %+v", state)
			}
			reason, err := database.ReadSourceUnavailability(ctx, id)
			if err != nil || reason == nil {
				t.Fatalf("the settled refusal was not recorded: %v, %v", reason, err)
			}
		})
	}
}

func ptrTo(value string) *string { return &value }
