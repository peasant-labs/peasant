package ingest_test

import (
	"context"
	_ "embed"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/codex_catalog.yaml
var codexCatalogYAML []byte

type codexCatalogFileSpec struct {
	Path string `yaml:"path"`
	ID   string `yaml:"id"`
}

type codexCatalogRowSpec struct {
	Path string `yaml:"path"`
	ID   string `yaml:"id"`
}

type codexCatalogCase struct {
	Name           string                 `yaml:"name"`
	Roots          []string               `yaml:"roots"`
	Files          []codexCatalogFileSpec `yaml:"files"`
	UnreadableDirs []string               `yaml:"unreadableDirs"`
	StatDenied     []string               `yaml:"statDenied"`
	SeedCatalog    []codexCatalogRowSpec  `yaml:"seedCatalog"`
	WantSessions   int                    `yaml:"wantSessions"`
	WantDiagnostic []string               `yaml:"wantDiagnostics"`
	WantCatalog    []codexCatalogRowSpec  `yaml:"wantCatalog"`
	WantCounts     map[string]int         `yaml:"wantSessionCounts"`
	// WantEnumeration is what the adapter must report about its own coverage,
	// and WantEnumerationFiles is the exact enumerated path=id set.
	WantEnumeration      string   `yaml:"wantEnumeration"`
	WantEnumerationFiles []string `yaml:"wantEnumerationFiles"`
}

type codexCatalogFixture struct {
	RequiredNames []string           `yaml:"requiredNames"`
	Cases         []codexCatalogCase `yaml:"cases"`
}

// codexCatalogWalkFS is a memory filesystem whose WalkDir reports one directory
// as unreadable and skips everything beneath it, which is what the real walk
// does when a readdir fails: the directory yields a diagnostic and its files are
// never enumerated. Stat still reaches those files unless the case denies it,
// which is the only reason a run can tell a hidden file from a deleted one.
type codexCatalogWalkFS struct {
	*testutil.MemFS
	unreadable map[string]struct{}
	statDenied map[string]struct{}
}

func (f *codexCatalogWalkFS) WalkDir(root string, fn fs.WalkDirFunc) error {
	var skipped string
	return f.MemFS.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if skipped != "" && strings.HasPrefix(path, skipped) {
			return nil
		}
		skipped = ""
		if _, hidden := f.unreadable[path]; hidden && err == nil {
			skipped = path + string(filepath.Separator)
			return fn(path, entry, &fs.PathError{Op: "readdir", Path: path, Err: fs.ErrPermission})
		}
		return fn(path, entry, err)
	})
}

func (f *codexCatalogWalkFS) Stat(path string) (os.FileInfo, error) {
	if _, denied := f.statDenied[path]; denied {
		return nil, &fs.PathError{Op: "stat", Path: path, Err: fs.ErrPermission}
	}
	return f.MemFS.Stat(path)
}

