package ingest_test

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_stale_spawner.yaml
var claudeStaleSpawnerYAML []byte

type claudeStaleSpawnerFixtures struct {
	RequiredNames []string                 `yaml:"required_names"`
	Cases         []claudeStaleSpawnerCase `yaml:"cases"`
}

type claudeStaleSpawnerCase struct {
	Name          string               `yaml:"name"`
	Child         string               `yaml:"child"`
	StaleSpawner  string               `yaml:"stale_spawner"`
	StoredSpawner string               `yaml:"stored_spawner"`
	Identity      claudeStaleIdentity  `yaml:"identity"`
	WantParent    string               `yaml:"want_parent"`
	Files         []claudeTeammateFile `yaml:"files"`
}

type claudeStaleIdentity struct {
	Team string `yaml:"team"`
	Name string `yaml:"name"`
}

func loadClaudeStaleSpawnerFixtures(t *testing.T) claudeStaleSpawnerFixtures {
	t.Helper()
	var fixtures claudeStaleSpawnerFixtures
	if err := yaml.Unmarshal(claudeStaleSpawnerYAML, &fixtures); err != nil {
		t.Fatalf("decode Claude stale-spawner fixtures: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, testCase := range fixtures.Cases {
		present[testCase.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude stale-spawner fixture", "case", fixtures.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestClaudeAdapter_StaleStoredSpawnerKeepsTheCorrectLink proves the link step
// filters a stored spawner path this run re-mined to no longer spawn. The store
// still holds the stale row until the evidence write, so the spawner-path read
// returns both it and the real spawner; without the filter the decision sees
// two paths and drops a link the evidence view forms.
func TestClaudeAdapter_StaleStoredSpawnerKeepsTheCorrectLink(t *testing.T) {
	fixtures := loadClaudeStaleSpawnerFixtures(t)
	for _, testCase := range fixtures.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			for _, file := range testCase.Files {
				body := strings.Join(file.Lines, "\n") + "\n"
				if err := mfs.WriteFile("/claude/"+file.Path, []byte(body), 0o644); err != nil {
					t.Fatalf("write transcript fixture %q: %v", file.Path, err)
				}
			}

			identity := ingest.ClaudeTeammateIdentity{Team: testCase.Identity.Team, Name: testCase.Identity.Name}
			cache := newFakeClaudeLinkingEvidenceCache()
			stalePath := ingest.ResolvedPath("/claude/" + testCase.StaleSpawner)
			storedPath := ingest.ResolvedPath("/claude/" + testCase.StoredSpawner)
			// The stale row still records the spawn, so the build's delta must
			// subtract it when the transcript is re-mined without it.
			cache.evidence[stalePath] = ingest.ClaudeTranscriptEvidence{
				SourcePath:            stalePath,
				Scope:                 ingest.ClaudeEvidenceScopeRoot,
				ModTimeUnixNano:       -1,
				SizeBytes:             -1,
				HasConversationRecord: true,
				Spawns:                []ingest.ClaudeTeammateIdentity{identity},
			}
			// The real spawner was discovered earlier and is not walked now.
			cache.evidence[storedPath] = ingest.ClaudeTranscriptEvidence{
				SourcePath:            storedPath,
				Scope:                 ingest.ClaudeEvidenceScopeRoot,
				ModTimeUnixNano:       1,
				SizeBytes:             1,
				HasConversationRecord: true,
				Spawns:                []ingest.ClaudeTeammateIdentity{identity},
			}

			derived := &failingClaudeDerivedLookup{
				spawners:     2,
				spawnerPaths: []string{string(stalePath), string(storedPath)},
			}
			location := newFakeSessionLocationLookup()
			location.markStored(ingest.SessionID(testCase.WantParent), "")

			adapter := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
			ingest.AttachClaudeEvidenceCache(adapter, cache)
			ingest.AttachClaudeDerivedLookup(adapter, derived)
			ingest.AttachSessionLocationLookup(adapter, location)

			sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{
				Paths:   []ingest.ResolvedPath{"/claude"},
				Enabled: true,
			})
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			byID := make(map[string]ingest.DiscoveredSession, len(sessions))
			for _, session := range sessions {
				byID[string(session.SessionID)] = session
			}
			child, ok := byID[probeIDFromPath(testCase.Child)]
			if !ok {
				t.Fatalf("child %s was not discovered", testCase.Child)
			}
			if child.ParentUUID == nil {
				t.Fatalf("child %s has no parent; the stale stored spawner path was not filtered and the correct link was dropped", probeIDFromPath(testCase.Child))
			}
			if got := child.ParentUUID.String(); got != testCase.WantParent {
				t.Errorf("child %s parent = %s, want %s", probeIDFromPath(testCase.Child), got, testCase.WantParent)
			}
		})
	}
}

// probeIDFromPath is the session id a fixture transcript path encodes.
func probeIDFromPath(path string) string {
	base := path
	if index := strings.LastIndex(path, "/"); index >= 0 {
		base = path[index+1:]
	}
	return strings.TrimSuffix(base, ".jsonl")
}
