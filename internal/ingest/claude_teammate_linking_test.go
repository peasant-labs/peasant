package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_teammate_linking.yaml
var claudeTeammateLinkingYAML []byte

type claudeTeammateFixtures struct {
	// RequiredNames is a deletion-protection manifest: every listed case
	// name must be present in Cases. It does not bound how many cases
	// exist, so adding a new case never requires touching this list.
	RequiredNames []string                `yaml:"required_names"`
	Cases         []claudeTeammateFixture `yaml:"cases"`
}

type claudeTeammateFixture struct {
	Name string `yaml:"name"`
	// Files and Expected describe a single Discover call (same-batch linking).
	Files    []claudeTeammateFile      `yaml:"files"`
	Expected []claudeExpectedDiscovery `yaml:"expected"`
	// Runs describes a sequence of Discover calls sharing one evidence cache,
	// for cross-run linking cases. When present, Files/Expected above are
	// unused: Expected below describes the outcome of the LAST run.
	Runs []claudeTeammateRun `yaml:"runs"`
}

type claudeTeammateFile struct {
	Path  string   `yaml:"path"`
	Lines []string `yaml:"lines"`
}

// claudeTeammateRun is one Discover call in a cross-run fixture. Paths sets
// the source roots THIS call walks, so a root written in an earlier run but
// absent from this run's Paths is visible only through the persisted
// evidence cache, exactly as a differently-scoped later ingest would see it.
// StoredSessions names the session ids that become "already stored" once this
// run completes, simulating the write stage a real pipeline run performs
// after Discover returns.
type claudeTeammateRun struct {
	Paths          []string             `yaml:"paths"`
	Files          []claudeTeammateFile `yaml:"files"`
	StoredSessions []string             `yaml:"stored_sessions"`
}

type claudeExpectedDiscovery struct {
	SessionID     string   `yaml:"session_id"`
	ParentUUID    string   `yaml:"parent_uuid"`
	SubagentPaths []string `yaml:"subagent_paths"`
}

func loadClaudeTeammateFixtures(t *testing.T) claudeTeammateFixtures {
	t.Helper()
	fixtures, err := decodeClaudeTeammateFixtures(claudeTeammateLinkingYAML)
	if err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func decodeClaudeTeammateFixtures(source []byte) (claudeTeammateFixtures, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	var fixtures claudeTeammateFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		return claudeTeammateFixtures{}, fmt.Errorf("decode Claude teammate fixtures: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return claudeTeammateFixtures{}, fmt.Errorf("Claude teammate fixture must contain exactly one YAML document: %v", err)
	}
	present := make(map[string]bool, len(fixtures.Cases))
	for _, testCase := range fixtures.Cases {
		present[testCase.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude teammate fixture", "case", fixtures.RequiredNames, present); err != nil {
		return claudeTeammateFixtures{}, err
	}
	return fixtures, nil
}

// TestClaudeTeammateFixtureGuardsRequiredCaseDeletion mutation-proves the
// required-name manifest: deleting a required case's block must fail the
// load with a message naming the missing case. This replaces the old
// declared_rows count guard, which would have also failed on any addition
// to the fixture.
func TestClaudeTeammateFixtureGuardsRequiredCaseDeletion(t *testing.T) {
	t.Parallel()

	// Baseline: the real, unmutated fixture must load cleanly first, so a
	// failure below is known to come from the mutation and not a broken
	// manifest.
	if _, err := decodeClaudeTeammateFixtures(claudeTeammateLinkingYAML); err != nil {
		t.Fatalf("baseline fixture failed to decode before mutation: %v", err)
	}

	const firstCaseMarker = "  - name: exact unique teammate spawn links roots\n"
	const secondCaseMarker = "  - name: teammate without matching spawn stays root\n"
	firstIndex := bytes.Index(claudeTeammateLinkingYAML, []byte(firstCaseMarker))
	secondIndex := bytes.Index(claudeTeammateLinkingYAML, []byte(secondCaseMarker))
	if firstIndex < 0 || secondIndex <= firstIndex {
		t.Fatalf("could not locate the first case block boundaries in the fixture (first=%d second=%d)", firstIndex, secondIndex)
	}

	mutated := append(append([]byte{}, claudeTeammateLinkingYAML[:firstIndex]...), claudeTeammateLinkingYAML[secondIndex:]...)
	_, err := decodeClaudeTeammateFixtures(mutated)
	if err == nil {
		t.Fatal("fixture decoder accepted a corpus missing a required case block")
	}
	if !strings.Contains(err.Error(), `missing required case "exact unique teammate spawn links roots"`) {
		t.Fatalf("deleted-required-case error = %v, want it to name the missing case", err)
	}
}

func TestClaudeAdapter_DiscoverTeammateLineage(t *testing.T) {
	fixtures := loadClaudeTeammateFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		if len(fixture.Runs) > 0 {
			continue // covered by TestClaudeAdapter_DiscoverTeammateLineageAcrossRuns
		}
		t.Run(fixture.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			for _, file := range fixture.Files {
				if err := mfs.WriteFile("/claude/"+file.Path, []byte(strings.Join(file.Lines, "\n")+"\n"), 0o644); err != nil {
					t.Fatalf("write transcript fixture %q: %v", file.Path, err)
				}
			}

			adapter := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
			sessions, err := adapter.Discover(context.Background(), ingest.SourceConfig{
				Paths:   []ingest.ResolvedPath{"/claude"},
				Enabled: true,
			})
			if err != nil {
				t.Fatalf("discover Claude fixture: %v", err)
			}
			assertClaudeTeammateDiscovery(t, sessions, fixture.Expected)
		})
	}
}