// TestCodexRolloutCatalogMaintenance pins the catalog the full walk keeps: one
// row per enumerated rollout path, and a row removed only when the walk
// re-walked its root and found the file gone. The store is real and attached the
// way the pipeline attaches it, so the assertion covers the production write
// path rather than a double.
func TestCodexRolloutCatalogMaintenance(t *testing.T) {
	var fixture codexCatalogFixture
	if err := testutil.DecodeNamedFixtureYAML(codexCatalogYAML, &fixture); err != nil {
		t.Fatalf("decode codex catalog fixture: %v", err)
	}

	for _, testCase := range fixture.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()

			mfs := testutil.NewMemFS()
			for _, file := range testCase.Files {
				body := `{"type":"session_meta","payload":{"id":"` + file.ID + `","cwd":"/work/project"}}` + "\n"
				if err := mfs.WriteFile(file.Path, []byte(body), 0o644); err != nil {
					t.Fatalf("write rollout %q: %v", file.Path, err)
				}
			}
			faults := make(map[string]struct{}, len(testCase.UnreadableDirs))
			for _, dir := range testCase.UnreadableDirs {
				faults[dir] = struct{}{}
			}
			denied := make(map[string]struct{}, len(testCase.StatDenied))
			for _, path := range testCase.StatDenied {
				denied[path] = struct{}{}
			}
			walkFS := &codexCatalogWalkFS{MemFS: mfs, unreadable: faults, statDenied: denied}

			dbPath := storetest.CopyGoldenDB(t)
			db, err := store.Open(dbPath, store.WithSkipMigrations())
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() {
				if closeErr := db.Close(); closeErr != nil {
					t.Errorf("close store: %v", closeErr)
				}
			})
			if len(testCase.SeedCatalog) > 0 {
				// Seed through the rebuild rather than the maintenance write, so
				// the seed cannot pass by sharing the code under test.
				seed := make([]ingest.RolloutFile, 0, len(testCase.SeedCatalog))
				for _, row := range testCase.SeedCatalog {
					seed = append(seed, ingest.RolloutFile{
						Path:      ingest.ResolvedPath(row.Path),
						SessionID: ingest.SessionID(row.ID),
					})
				}
				if err := db.RebuildDerivedLookups(ctx, ingest.RolloutEnumeration{Status: ingest.RolloutEnumerationComplete, Files: seed}); err != nil {
					t.Fatalf("seed the rollout catalog: %v", err)
				}
			}

			adapter := ingest.NewCodexAdapter(walkFS, testutil.NoGitResolver(), salt.Salt{})
			ingest.AttachCodexRolloutCatalog(adapter, db)

			roots := make([]ingest.ResolvedPath, 0, len(testCase.Roots))
			for _, root := range testCase.Roots {
				roots = append(roots, ingest.ResolvedPath(root))
			}
			sessions, err := adapter.Discover(ctx, ingest.SourceConfig{Enabled: true, Paths: roots})
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(sessions) != testCase.WantSessions {
				t.Fatalf("Discover returned %d sessions, want %d; a catalog write must not change discovery", len(sessions), testCase.WantSessions)
			}
			assertCodexCatalogDiagnostics(t, adapter, testCase.WantDiagnostic)
			assertCodexRolloutEnumeration(t, adapter, testCase)

			rows, err := db.LoadCodexRolloutCatalog(ctx)
			if err != nil {
				t.Fatalf("load the rollout catalog: %v", err)
			}
			got := make([]string, 0, len(rows))
			for _, row := range rows {
				got = append(got, row.Path.String()+" "+row.SessionID.String())
			}
			want := make([]string, 0, len(testCase.WantCatalog))
			for _, row := range testCase.WantCatalog {
				want = append(want, row.Path+" "+row.ID)
			}
			sort.Strings(got)
			sort.Strings(want)
			if len(got) != len(want) {
				t.Fatalf("catalog holds %d rows, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
			}
			for index := range want {
				if got[index] != want[index] {
					t.Fatalf("catalog row %d = %q, want %q\n got: %v\nwant: %v", index, got[index], want[index], got, want)
				}
			}

			for id, wantCount := range testCase.WantCounts {
				count, err := db.CountCodexRolloutsBySession(ctx, ingest.SessionID(id))
				if err != nil {
					t.Fatalf("count catalogued rollouts for %s: %v", id, err)
				}
				if count != wantCount {
					t.Fatalf("CountCodexRolloutsBySession(%s) = %d, want %d", id, count, wantCount)
				}
			}
		})
	}
}

// TestCodexLocatedBuildWritesNoCatalogRow pins the other half of the catalog
// rule: the scoped build reads its parent from the catalog through the core's
// probe, and writes nothing. A build that upserted would make a scoped run a
// catalog writer, and its rows would then outlive the walk that enumerated them.
func TestCodexLocatedBuildWritesNoCatalogRow(t *testing.T) {
	ctx := context.Background()

	const (
		childID  = ingest.SessionID("11111111-1111-4111-8111-111111111111")
		parentID = ingest.SessionID("66666666-6666-4666-8666-666666666666")
	)
	childPath := ingest.ResolvedPath("/src/codex/2026/11/09/rollout-2026-11-09T10-00-00-11111111-1111-4111-8111-111111111111.jsonl")
	parentPath := ingest.ResolvedPath("/src/codex/2026/11/08/rollout-2026-11-08T09-00-00-66666666-6666-4666-8666-666666666666.jsonl")

	mfs := testutil.NewMemFS()
	body := `{"type":"session_meta","payload":{"id":"` + childID.String() + `","cwd":"/work/project","parent_thread_id":"` + parentID.String() + `"}}` + "\n"
	if err := mfs.WriteFile(childPath.String(), []byte(body), 0o644); err != nil {
		t.Fatalf("write child rollout: %v", err)
	}

	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("close store: %v", closeErr)
		}
	})
	seed := []ingest.RolloutFile{{Path: childPath, SessionID: childID}, {Path: parentPath, SessionID: parentID}}
	if err := db.RebuildDerivedLookups(ctx, ingest.RolloutEnumeration{Status: ingest.RolloutEnumerationComplete, Files: seed}); err != nil {
		t.Fatalf("seed the rollout catalog: %v", err)
	}

	adapter := ingest.NewCodexAdapter(mfs, testutil.NoGitResolver(), salt.Salt{})
	// The store is attached even though the scoped path must not use it for a
	// write: an implementation that upserted from the build would be caught.
	ingest.AttachCodexRolloutCatalog(adapter, db)

	build, err := adapter.DiscoverSessionAt(ctx, ingest.SourceConfig{
		Enabled: true,
		Paths:   []ingest.ResolvedPath{"/src/codex"},
	}, ingest.LocatedSource{ID: childID, Path: childPath})
	if err != nil {
		t.Fatalf("DiscoverSessionAt: %v", err)
	}
	if len(build.Members) != 1 {
		t.Fatalf("DiscoverSessionAt returned %d members, want 1", len(build.Members))
	}
	probe := build.Members[0].ParentProbe
	if probe == nil {
		t.Fatal("the located build declared no parent probe; the parent it names would never be located")
	}
	if probe.Kind != ingest.ProbeCatalogWindow {
		t.Fatalf("parent probe kind = %v, want the catalog window", probe.Kind)
	}
	if probe.ID != parentID {
		t.Fatalf("parent probe id = %s, want %s", probe.ID, parentID)
	}

	rows, err := db.LoadCodexRolloutCatalog(ctx)
	if err != nil {
		t.Fatalf("load the rollout catalog: %v", err)
	}
	if len(rows) != len(seed) {
		t.Fatalf("the located build changed the catalog: %d rows, want the %d seeded; a scoped run reads the catalog and writes nothing", len(rows), len(seed))
	}
	want := append([]ingest.RolloutFile(nil), seed...)
	sort.Slice(want, func(i, j int) bool { return want[i].Path < want[j].Path })
	for index := range want {
		if rows[index] != want[index] {
			t.Fatalf("catalog row %d = %+v, want %+v; a scoped run reads the catalog and writes nothing", index, rows[index], want[index])
		}
	}
}

