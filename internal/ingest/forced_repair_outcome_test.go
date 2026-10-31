package ingest_test

import (
	"context"
	_ "embed"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		Name            string `yaml:"name"`
		Command         string `yaml:"command"`
		Source          string `yaml:"source"`
		StoredPair      string `yaml:"stored_pair"`
		Since           string `yaml:"since"`
		Force           bool   `yaml:"force"`
		CompletedIndex  bool   `yaml:"completed_index"`
		WantErrors      bool   `yaml:"want_errors"`
		WantIndexed     int    `yaml:"want_indexed"`
		WantUpdated     int    `yaml:"want_updated"`
		WantDiagnostics int    `yaml:"want_diagnostics"`
		WantPromise     string `yaml:"want_promise"`
		WantMentions    string `yaml:"want_mentions"`
		// WantContentPromise pins the wording of the stored-content lane's own
		// report, which is a different message from the pair lane's. It is
		// asserted on the content-recovery diagnostic itself, so reverting that
		// lane to an unconditional promise fails even though the pair lane's
		// text is unchanged.
		WantContentPromise string `yaml:"want_content_promise"`
	} `yaml:"cases"`
}

// storedCopyClaims are the clauses that tell the reader a stored copy is still
// there to read. Every production message that promises one uses exactly one of
// these, so a row with no completed index must carry none of them. Each entry is
// the shortest fragment that still identifies its own sentence, and no entry is
// a substring of another, so every entry can match independently: a duplicate
// would look like extra coverage while testing nothing new. The list is a phrase
// scan on purpose: the honest messages also say "no usable saved copy" and
// "nothing is served", and a word scan on "copy" or "served" would flag those as
// promises.
var storedCopyClaims = []string{
	// "…; the stored copy keeps serving meanwhile."
	"keeps serving",
	// "; the stored copy keeps its last-good entries"
	"keeps its last-good",
	// "Its stored transcript is still served from the database."
	"still served",
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
		switch fixture.StoredPair {
		case "", "damaged-identity":
		default:
			t.Fatalf("fixture %s names unknown stored pair %q", fixture.Name, fixture.StoredPair)
		}
		switch fixture.Since {
		case "", "in-window", "out-of-window":
		default:
			t.Fatalf("fixture %s names unknown since window %q", fixture.Name, fixture.Since)
		}
		if fixture.WantDiagnostics == 0 {
			if fixture.WantMentions != "" {
				t.Fatalf("fixture %s asserts text on a run that owes no diagnostic", fixture.Name)
			}
		} else {
			switch fixture.WantPromise {
			case "serving", "neutral":
			default:
				t.Fatalf("fixture %s names unknown promise %q", fixture.Name, fixture.WantPromise)
			}
			if fixture.WantMentions == "" {
				t.Fatalf("fixture %s asserts no diagnostic text", fixture.Name)
			}
		}
		switch fixture.WantContentPromise {
		case "", "serving", "neutral":
		default:
			t.Fatalf("fixture %s names unknown content-recovery promise %q", fixture.Name, fixture.WantContentPromise)
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
			artifact := seedStoredPair(t, database, meta, valid)
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
			storedDir := ingest.SessionDir(testOutputDir, testutil.TestHostSlug, id.String(), "")
			if fixture.Source == "malformed" {
				if err := memfs.WriteFile(filepath.Join(storedDir, id.String()+"--transcript.jsonl"), []byte(damagedSourceTranscript), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if fixture.StoredPair == "damaged-identity" {
				// Both halves are readable, and the transcript is not the bytes the
				// row recorded. The pair still needs repair, and the metadata that
				// survived is what gives the session a start the run's age filter can
				// compare against.
				if err := memfs.WriteFile(filepath.Join(storedDir, id.String()+"--metadata.json"), artifact.MetadataJSON, 0600); err != nil {
					t.Fatal(err)
				}
				if err := memfs.WriteFile(filepath.Join(storedDir, id.String()+"--transcript.jsonl"), []byte(distinctStoredTranscript), 0600); err != nil {
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
			switch fixture.Since {
			case "in-window":
				// Before the recorded session start, so the session is this run's work.
				since := time.UnixMilli(1577836800000) // 2020-01-01T00:00:00Z
				cfg.Since = &since
			case "out-of-window":
				// After the recorded session start, so the age filter excludes it.
				since := time.UnixMilli(1748736000000) // 2025-06-01T00:00:00Z
				cfg.Since = &since
			}
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
			// recovery first, so the stored-content lane is the one that names a
			// missing or malformed source there, and the pair lane stays quiet for
			// that same cause. Whichever lane carries it, the wording is the same
			// contract, so the text is asserted on the diagnostic that states it and
			// the total is asserted separately.
			reported := sourceUnavailableDiagnostics(result, id)
			if len(reported) != fixture.WantDiagnostics {
				t.Fatalf("the unavailable source was reported %d times, want %d: %+v", len(reported), fixture.WantDiagnostics, result.Diagnostics)
			}
			if fixture.WantDiagnostics > 0 {
				var reporting []ingest.DiagnosticEntry
				for _, diagnostic := range reported {
					if strings.Contains(diagnosticText(diagnostic), fixture.WantMentions) {
						reporting = append(reporting, diagnostic)
					}
				}
				if len(reporting) != 1 {
					t.Fatalf("want exactly one diagnostic naming %q, got %+v", fixture.WantMentions, reported)
				}
				text := diagnosticText(reporting[0])
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
			}
			assertContentRecoveryWording(t, result, id, fixture.Name, fixture.WantContentPromise)
			sessionErrors := failedSessionCount(result, id)
			if fixture.WantErrors {
				if result.Summary.Errors == 0 || sessionErrors != 1 {
					t.Fatalf("forced or unserving repair errors = summary %d session %d, want summary > 0 and exactly one session error; summary=%+v", result.Summary.Errors, sessionErrors, result.Summary)
				}
			} else if result.Summary.Errors != 0 || sessionErrors != 0 {
				t.Fatalf("a routine repair was counted as a failed session: summary=%+v sessionErrors=%d diagnostics=%+v", result.Summary, sessionErrors, result.Diagnostics)
			}
			if result.Summary.Indexed != fixture.WantIndexed {
				t.Fatalf("indexed %d sessions, want %d: %+v", result.Summary.Indexed, fixture.WantIndexed, result.Summary)
			}
			if result.Summary.Updated != fixture.WantUpdated {
				t.Fatalf("updated %d sessions, want %d: the run must still process every repair it kept: %+v", result.Summary.Updated, fixture.WantUpdated, result.Summary)
			}
		})
	}
}

// assertContentRecoveryWording pins the stored-content lane's own report. That
// lane prints a different sentence from the pair lane, so the pair lane's
// wording says nothing about it: a row with no completed index must say nothing
// is served yet, and a row that still serves must say so instead. The neutral
// clause is asserted word for word, so restoring the unconditional promise is a
// failure here and not only a change in tone.
func assertContentRecoveryWording(t *testing.T, result *ingest.PipelineResult, id ingest.SessionID, name, want string) {
	t.Helper()
	if want == "" {
		return
	}
	reports := typedSessionDiagnostics(result, id, "content_recovery_unavailable")
	if len(reports) != 1 {
		t.Fatalf("the stored-content lane reported %d times, want exactly one: %+v", len(reports), sessionDiagnostics(result, id))
	}
	text := diagnosticText(reports[0])
	switch want {
	case "serving":
		if !mentionsStoredCopyPromise(text) {
			t.Fatalf("%s: a completed index was not told its stored copy keeps serving: %q", name, text)
		}
		if strings.Contains(text, "nothing is served from the database for this session yet") {
			t.Fatalf("%s: a completed index was told nothing is served: %q", name, text)
		}
	case "neutral":
		if mentionsStoredCopyPromise(text) {
			t.Fatalf("%s: a row with no completed index was promised a stored copy: %q", name, text)
		}
		if !strings.Contains(text, "nothing is served from the database for this session yet") {
			t.Fatalf("%s: a row with no completed index did not say nothing is served yet: %q", name, text)
		}
	}
}

func mentionsStoredCopyPromise(text string) bool {
	for _, phrase := range storedCopyClaims {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
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
