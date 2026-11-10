package ingest_test

import (
	"context"
	_ "embed"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_scoped_link_exactness.yaml
var claudeScopedLinkExactnessYAML []byte

type claudeScopedLinkExactnessFixtures struct {
	RequiredNames []string                        `yaml:"required_names"`
	Cases         []claudeScopedLinkExactnessCase `yaml:"cases"`
}

type claudeScopedLinkExactnessCase struct {
	Name         string               `yaml:"name"`
	DryRun       bool                 `yaml:"dry_run"`
	SeedExcludes []string             `yaml:"seed_excludes"`
	Touch        string               `yaml:"touch"`
	Corrupt      string               `yaml:"corrupt"`
	Child        string               `yaml:"child"`
	Selected     []string             `yaml:"selected"`
	WantParent   string               `yaml:"want_parent"`
	Files        []claudeTeammateFile `yaml:"files"`
}

func loadClaudeScopedLinkExactnessFixtures(t *testing.T) claudeScopedLinkExactnessFixtures {
	t.Helper()
	var fixtures claudeScopedLinkExactnessFixtures
	if err := yaml.Unmarshal(claudeScopedLinkExactnessYAML, &fixtures); err != nil {
		t.Fatalf("decode Claude scoped-link exactness fixtures: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, testCase := range fixtures.Cases {
		present[testCase.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude scoped-link exactness fixture", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestClaudeScopedLinkExactness proves the scoped build takes the store's
// identity counts only when its evidence view is complete. Both shapes fail on
// the "scoped mode always counts from the store" rule: a dry run, whose cache
// must be read (read-only) but never written, and a normal scoped run whose
// by-path read skipped an unreadable stored row.
func TestClaudeScopedLinkExactness(t *testing.T) {
	fixtures := loadClaudeScopedLinkExactnessFixtures(t)
	for _, testCase := range fixtures.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			database := openEvidenceStore(t, storetest.CopyGoldenDB(t))
			output, err := ingest.NewResolvedPath(t.TempDir())
			if err != nil {
				t.Fatalf("resolve output directory: %v", err)
			}
			mfs := testutil.NewMemFS()
			excluded := make(map[string]bool, len(testCase.SeedExcludes))
			for _, path := range testCase.SeedExcludes {
				excluded[path] = true
			}

			// Phase 1: the child alone, so its sessions row is written with no
			// parent. Phase 2 adds the spawner without re-linking the unchanged
			// child, so the store has the identity and spawns rows but the
			// child's parent cache stays NULL. The scoped run must form the link
			// itself; a stored-parent fill would otherwise mask a dropped link.
			writeClaudeScopedFiles(t, mfs, testCase.Files, excluded)
			runClaudeOnlyPipeline(t, ctx, mfs, database, output, nil, false)
			writeClaudeScopedFiles(t, mfs, testCase.Files, nil)
			seeded := runClaudeOnlyPipeline(t, ctx, mfs, database, output, nil, false)
			requireSessionParent(t, seeded, testCase.Child, testCase.WantParent)
			if parent := storedParent(t, ctx, database, testCase.Child); parent != "" {
				t.Fatalf("the seed wrote parent %q for the unchanged child; the scoped link would be masked", parent)
			}

			if testCase.Corrupt != "" {
				makeStoredEvidenceUnreadable(t, ctx, database, "/claude/"+testCase.Corrupt)
			}
			evidencePath := "/claude/" + testCase.Child
			if testCase.Touch != "" {
				touchTranscript(t, mfs, "/claude/"+testCase.Touch, testCase.Child)
			}
			modBefore, hadBefore := evidenceModTime(t, ctx, database, evidencePath)

			ids := make([]ingest.SessionID, 0, len(testCase.Selected))
			for _, id := range testCase.Selected {
				ids = append(ids, ingest.SessionID(id))
			}
			scope, err := ingest.NewSessionScope(ids, ingest.RelatedInclude)
			if err != nil {
				t.Fatalf("build session scope: %v", err)
			}
			scoped := runClaudeOnlyPipeline(t, ctx, mfs, database, output, &scope, testCase.DryRun)
			requireMemberParent(t, scoped, testCase.Child, testCase.WantParent)

			if testCase.DryRun {
				modAfter, hadAfter := evidenceModTime(t, ctx, database, evidencePath)
				if hadAfter != hadBefore || modAfter != modBefore {
					t.Errorf("the dry run wrote the evidence row (before %d/%v, after %d/%v); a forecast must write nothing", modBefore, hadBefore, modAfter, hadAfter)
				}
			}
		})
	}
}

func writeClaudeScopedFiles(t *testing.T, mfs *testutil.MemFS, files []claudeTeammateFile, excluded map[string]bool) {
	t.Helper()
	for _, file := range files {
		if excluded[file.Path] {
			continue
		}
		path := "/claude/" + file.Path
		body := strings.Join(file.Lines, "\n") + "\n"
		if err := mfs.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write transcript fixture %q: %v", file.Path, err)
		}
		mfs.ModTimes[path] = time.Now().Add(-2 * time.Hour)
	}
}

// touchTranscript rewrites a transcript with one extra harmless line, so the
// next build must mine it and would return a deferred evidence write.
func touchTranscript(t *testing.T, mfs *testutil.MemFS, path, sessionID string) {
	t.Helper()
	existing, err := mfs.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q to touch it: %v", path, err)
	}
	line := `{"sessionId":"` + sessionID + `","type":"assistant","cwd":"/workspace","message":{"role":"assistant","content":"forecast only"}}` + "\n"
	if err := mfs.WriteFile(path, append(existing, []byte(line)...), 0o644); err != nil {
		t.Fatalf("touch transcript %q: %v", path, err)
	}
	mfs.ModTimes[path] = time.Now().Add(-2 * time.Hour)
}

