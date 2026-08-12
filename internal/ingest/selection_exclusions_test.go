package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_exclusions.yaml
var matcherExclusionFixtureYAML []byte

type matcherExclusionFixtures struct {
	DeclaredRows int                       `yaml:"declared_rows"`
	Cases        []matcherExclusionFixture `yaml:"cases"`
}

type matcherExclusionFixture struct {
	Name                           string                          `yaml:"name"`
	Mode                           config.SelectionMode            `yaml:"mode"`
	PositiveHarness                string                          `yaml:"positive_harness"`
	ExclusionHarness               string                          `yaml:"exclusion_harness"`
	Projects                       []discoveryProjectFixture       `yaml:"projects"`
	ExplicitSessions               []string                        `yaml:"explicit_sessions"`
	BranchExclusions               []matcherBranchExclusionFixture `yaml:"branch_exclusions"`
	SessionExclusions              []string                        `yaml:"session_exclusions"`
	AutoNewBranches                bool                            `yaml:"auto_new_branches"`
	Candidate                      discoveryCandidateInput         `yaml:"candidate"`
	ExpectedCandidate              discoveryExpectedMatch          `yaml:"expected_candidate"`
	ExpectedProjectMatch           bool                            `yaml:"expected_project_match"`
	ExpectedExcluded               bool                            `yaml:"expected_excluded"`
	ExpectedPositional             discoveryExpectedMatch          `yaml:"expected_positional"`
	ExpectedPositionalProjectMatch bool                            `yaml:"expected_positional_project_match"`
}

type matcherBranchExclusionFixture struct {
	ClonePath string   `yaml:"clone_path"`
	Branches  []string `yaml:"branches"`
}

var expectedMatcherExclusionFixtureNames = []string{
	"session-deny-overrides-explicit-session",
	"session-deny-overrides-project-admission",
	"session-deny-does-not-exclude-another-session",
	"session-deny-does-not-cross-harnesses",
	"session-deny-overrides-withheld-project-admission",
	"branch-deny-overrides-auto-new-branch-admission",
	"branch-deny-overrides-explicit-session-admission",
	"branch-deny-applies-to-unrestricted-harness",
	"branch-deny-overrides-withheld-project-admission",
	"branch-deny-does-not-exclude-sibling-clone",
	"branch-deny-does-not-exclude-sibling-branch",
	"branch-deny-never-falls-back-to-remote",
	"branch-deny-never-falls-back-to-name",
	"branch-deny-does-not-cross-harnesses",
	"branch-deny-requires-nonempty-branch-evidence",
	"exact-exclusion-is-observable-without-positive-admission",
	"candidate-branch-is-normalized-before-exact-denial",
	"branch-deny-preserves-case-sensitive-spelling",
	"mode-all-does-not-compile-selected-mode-exclusions",
}

