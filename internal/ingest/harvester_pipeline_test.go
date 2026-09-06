package ingest_test

import (
	_ "embed"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/harvester_pipeline.yaml
var harvesterPipelineYAML []byte

func TestPipelineHarvesterTargets(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Sessions      []struct {
			ID         ingest.SessionID `yaml:"id"`
			Harness    ingest.Harness   `yaml:"harness"`
			Transcript string           `yaml:"transcript"`
		} `yaml:"sessions"`
		Cases []struct {
			Name              string           `yaml:"name"`
			Reindex           bool             `yaml:"reindex"`
			Force             bool             `yaml:"force"`
			DryRun            bool             `yaml:"dryRun"`
			Scope             *ingest.Harness  `yaml:"scope"`
			ClaudeTarget      int              `yaml:"claudeTarget"`
			CodexTarget       int              `yaml:"codexTarget"`
			ClaudeStored      int              `yaml:"claudeStored"`
			ExpectedHarnesses []ingest.Harness `yaml:"expectedHarnesses"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(harvesterPipelineYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("invalid fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			fs := testutil.NewMemFS()
			db, err := store.Open(filepath.Join(t.TempDir(), "peasant.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			before := make(map[ingest.SessionID][]schema.SessionEntry)
			versions := maps.Clone(ingest.HarvesterVersionRegistry)
			claude, codex := versions[ingest.HarnessClaudeCode], versions[ingest.HarnessCodex]
			claude.IndexerVersion, codex.IndexerVersion = fixture.ClaudeTarget, fixture.CodexTarget
			versions[ingest.HarnessClaudeCode], versions[ingest.HarnessCodex] = claude, codex
			for _, session := range fixtures.Sessions {
				meta := makeReindexMeta(t, string(session.ID), "/missing/native.jsonl")
				meta.ModelHarness = session.Harness
				meta.Project.Hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
				_, path := setupPeasantSyncSession(t, fs, testOutputDir, testutil.TestHostSlug, string(session.ID), meta)
				if err := fs.WriteFile(path, []byte(session.Transcript), 0600); err != nil {
					t.Fatal(err)
				}
				if err := db.InsertSessions(ctx, []ingest.StoreEntry{{Metadata: meta, Session: ingest.DiscoveredSession{SessionID: session.ID, Harness: session.Harness}}}); err != nil {
					t.Fatal(err)
				}
				previous := 15
				if session.Harness == ingest.HarnessClaudeCode {
					previous = fixture.ClaudeStored
				}
				content := "last-good " + string(session.Harness)
				entries := []schema.SessionEntry{{SessionID: session.ID, Harness: session.Harness, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &content}}
				write := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: session.ID, Entries: entries, IndexerVersion: previous, IndexedAtMs: 1700000001000}})
				if len(write) != 1 || !write[0].Written {
					t.Fatalf("seed index: %+v", write)
				}
				before[session.ID], err = db.ListEntries(ctx, session.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			cfg := makePipelineConfig(testOutputDir)
			cfg.Reindex, cfg.Force, cfg.DryRun, cfg.Harness = fixture.Reindex, fixture.Force, fixture.DryRun, fixture.Scope
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter(nil, nil)}
			pipeline, err := ingest.NewPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg, ingest.WithStore(db), ingest.WithMetricsStore(db), ingest.WithIndexLogger(db), ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})), ingest.WithHarvesterVersions(versions))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(result.Summary.HarvesterVersions, versions) {
				t.Fatalf("summary targets = %+v", result.Summary.HarvesterVersions)
			}
			got := make([]ingest.Harness, 0)
			for _, entry := range result.IndexLog {
				if entry.IndexerVersion != versions[entry.Harness].IndexerVersion {
					t.Fatalf("log producer target = %+v", entry)
				}
				if entry.Outcome == ingest.IndexOutcomeIndexed || entry.Outcome == ingest.IndexOutcomeReindexed {
					got = append(got, entry.Harness)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, fixture.ExpectedHarnesses) || result.Summary.Indexed != len(fixture.ExpectedHarnesses) {
				t.Fatalf("indexed harnesses=%v count=%d, want %v", got, result.Summary.Indexed, fixture.ExpectedHarnesses)
			}
			for _, session := range fixtures.Sessions {
				entries, err := db.ListEntries(ctx, session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Contains(fixture.ExpectedHarnesses, session.Harness) && !reflect.DeepEqual(entries, before[session.ID]) {
					t.Fatalf("unselected/refused %s entries changed", session.Harness)
				}
				conn, err := db.Pool().Take(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var stamp int
				err = sqlitex.ExecuteTransient(conn, "SELECT index_version FROM sessions WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{string(session.ID)}, ResultFunc: func(stmt *sqlite.Stmt) error { stamp = stmt.ColumnInt(0); return nil }})
				db.Pool().Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				want := 15
				if session.Harness == ingest.HarnessClaudeCode {
					want = fixture.ClaudeStored
				}
				if slices.Contains(fixture.ExpectedHarnesses, session.Harness) {
					want = versions[session.Harness].IndexerVersion
				}
				if stamp != want {
					t.Fatalf("%s actual producer=%d, want %d", session.Harness, stamp, want)
				}
			}
		})
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("missing required fixture %q", name)
		}
	}
}