// assertClaudeTeammateDiscovery checks discovered sessions against the expectations
// one fixture case names, shared by the same-batch and cross-run test
// functions so both hold discovered sessions to the identical contract.
func assertClaudeTeammateDiscovery(t *testing.T, sessions []ingest.DiscoveredSession, expected []claudeExpectedDiscovery) {
	t.Helper()
	if len(sessions) != len(expected) {
		t.Fatalf("discovered %d sessions, want %d", len(sessions), len(expected))
	}

	byID := make(map[string]ingest.DiscoveredSession, len(sessions))
	for _, session := range sessions {
		byID[string(session.SessionID)] = session
	}
	for _, want := range expected {
		session, ok := byID[want.SessionID]
		if !ok {
			t.Fatalf("session %q not discovered", want.SessionID)
		}
		gotParent := ""
		if session.ParentUUID != nil {
			gotParent = string(*session.ParentUUID)
		}
		if gotParent != want.ParentUUID {
			t.Errorf("session %q parent = %q, want %q", want.SessionID, gotParent, want.ParentUUID)
		}
		gotPaths := make([]string, len(session.SubagentPaths))
		for index, path := range session.SubagentPaths {
			gotPaths[index] = string(path)
		}
		if strings.Join(gotPaths, "\n") != strings.Join(want.SubagentPaths, "\n") {
			t.Errorf("session %q subagent paths = %q, want %q", want.SessionID, gotPaths, want.SubagentPaths)
		}
	}
}

// fakeClaudeStoreCache is a minimal in-memory double for the evidence cache
// the real local store provides. It also answers LookupSessionLocation, the
// exact method the store already exposes for an unrelated reason (pre-
// populating the diff-stage location cache), which cross-run linking reuses
// to confirm a persisted-only candidate parent is really stored before
// pointing a child at it. markStored simulates the write stage a real
// pipeline run performs after Discover returns.
type fakeClaudeStoreCache struct {
	evidence map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence
	stored   map[ingest.SessionID]string // session id -> its stored parent id ("" for a stored root)
}

func newFakeClaudeStoreCache() *fakeClaudeStoreCache {
	return &fakeClaudeStoreCache{
		evidence: make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence),
		stored:   make(map[ingest.SessionID]string),
	}
}

func (f *fakeClaudeStoreCache) LoadClaudeEvidence(context.Context) (map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, error) {
	out := make(map[ingest.ResolvedPath]ingest.ClaudeTranscriptEvidence, len(f.evidence))
	for path, record := range f.evidence {
		out[path] = record
	}
	return out, nil
}

func (f *fakeClaudeStoreCache) SaveClaudeEvidence(_ context.Context, upserts []ingest.ClaudeTranscriptEvidence, deletes []ingest.ResolvedPath) error {
	for _, record := range upserts {
		f.evidence[record.SourcePath] = record
	}
	for _, path := range deletes {
		delete(f.evidence, path)
	}
	return nil
}

// LookupSessionLocation mirrors the store's contract: an unknown session id
// answers empty strings and no error.
func (f *fakeClaudeStoreCache) LookupSessionLocation(_ context.Context, sessionID ingest.SessionID) (string, string, error) {
	parentID, ok := f.stored[sessionID]
	if !ok {
		return "", "", nil
	}
	return "fake-host", parentID, nil
}

func (f *fakeClaudeStoreCache) markStored(sessionID ingest.SessionID, parentID string) {
	f.stored[sessionID] = parentID
}

// TestClaudeAdapter_DiscoverTeammateLineageAcrossRuns runs each fixture's Runs
// sequence against ONE adapter sharing ONE evidence cache, so the second (and
// later) Discover calls see whatever the earlier calls persisted — exactly
// the cross-run linking path the same-batch test above cannot exercise.
func TestClaudeAdapter_DiscoverTeammateLineageAcrossRuns(t *testing.T) {
	fixtures := loadClaudeTeammateFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		if len(fixture.Runs) == 0 {
			continue // covered by TestClaudeAdapter_DiscoverTeammateLineage
		}
		t.Run(fixture.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			cache := newFakeClaudeStoreCache()
			adapter := ingest.NewClaudeAdapter(mfs, testutil.DefaultGitResolver(), salt.Salt{})
			ingest.AttachClaudeEvidenceCache(adapter, cache)

			var sessions []ingest.DiscoveredSession
			for runIndex, run := range fixture.Runs {
				for _, file := range run.Files {
					if err := mfs.WriteFile("/claude/"+file.Path, []byte(strings.Join(file.Lines, "\n")+"\n"), 0o644); err != nil {
						t.Fatalf("run %d: write transcript fixture %q: %v", runIndex, file.Path, err)
					}
				}

				paths := make([]ingest.ResolvedPath, len(run.Paths))
				for i, p := range run.Paths {
					paths[i] = ingest.ResolvedPath(p)
				}

				var err error
				sessions, err = adapter.Discover(context.Background(), ingest.SourceConfig{
					Paths:   paths,
					Enabled: true,
				})
				if err != nil {
					t.Fatalf("run %d: discover Claude fixture: %v", runIndex, err)
				}

				byID := make(map[string]ingest.DiscoveredSession, len(sessions))
				for _, session := range sessions {
					byID[string(session.SessionID)] = session
				}
				for _, sid := range run.StoredSessions {
					parentID := ""
					if session, ok := byID[sid]; ok && session.ParentUUID != nil {
						parentID = string(*session.ParentUUID)
					}
					cache.markStored(ingest.SessionID(sid), parentID)
				}
			}

			assertClaudeTeammateDiscovery(t, sessions, fixture.Expected)
		})
	}
}