func loadMatcherExclusionFixtures(t *testing.T) matcherExclusionFixtures {
	t.Helper()
	fixtures, err := decodeMatcherExclusionFixtures(matcherExclusionFixtureYAML)
	if err != nil {
		t.Fatalf("decode exact matcher exclusion fixtures: %v", err)
	}
	if fixtures.DeclaredRows != len(expectedMatcherExclusionFixtureNames) || len(fixtures.Cases) != len(expectedMatcherExclusionFixtureNames) {
		t.Fatalf("exact matcher exclusion fixture row guard failed: declared=%d actual=%d expected=%d", fixtures.DeclaredRows, len(fixtures.Cases), len(expectedMatcherExclusionFixtureNames))
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	actualNames := make([]string, 0, len(fixtures.Cases))
	for index, fixture := range fixtures.Cases {
		if strings.TrimSpace(fixture.Name) == "" || strings.TrimSpace(fixture.Candidate.Harness) == "" || strings.TrimSpace(fixture.Candidate.SessionID) == "" {
			t.Fatalf("exact matcher exclusion fixture row %d needs a name, harness, and session ID", index)
		}
		if _, duplicate := seen[fixture.Name]; duplicate {
			t.Fatalf("exact matcher exclusion fixture repeats name %q", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
		actualNames = append(actualNames, fixture.Name)
		validateExpectedMatch(t, fixture.Name, fixture.ExpectedCandidate)
		validateExpectedMatch(t, fixture.Name, fixture.ExpectedPositional)
		validateMultiplicity(t, fixture.Name, fixture.Candidate.RemoteMultiplicity)
		validateMultiplicity(t, fixture.Name, fixture.Candidate.NameMultiplicity)
		if _, err := ingest.NewSessionID(fixture.Candidate.SessionID); err != nil {
			t.Fatalf("exact matcher exclusion fixture %q has invalid candidate session ID: %v", fixture.Name, err)
		}
	}
	if !slices.Equal(actualNames, expectedMatcherExclusionFixtureNames) {
		t.Fatalf("exact matcher exclusion fixture names = %v, want %v", actualNames, expectedMatcherExclusionFixtureNames)
	}
	return fixtures
}

func decodeMatcherExclusionFixtures(data []byte) (matcherExclusionFixtures, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixtures matcherExclusionFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		return matcherExclusionFixtures{}, fmt.Errorf("decode fixture fields: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return matcherExclusionFixtures{}, fmt.Errorf("fixture must contain exactly one YAML document: %v", err)
	}
	return fixtures, nil
}

func validateExpectedMatch(t *testing.T, name string, expected discoveryExpectedMatch) {
	t.Helper()
	switch expected {
	case discoveryExpectedSelected, discoveryExpectedRejected, discoveryExpectedWithheld:
	default:
		t.Fatalf("exact matcher exclusion fixture %q has unknown match result %q", name, expected)
	}
}

func TestSelectionMatcher_ExactExclusionAuthority(t *testing.T) {
	t.Parallel()
	fixtures := loadMatcherExclusionFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			matcher := matcherFromExclusionFixture(fixture)
			candidate := candidateFromFixture(fixture.Candidate)
			wantCandidate := branchMatchFromFixture(fixture.ExpectedCandidate)
			wantPositional := branchMatchFromFixture(fixture.ExpectedPositional)

			if got := matcher.ExcludesCandidate(candidate); got != fixture.ExpectedExcluded {
				t.Fatalf("ExcludesCandidate = %v, want %v", got, fixture.ExpectedExcluded)
			}
			if got := matcher.MatchesCandidate(candidate); got != fixture.ExpectedProjectMatch {
				t.Fatalf("MatchesCandidate = %v, want %v", got, fixture.ExpectedProjectMatch)
			}
			if got := matcher.MatchBranchCandidate(candidate); got != wantCandidate {
				t.Fatalf("MatchBranchCandidate = %v, want %v", got, wantCandidate)
			}
			if got := matcher.MatchDiscoveryCandidate(candidate, fixture.AutoNewBranches); got != wantCandidate {
				t.Fatalf("MatchDiscoveryCandidate = %v, want %v", got, wantCandidate)
			}
			candidateDecision := matcher.MatchDiscoveryCandidateDecision(candidate, fixture.AutoNewBranches)
			if candidateDecision.Match != wantCandidate {
				t.Fatalf("MatchDiscoveryCandidateDecision = %v, want %v", candidateDecision.Match, wantCandidate)
			}

			if got := matcher.Matches(candidate.Harness, candidate.GitRemote, candidate.ProjectName, candidate.SessionID); got != fixture.ExpectedPositionalProjectMatch {
				t.Fatalf("Matches positional result = %v, want %v", got, fixture.ExpectedPositionalProjectMatch)
			}
			if got := matcher.MatchBranch(candidate.Harness, candidate.GitRemote, candidate.ProjectName, candidate.Branch, candidate.SessionID); got != wantPositional {
				t.Fatalf("MatchBranch positional result = %v, want %v", got, wantPositional)
			}
			if got := matcher.MatchDiscovery(candidate.Harness, candidate.GitRemote, candidate.ProjectName, candidate.Branch, candidate.SessionID, fixture.AutoNewBranches); got != wantPositional {
				t.Fatalf("MatchDiscovery positional result = %v, want %v", got, wantPositional)
			}
			positionalDecision := matcher.MatchDiscoveryDecision(candidate.Harness, candidate.GitRemote, candidate.ProjectName, candidate.Branch, candidate.SessionID, fixture.AutoNewBranches)
			if positionalDecision.Match != wantPositional {
				t.Fatalf("MatchDiscoveryDecision positional result = %v, want %v", positionalDecision.Match, wantPositional)
			}
			if candidate.ClonePath == "" && !reflect.DeepEqual(candidateDecision, positionalDecision) {
				t.Fatalf("candidate and positional decisions differ without clone-path evidence:\ncandidate: %+v\npositional: %+v", candidateDecision, positionalDecision)
			}
		})
	}
}

func TestMatcherExclusionFixtures_RejectUnknownFields(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(matcherExclusionFixtureYAML, []byte("declared_rows:"), []byte("unknown_field: true\ndeclared_rows:"), 1)
	if _, err := decodeMatcherExclusionFixtures(mutated); err == nil || !strings.Contains(err.Error(), "field unknown_field") {
		t.Fatalf("strict exact matcher fixture mutation error = %v, want unknown-field rejection", err)
	}
}

func matcherFromExclusionFixture(fixture matcherExclusionFixture) ingest.SelectionMatcher {
	positiveHarness := fixture.PositiveHarness
	if positiveHarness == "" {
		positiveHarness = fixture.Candidate.Harness
	}
	projects := make([]config.ProjectSelection, len(fixture.Projects))
	for index, project := range fixture.Projects {
		projects[index] = config.ProjectSelection{
			GitRemote:  project.GitRemote,
			Name:       project.Name,
			ClonePaths: append([]string(nil), project.ClonePaths...),
			Branches:   append([]string(nil), project.Branches...),
		}
	}
	mode := fixture.Mode
	if mode == "" {
		mode = config.SelectionModeSelected
	}
	selection := config.SelectionConfig{
		Mode: mode,
		Harnesses: map[string]config.SelectionHarnessConfig{
			positiveHarness: {
				Projects: projects,
				Sessions: append([]string(nil), fixture.ExplicitSessions...),
			},
		},
	}
	exclusionHarness := fixture.ExclusionHarness
	if exclusionHarness == "" {
		exclusionHarness = positiveHarness
	}
	harnessConfig := selection.Harnesses[exclusionHarness]
	harnessConfig.Exclusions.Sessions = append([]string(nil), fixture.SessionExclusions...)
	harnessConfig.Exclusions.Branches = make([]config.BranchExclusion, len(fixture.BranchExclusions))
	for index, exclusion := range fixture.BranchExclusions {
		harnessConfig.Exclusions.Branches[index] = config.BranchExclusion{
			ClonePath: exclusion.ClonePath,
			Branches:  append([]string(nil), exclusion.Branches...),
		}
	}
	selection.Harnesses[exclusionHarness] = harnessConfig
	return config.CompileSelectionMatcher(selection)
}
