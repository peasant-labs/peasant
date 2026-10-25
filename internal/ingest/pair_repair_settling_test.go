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
		DiagnosticMentions string `yaml:"diagnostic_mentions"`
	} `yaml:"cases"`
}

// damagedSourceTranscript is one good record followed by a record that is not
// complete JSON. Claude Code stores both lines, so a source carrying it cannot
// be rebuilt without repairing the record itself.
const damagedSourceTranscript = `{"type":"user","message":{"role":"user","content":"inspect"},"uuid":"u1"}
{"type":"assistant","message":{"role":"assistant","content":
`

// TestUnrepairableSourceSettles proves a session whose stored pair cannot be
// rebuilt costs one actionable diagnostic and then nothing.
//
// The saved copy is still readable: its last-good entries keep serving, which
// is why the failure must not be counted as a failed ingest again and again.
// The second harvest asserts both halves — no new diagnostic and no new
// parse or index work — because a run that stopped warning while still
// re-reading and one that stopped working while still warning are each wrong
// in a way the other assertion cannot see.
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
			nativeBytes := valid
			if fixture.NativeSource == "malformed" {
				nativeBytes = []byte(damagedSourceTranscript)
			}
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
			// The row records the identity of the last usable pair. The pair on
			// disk then fails to match it, which is what selects the repair.
			pair, err := ingest.NewManagedArtifact(encoded, valid)
			if err != nil {
				t.Fatal(err)
			}
			mirrored := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
			if len(mirrored) != 1 || mirrored[0].Err != nil || !mirrored[0].Mirrored {
				t.Fatalf("seed stored artifact identity: %+v", mirrored)
			}
			var priorPreview = "the last good entries"
			writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
				SessionID: id, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000000,
				Result: indexformat.V1{Entries: []schema.SessionEntry{{
					SessionID: id, EntryIndex: 0,
					EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &priorPreview,
				}}},
			}})
			if len(writes) != 1 || writes[0].Err != nil {
				t.Fatalf("seed a last-good entry: %+v", writes)
			}
			beforeEntries, err := database.ListEntries(ctx, id)
			if err != nil || len(beforeEntries) == 0 {
				t.Fatalf("seed readable prior entries: %+v, %v", beforeEntries, err)
			}

			storedBytes := valid
			if fixture.StoredTranscript == "malformed" {
				storedBytes = []byte(damagedSourceTranscript)
			}
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

			first := run()
			if first.Summary.Errors != 0 {
				t.Fatalf("a readable stored copy was counted as a failed ingest: %+v", first.Summary)
			}
			var reported []ingest.DiagnosticEntry
			for _, diagnostic := range first.Diagnostics {
				if diagnostic.ErrorType == "pair_repair_unavailable" {
					reported = append(reported, diagnostic)
				}
			}
			if len(reported) != 1 {
				t.Fatalf("want exactly one pair_repair_unavailable diagnostic, got %+v", first.Diagnostics)
			}
			if !strings.Contains(reported[0].Message, fixture.DiagnosticMentions) {
				t.Fatalf("diagnostic %q does not name %q", reported[0].Message, fixture.DiagnosticMentions)
			}
			if reported[0].Remediation == "" {
				t.Fatal("diagnostic carries no remediation")
			}

			second := run()
			if second.Summary.Errors != 0 {
				t.Fatalf("the settled session failed a second ingest: %+v", second.Summary)
			}
			for _, diagnostic := range second.Diagnostics {
				if diagnostic.ErrorType == "pair_repair_unavailable" || diagnostic.ErrorType == "content_recovery_unavailable" {
					t.Fatalf("the settled refusal was reported again: %+v", second.Diagnostics)
				}
			}
			if got := indexLogErrors(t, database, id); got != 0 {
				t.Fatalf("the settled session added %d index error row(s)", got)
			}
			if _, err := memfs.ReadFile(storedPath); err != nil {
				t.Fatalf("the readable stored copy was removed: %v", err)
			}
			afterEntries, err := database.ListEntries(ctx, id)
			if err != nil || !reflect.DeepEqual(beforeEntries, afterEntries) {
				t.Fatalf("the last-good entries changed: %+v, %v", afterEntries, err)
			}
			state, err := database.ReadIndexState(ctx, id)
			if err != nil || state == nil {
				t.Fatalf("read settled index state: %+v, %v", state, err)
			}
			reason, err := database.ReadSourceUnavailability(ctx, id)
			if err != nil || reason == nil {
				t.Fatalf("the settled refusal was not recorded: %v, %v", reason, err)
			}
		})
	}
}
