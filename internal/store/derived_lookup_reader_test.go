package store_test

import (
	"context"
	_ "embed"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/derived_lookup_reader.yaml
var derivedLookupReaderYAML []byte

//go:embed testdata/derived_lookup_reader.manifest.yaml
var derivedLookupReaderManifestYAML []byte

type derivedSpawn struct {
	Team string `yaml:"team"`
	Name string `yaml:"name"`
}

type derivedEvidenceSpec struct {
	Path         string         `yaml:"path"`
	Scope        string         `yaml:"scope"`
	IdentityTeam string         `yaml:"identityTeam"`
	IdentityName string         `yaml:"identityName"`
	Spawns       []derivedSpawn `yaml:"spawns"`
}

type derivedRolloutSpec struct {
	Path    string `yaml:"path"`
	Session string `yaml:"session"`
}

type derivedReaderCase struct {
	Name              string   `yaml:"name"`
	Team              string   `yaml:"team"`
	IdentityName      string   `yaml:"identityName"`
	Session           string   `yaml:"session"`
	WantDeclarers     int      `yaml:"wantDeclarers"`
	WantSpawners      int      `yaml:"wantSpawners"`
	CheckSpawnerPaths bool     `yaml:"checkSpawnerPaths"`
	WantSpawnerPaths  []string `yaml:"wantSpawnerPaths"`
	WantRolloutCount  int      `yaml:"wantRolloutCount"`
	WantRolloutPaths  []string `yaml:"wantRolloutPaths"`
}

type derivedReaderFixture struct {
	Evidence []derivedEvidenceSpec `yaml:"evidence"`
	Rollouts []derivedRolloutSpec  `yaml:"rollouts"`
	Cases    []derivedReaderCase   `yaml:"cases"`
}

func loadDerivedReaderFixture(t *testing.T) derivedReaderFixture {
	t.Helper()
	var fixture derivedReaderFixture
	if err := testutil.DecodeFixtureYAML(derivedLookupReaderYAML, &fixture); err != nil {
		t.Fatalf("decode derived lookup reader fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(derivedLookupReaderManifestYAML, "derived lookup reader")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "derived lookup reader"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestDerivedLookupReaderIndexedAnswers drives the two Claude identity counts
// and the Codex rollout count and paths. Each is a single indexed read over a
// derived table; the fixture seeds the authoritative evidence and the catalog
// through their production write paths where one exists.
func TestDerivedLookupReaderIndexedAnswers(t *testing.T) {
	t.Parallel()
	fixture := loadDerivedReaderFixture(t)
	db := storetest.Open(t)
	ctx := context.Background()
	seedClaudeEvidence(t, db, fixture.Evidence)
	seedRolloutCatalog(t, db, fixture.Rollouts)

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			if testCase.Team != "" || testCase.IdentityName != "" {
				identity := ingest.ClaudeTeammateIdentity{Team: testCase.Team, Name: testCase.IdentityName}
				declarers, err := db.CountClaudeIdentityDeclarers(ctx, identity)
				if err != nil {
					t.Fatal(err)
				}
				if declarers != testCase.WantDeclarers {
					t.Fatalf("CountClaudeIdentityDeclarers(%v) = %d, want %d", identity, declarers, testCase.WantDeclarers)
				}
				spawners, err := db.CountClaudeIdentitySpawners(ctx, identity)
				if err != nil {
					t.Fatal(err)
				}
				if spawners != testCase.WantSpawners {
					t.Fatalf("CountClaudeIdentitySpawners(%v) = %d, want %d", identity, spawners, testCase.WantSpawners)
				}
			}
			if testCase.CheckSpawnerPaths {
				paths, err := db.ClaudeIdentitySpawnerPaths(ctx, testCase.Team, testCase.IdentityName)
				if err != nil {
					t.Fatal(err)
				}
				want := append([]string(nil), testCase.WantSpawnerPaths...)
				sort.Strings(want)
				if len(paths) != len(want) {
					t.Fatalf("ClaudeIdentitySpawnerPaths(%q, %q) = %v, want %v", testCase.Team, testCase.IdentityName, paths, want)
				}
				for index := range want {
					if paths[index] != want[index] {
						t.Fatalf("ClaudeIdentitySpawnerPaths(%q, %q) = %v, want %v", testCase.Team, testCase.IdentityName, paths, want)
					}
				}
			}
			if testCase.Session != "" {
				id := ingest.SessionID(testCase.Session)
				count, err := db.CountCodexRolloutsBySession(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if count != testCase.WantRolloutCount {
					t.Fatalf("CountCodexRolloutsBySession(%s) = %d, want %d", id, count, testCase.WantRolloutCount)
				}
				paths, err := db.CodexRolloutPaths(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				got := make([]string, 0, len(paths))
				for _, path := range paths {
					got = append(got, path.String())
				}
				want := append([]string(nil), testCase.WantRolloutPaths...)
				sort.Strings(want)
				if len(got) != len(want) {
					t.Fatalf("CodexRolloutPaths(%s) = %v, want %v", id, got, want)
				}
				for index := range want {
					if got[index] != want[index] {
						t.Fatalf("CodexRolloutPaths(%s) = %v, want %v", id, got, want)
					}
				}
			}
		})
	}
}

func seedClaudeEvidence(t *testing.T, db *store.Store, specs []derivedEvidenceSpec) {
	t.Helper()
	upserts := make([]ingest.ClaudeTranscriptEvidence, 0, len(specs))
	for _, spec := range specs {
		scope := ingest.ClaudeEvidenceScope(spec.Scope)
		if !scope.IsValid() {
			t.Fatalf("fixture evidence %s names unknown scope %q", spec.Path, spec.Scope)
		}
		record := ingest.ClaudeTranscriptEvidence{
			SourcePath: ingest.ResolvedPath(spec.Path),
			Scope:      scope,
		}
		if spec.IdentityTeam != "" || spec.IdentityName != "" {
			record.Identity = &ingest.ClaudeTeammateIdentity{Team: spec.IdentityTeam, Name: spec.IdentityName}
		}
		for _, spawn := range spec.Spawns {
			record.Spawns = append(record.Spawns, ingest.ClaudeTeammateIdentity{Team: spawn.Team, Name: spawn.Name})
		}
		upserts = append(upserts, record)
	}
	if err := db.SaveClaudeEvidence(context.Background(), upserts, nil); err != nil {
		t.Fatalf("seed Claude evidence: %v", err)
	}
}

func seedRolloutCatalog(t *testing.T, db *store.Store, specs []derivedRolloutSpec) {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	for _, spec := range specs {
		if _, err := ingest.NewSessionID(spec.Session); err != nil {
			t.Fatalf("fixture rollout %s names invalid session: %v", spec.Path, err)
		}
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO codex_rollout_catalog(source_path, session_id) VALUES (?, ?)`, &sqlitex.ExecOptions{
			Args: []any{spec.Path, spec.Session},
		}); err != nil {
			t.Fatalf("seed rollout catalog %s: %v", spec.Path, err)
		}
	}
}