// TestPipelineDiscoveryAttachesTheCodexRolloutCatalog runs the production full
// walk end to end: the pipeline builds the real Codex adapter, attaches the
// store as its rollout-catalog writer, and the walk catalogues the rollout it
// enumerated. Dropping the attach leaves the table empty, so this test fails.
func TestPipelineDiscoveryAttachesTheCodexRolloutCatalog(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	output := filepath.Join(root, "managed")
	codexRoot := filepath.Join(root, "sessions")
	sessionID := ingest.SessionID("11111111-1111-4111-8111-111111111111")
	path := filepath.Join(codexRoot, "2026", "11", "09", "rollout-2026-11-09T10-00-00-"+sessionID.String()+".jsonl")
	body := `{"timestamp":"2026-11-09T10:00:00Z","type":"session_meta","payload":{"id":"` + sessionID.String() + `","cwd":"/work/project","cli_version":"0.133.0"}}` + "\n" +
		`{"timestamp":"2026-11-09T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}` + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the Codex source tree: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the rollout: %v", err)
	}

	db := storetest.Open(t)
	fs := &ingest.OSFileSystem{}
	adapters := map[ingest.Harness]ingest.AdapterFactory{
		ingest.HarnessCodex: func(filesystem ingest.FileSystem, git ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewCodexAdapter(filesystem, git, s)
		},
	}
	cfg := makePipelineConfig(output, func(c *ingest.PipelineConfig) {
		c.Sources = map[ingest.Harness]ingest.SourceConfig{
			ingest.HarnessCodex: {
				Enabled: true,
				Paths:   []ingest.ResolvedPath{ingest.ResolvedPath(codexRoot)},
			},
		}
	})
	pipeline, err := newTestPipeline(fs, testutil.DefaultGitResolver(), adapters, cfg,
		ingest.WithStore(db),
		ingest.WithMetricsStore(db),
		ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
		ingest.WithHarvesterVersions(maps.Clone(ingest.HarvesterVersionRegistry)),
	)
	if err != nil {
		t.Fatalf("build the pipeline: %v", err)
	}
	if _, err := pipeline.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rows, err := db.LoadCodexRolloutCatalog(ctx)
	if err != nil {
		t.Fatalf("load the rollout catalog: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("catalog holds %d rows, want the one rollout the full walk enumerated; the pipeline must attach the store as the Codex rollout-catalog writer", len(rows))
	}
	if rows[0].Path.String() != path || rows[0].SessionID != sessionID {
		t.Fatalf("catalog row = %+v, want %s=%s", rows[0], path, sessionID)
	}
}

func assertCodexRolloutEnumeration(t *testing.T, adapter *ingest.CodexAdapter, testCase codexCatalogCase) {
	t.Helper()
	enumeration := adapter.EnumeratedRollouts()
	wantStatus := ingest.RolloutEnumerationComplete
	if testCase.WantEnumeration == "partial" {
		wantStatus = ingest.RolloutEnumerationPartial
	}
	if enumeration.Status != wantStatus {
		t.Fatalf("enumerated rollout status = %v, want %v; a partial walk must say so, because the rebuild refuses on it", enumeration.Status, wantStatus)
	}
	got := make([]string, 0, len(enumeration.Files))
	for _, file := range enumeration.Files {
		got = append(got, file.Path.String()+"="+file.SessionID.String())
	}
	want := append([]string(nil), testCase.WantEnumerationFiles...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("enumerated %d rollout files, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("enumerated rollout %d = %q, want %q", index, got[index], want[index])
		}
	}
}

func assertCodexCatalogDiagnostics(t *testing.T, adapter *ingest.CodexAdapter, want []string) {
	t.Helper()
	codes := make([]string, 0, len(want))
	for _, diagnostic := range adapter.DiscoveryDiagnostics() {
		codes = append(codes, diagnostic.Code)
	}
	seen := make(map[string]bool, len(codes))
	for _, code := range codes {
		seen[code] = true
	}
	for _, code := range want {
		if !seen[code] {
			t.Fatalf("discovery did not report %q; diagnostics = %v", code, codes)
		}
	}
}