// runClaudeOnlyPipeline runs one harvest over /claude. scope == nil is the full
// run; the same output directory is used for every phase of a case so the
// stored pair a scoped run inspects is the one the seed wrote.
func runClaudeOnlyPipeline(t *testing.T, ctx context.Context, mfs *testutil.MemFS, database *store.Store, output ingest.ResolvedPath, scope *ingest.SessionScope, dryRun bool) *ingest.PipelineResult {
	t.Helper()
	cfg := makePipelineConfig(string(output), func(cfg *ingest.PipelineConfig) {
		cfg.Sources = map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessClaudeCode: {Paths: []ingest.ResolvedPath{"/claude"}, Enabled: true},
		}
		cfg.Command = "harvest"
		cfg.Scope = scope
		cfg.DryRun = dryRun
	})
	pipeline, err := newTestPipeline(mfs, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, cfg,
		ingest.WithIndexers(ingest.NewIndexerRegistry(mfs, ingest.IndexerRegistryOptions{})),
		ingest.WithStore(database),
		ingest.WithMetricsStore(database),
		ingest.WithIndexLogger(database),
		ingest.WithAnalyzer(metrics.NewEngine(database)),
		ingest.WithLogger(database),
		ingest.WithHarvesterVersions(maps.Clone(ingest.HarvesterVersionRegistry)),
	)
	if err != nil {
		t.Fatalf("construct pipeline: %v", err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatalf("run pipeline: %v", err)
	}
	return result
}

// requireSessionParent asserts the result carries the discovery link for id.
func requireSessionParent(t *testing.T, result *ingest.PipelineResult, id, parent string) {
	t.Helper()
	for _, session := range result.Sessions {
		if session.SessionID.String() != id {
			continue
		}
		if session.ParentUUID == nil {
			t.Fatalf("session %s has no parent in the result; want %s", id, parent)
		}
		if got := session.ParentUUID.String(); got != parent {
			t.Fatalf("session %s parent = %s, want %s", id, got, parent)
		}
		return
	}
	t.Fatalf("session %s is missing from the result", id)
}

// requireMemberParent asserts the located member built its own source with the
// named parent. It reads ScopeReport rather than the session result because the
// result can echo a parent recorded in the stored pair.
func requireMemberParent(t *testing.T, result *ingest.PipelineResult, id, parent string) {
	t.Helper()
	if result.ScopeReport == nil {
		t.Fatalf("the scoped run returned no ScopeReport")
	}
	for _, member := range result.ScopeReport.Members {
		if member.ID.String() != id {
			continue
		}
		if member.Source == nil {
			t.Fatalf("located member %s has no source", id)
		}
		if member.Source.ParentUUID == nil {
			t.Fatalf("located member %s has no discovery parent; want %s", id, parent)
		}
		if got := member.Source.ParentUUID.String(); got != parent {
			t.Fatalf("located member %s parent = %s, want %s", id, got, parent)
		}
		return
	}
	t.Fatalf("located member %s is missing from the ScopeReport", id)
}

func makeStoredEvidenceUnreadable(t *testing.T, ctx context.Context, database *store.Store, path string) {
	t.Helper()
	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take a connection to corrupt stored evidence: %v", err)
	}
	defer database.Pool().Put(conn)
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE claude_transcript_evidence SET identity_records_json = 'null' WHERE source_path = ?`,
		&sqlitex.ExecOptions{Args: []any{path}}); err != nil {
		t.Fatalf("make the stored evidence row %q unreadable: %v", path, err)
	}
}

func storedParent(t *testing.T, ctx context.Context, database *store.Store, id string) string {
	t.Helper()
	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take a connection to read the stored parent: %v", err)
	}
	defer database.Pool().Put(conn)
	parent := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{id},
		ResultFunc: func(stmt *sqlite.Stmt) error { parent = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatalf("read the stored parent of %s: %v", id, err)
	}
	return parent
}

func evidenceModTime(t *testing.T, ctx context.Context, database *store.Store, path string) (int64, bool) {
	t.Helper()
	conn, err := database.Pool().Take(ctx)
	if err != nil {
		t.Fatalf("take a connection to read the evidence row: %v", err)
	}
	defer database.Pool().Put(conn)
	mod := int64(0)
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT mod_time_unix_nano FROM claude_transcript_evidence WHERE source_path = ?`, &sqlitex.ExecOptions{
		Args: []any{path},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			mod = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read the evidence row %q: %v", path, err)
	}
	return mod, found
}
