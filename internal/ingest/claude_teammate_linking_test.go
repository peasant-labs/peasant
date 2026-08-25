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
	Name     string                    `yaml:"name"`
	Files    []claudeTeammateFile      `yaml:"files"`
	Expected []claudeExpectedDiscovery `yaml:"expected"`
}

type claudeTeammateFile struct {
	Path  string   `yaml:"path"`
	Lines []string `yaml:"lines"`
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
	if len(fixtures.RequiredNames) == 0 {
		return claudeTeammateFixtures{}, errors.New("Claude teammate fixture required_names is empty; list every case name the fixture must retain")
	}
	present := make(map[string]struct{}, len(fixtures.Cases))
	for _, testCase := range fixtures.Cases {
		present[testCase.Name] = struct{}{}
	}
	seenRequired := make(map[string]struct{}, len(fixtures.RequiredNames))
	for _, name := range fixtures.RequiredNames {
		if name == "" {
			return claudeTeammateFixtures{}, errors.New("Claude teammate fixture required_names has a blank entry")
		}
		if _, duplicate := seenRequired[name]; duplicate {
			return claudeTeammateFixtures{}, fmt.Errorf("Claude teammate fixture required_names repeats %q", name)
		}
		seenRequired[name] = struct{}{}
		if _, ok := present[name]; !ok {
			return claudeTeammateFixtures{}, fmt.Errorf("Claude teammate fixture is missing required case %q; restore the row or remove it from required_names", name)
		}
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
			if len(sessions) != len(fixture.Expected) {
				t.Fatalf("discovered %d sessions, want %d", len(sessions), len(fixture.Expected))
			}

			byID := make(map[string]ingest.DiscoveredSession, len(sessions))
			for _, session := range sessions {
				byID[string(session.SessionID)] = session
			}
			for _, expected := range fixture.Expected {
				session, ok := byID[expected.SessionID]
				if !ok {
					t.Fatalf("session %q not discovered", expected.SessionID)
				}
				gotParent := ""
				if session.ParentUUID != nil {
					gotParent = string(*session.ParentUUID)
				}
				if gotParent != expected.ParentUUID {
					t.Errorf("session %q parent = %q, want %q", expected.SessionID, gotParent, expected.ParentUUID)
				}
				gotPaths := make([]string, len(session.SubagentPaths))
				for index, path := range session.SubagentPaths {
					gotPaths[index] = string(path)
				}
				if strings.Join(gotPaths, "\n") != strings.Join(expected.SubagentPaths, "\n") {
					t.Errorf("session %q subagent paths = %q, want %q", expected.SessionID, gotPaths, expected.SubagentPaths)
				}
			}
		})
	}
}
