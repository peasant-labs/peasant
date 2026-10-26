package ingest_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/forced_repair_outcome.yaml
var forcedRepairOutcomeFixtureData []byte

type forcedRepairOutcomeFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name           string `yaml:"name"`
		Command        string `yaml:"command"`
		Source         string `yaml:"source"`
		Force          bool   `yaml:"force"`
		CompletedIndex bool   `yaml:"completed_index"`
		WantErrors     bool   `yaml:"want_errors"`
		WantUnindexed  bool   `yaml:"want_unindexed"`
		WantPromise    string `yaml:"want_promise"`
		WantMentions   string `yaml:"want_mentions"`
	} `yaml:"cases"`
}

// storedCopyPromises are the clauses that claim a finished index is still
// serving. A row with no completed index must not carry any of them.
var storedCopyPromises = []string{
	"keeps serving",
	"last-good",
	"stored copy keeps",
}

// TestForcedRepairOutcomeFailsEveryUnroutedShape runs the production harvest
// and reindex paths. A present-but-unusable source already fails a forced run
// through processing. A missing or unrecorded source must fail that run too,
// once, and must not be counted as indexed. A routine run reports without
// failing, except when nothing has finished indexing and the source itself
// cannot be read: that failure is the outcome, because no copy is serving.
func TestForcedRepairOutcomeFailsEveryUnroutedShape(t *testing.T) {
	var fixtures forcedRepairOutcomeFixtures
	if err := testutil.DecodeFixtureYAML(forcedRepairOutcomeFixtureData, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate forced-repair fixture %q", fixture.Name)
		}
		switch fixture.Command {
		case "harvest", "reindex":
		default:
			t.Fatalf("fixture %s names unknown command %q", fixture.Name, fixture.Command)
		}
		switch fixture.Source {
		case "missing", "malformed", "unrecorded":
		default:
			t.Fatalf("fixture %s names unknown source %q", fixture.Name, fixture.Source)
		}
		switch fixture.WantPromise {
		case "serving", "neutral":
		default:
			t.Fatalf("fixture %s names unknown promise %q", fixture.Name, fixture.WantPromise)
		}
		if fixture.WantMentions == "" {
			t.Fatalf("fixture %s asserts no diagnostic text", fixture.Name)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("forced repair outcome", "case", fixtures.Required, names); err != nil {
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
			nativePath := "/synthetic/native/" + id.String() + ".jsonl"
			if fixture.Source == "malformed" {
				if err := memfs.WriteFile(nativePath, []byte(damagedSourceTranscript), 0600); err != nil {
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
			pair, err := ingest.NewManagedArtifact(encoded, valid)
			if err != nil {
				t.Fatal(err)
			}
			mirrored := database.MirrorArtifacts(ctx, []ingest.ArtifactMirrorRequest{{Artifact: pair}})
			if len(mirrored) != 1 || mirrored[0].Err != nil || !mirrored[0].Mirrored {
				t.Fatalf("seed stored artifact identity: %+v", mirrored)
			}
			if fixture.CompletedIndex {
				writes := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
					SessionID: id, IndexVersion: 1, IndexerVersion: 15, IndexedAtMs: 1700000000000,
					Result: indexformat.V1{Entries: []schema.SessionEntry{{
						SessionID: id, EntryIndex: 0,
						EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: ptrTo("the last good entries"),
					}}},
				}})
				if len(writes) != 1 || writes[0].Err != nil {
					t.Fatalf("seed a completed index: %+v", writes)
				}
			}
			if fixture.Source == "unrecorded" {
				clearRecordedSource(t, ctx, database, id)
			}
			// A stored transcript that does not match the recorded identity makes
			// the pair need repair when the file is present. Leaving it absent
			// is the missing-pair shape both commands already select.
			if fixture.Source == "malformed" {
				storedPath := filepath.Join(ingest.SessionDir(testOutputDir, testutil.TestHostSlug, id.String(), ""), id.String()+"--transcript.jsonl")
				if err := memfs.WriteFile(storedPath, []byte(damagedSourceTranscript), 0600); err != nil {
					t.Fatal(err)
				}
			}

			fresh := makeMinimalMeta(t, id.String())
			fresh.ModelHarness = ingest.HarnessClaudeCode
			fresh.Source.FilePath = nativePath
			fresh.Source.Format = ingest.SourceFormatJSONL
			fresh.ContentHash = schema.ComputeTranscriptHash(valid)
			cfg := makePipelineConfig(testOutputDir)
			cfg.Force = fixture.Force
			cfg.Reindex = fixture.Command == "reindex"
			// The golden database carries other rows. Scope the run to this
			// session so their repair state cannot change the counts.
			cfg.AllowedSessionIDs = map[ingest.SessionID]bool{id: true}
			pipeline, err := newTestPipeline(memfs, testutil.DefaultGitResolver(), map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: makeStubAdapter(nil, map[ingest.SessionID]*ingest.UnifiedMetadata{id: fresh}),
			}, cfg,
				ingest.WithIndexers(ingest.NewIndexerRegistry(memfs, ingest.IndexerRegistryOptions{})),
				ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexLogger(database))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatalf("%s: %v", fixture.Command, err)
			}

			// Harvest reports the pair diagnostic itself. Reindex runs stored-content
			// recovery first, so the pair lane stays quiet when that lane already
			// named the same cause; the fault is still reported, just once.
			reports := diagnosticsFor(result, id, "pair_repair_unavailable")
			if len(reports) == 1 {
				text := reports[0].Message + "\n" + reports[0].Remediation
				if !strings.Contains(text, fixture.WantMentions) {
					t.Fatalf("diagnostic %q does not name %q", text, fixture.WantMentions)
				}
				switch fixture.WantPromise {
				case "serving":
					if !mentionsStoredCopyPromise(text) {
						t.Fatalf("a completed index was not told its stored copy keeps serving: %q", text)
					}
				case "neutral":
					if mentionsStoredCopyPromise(text) {
						t.Fatalf("a row with no completed index was promised a stored copy: %q", text)
					}
				}
			} else if fixture.Command == "harvest" || len(diagnosticsFor(result, id, "content_recovery_unavailable")) == 0 {
				t.Fatalf("want the pair diagnostic, or on reindex the one content diagnostic for the same cause; got %+v", result.Diagnostics)
			}
			sessionErrors := errorsFor(result, id)
			if fixture.WantErrors {
				if result.Summary.Errors == 0 || sessionErrors != 1 {
					t.Fatalf("forced or unserving repair errors = summary %d session %d, want summary > 0 and exactly one session error; summary=%+v", result.Summary.Errors, sessionErrors, result.Summary)
				}
			} else if result.Summary.Errors != 0 || sessionErrors != 0 {
				t.Fatalf("a routine repair was counted as a failed session: summary=%+v sessionErrors=%d diagnostics=%+v", result.Summary, sessionErrors, reports)
			}
			if fixture.WantUnindexed && result.Summary.Indexed != 0 {
				t.Fatalf("the unrepaired session was indexed: %+v", result.Summary)
			}
		})
	}
}

func mentionsStoredCopyPromise(text string) bool {
	for _, phrase := range storedCopyPromises {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

func diagnosticsFor(result *ingest.PipelineResult, id ingest.SessionID, errorType string) []ingest.DiagnosticEntry {
	var found []ingest.DiagnosticEntry
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.ErrorType == errorType && strings.Contains(diagnostic.Location, id.String()) {
			found = append(found, diagnostic)
		}
	}
	return found
}

func errorsFor(result *ingest.PipelineResult, id ingest.SessionID) int {
	count := 0
	for _, session := range result.Sessions {
		if session.SessionID == id && session.Error != nil {
			count++
		}
	}
	return count
}

func clearRecordedSource(t *testing.T, ctx context.Context, database *store.Store, id ingest.SessionID) {
	t.Helper()
	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, "UPDATE sessions SET source_path = '' WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(id)},
	}); err != nil {
		t.Fatal(err)
	}
	var source string
	if err := sqlitex.ExecuteTransient(conn, "SELECT source_path FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			source = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if source != "" {
		t.Fatalf("recorded source = %q, want empty", source)
	}
}
