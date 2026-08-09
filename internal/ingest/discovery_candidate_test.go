package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/discovery_candidate_matching.yaml
var discoveryCandidateFixtureYAML []byte

type discoveryCandidateFixtures struct {
	DeclaredMatchRows  int                         `yaml:"declared_match_rows"`
	DeclaredParityRows int                         `yaml:"declared_parity_rows"`
	MatchCases         []discoveryCandidateFixture `yaml:"match_cases"`
	DelegationParity   []discoveryCandidateFixture `yaml:"delegation_parity"`
}

type discoveryCandidateFixture struct {
	Name              string                    `yaml:"name"`
	ConfiguredHarness string                    `yaml:"configured_harness"`
	Projects          []discoveryProjectFixture `yaml:"projects"`
	ExplicitSessions  []string                  `yaml:"explicit_sessions"`
	AutoNewBranches   bool                      `yaml:"auto_new_branches"`
	Candidate         discoveryCandidateInput   `yaml:"candidate"`
	Expected          discoveryExpectedMatch    `yaml:"expected"`
}

type discoveryProjectFixture struct {
	GitRemote  string   `yaml:"git_remote"`
	Name       string   `yaml:"name"`
	ClonePaths []string `yaml:"clone_paths"`
	Branches   []string `yaml:"branches"`
}

type discoveryCandidateInput struct {
	Harness            string `yaml:"harness"`
	GitRemote          string `yaml:"git_remote"`
	ProjectName        string `yaml:"project_name"`
	ClonePath          string `yaml:"clone_path"`
	Branch             string `yaml:"branch"`
	SessionID          string `yaml:"session_id"`
	RemoteMultiplicity string `yaml:"remote_multiplicity"`
	NameMultiplicity   string `yaml:"name_multiplicity"`
}

type discoveryExpectedMatch string

const (
	discoveryExpectedSelected discoveryExpectedMatch = "selected"
	discoveryExpectedRejected discoveryExpectedMatch = "rejected"
	discoveryExpectedWithheld discoveryExpectedMatch = "withheld"
)

func loadDiscoveryCandidateFixtures(t *testing.T) discoveryCandidateFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(discoveryCandidateFixtureYAML))
	decoder.KnownFields(true)
	var fixtures discoveryCandidateFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode discovery candidate fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("discovery candidate fixture must contain exactly one YAML document: %v", err)
	}
	const expectedMatchRows = 21
	const expectedParityRows = 8
	if fixtures.DeclaredMatchRows != expectedMatchRows || len(fixtures.MatchCases) != expectedMatchRows {
		t.Fatalf("discovery match fixture row guard failed: declared=%d actual=%d expected=%d", fixtures.DeclaredMatchRows, len(fixtures.MatchCases), expectedMatchRows)
	}
	if fixtures.DeclaredParityRows != expectedParityRows || len(fixtures.DelegationParity) != expectedParityRows {
		t.Fatalf("discovery delegation fixture row guard failed: declared=%d actual=%d expected=%d", fixtures.DeclaredParityRows, len(fixtures.DelegationParity), expectedParityRows)
	}
	seen := make(map[string]struct{}, expectedMatchRows+expectedParityRows)
	for _, fixture := range append(append([]discoveryCandidateFixture(nil), fixtures.MatchCases...), fixtures.DelegationParity...) {
		if strings.TrimSpace(fixture.Name) == "" || strings.TrimSpace(fixture.Candidate.Harness) == "" || strings.TrimSpace(fixture.Candidate.SessionID) == "" {
			t.Fatalf("discovery fixture needs a name, candidate harness, and candidate session ID: %+v", fixture)
		}
		if _, duplicate := seen[fixture.Name]; duplicate {
			t.Fatalf("discovery fixture repeats name %q", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
		switch fixture.Expected {
		case discoveryExpectedSelected, discoveryExpectedRejected, discoveryExpectedWithheld:
		default:
			t.Fatalf("discovery fixture %q has unknown expected result %q", fixture.Name, fixture.Expected)
		}
		validateMultiplicity(t, fixture.Name, fixture.Candidate.RemoteMultiplicity)
		validateMultiplicity(t, fixture.Name, fixture.Candidate.NameMultiplicity)
	}
	return fixtures
}

func validateMultiplicity(t *testing.T, name, value string) {
	t.Helper()
	switch value {
	case "", "unique", "ambiguous":
	default:
		t.Fatalf("discovery fixture %q has unknown identity multiplicity %q", name, value)
	}
}

func TestSelectionMatcher_MatchDiscoveryCandidateEvidenceOrder(t *testing.T) {
	t.Parallel()
	fixtures := loadDiscoveryCandidateFixtures(t)
	for _, fixture := range fixtures.MatchCases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			matcher := matcherFromFixture(fixture)
			got := matcher.MatchDiscoveryCandidate(candidateFromFixture(fixture.Candidate), fixture.AutoNewBranches)
			if want := branchMatchFromFixture(fixture.Expected); got != want {
				t.Fatalf("candidate match = %v, want %v", got, want)
			}
		})
	}
}

func TestSelectionMatcher_MatchDiscoveryDelegatesToCandidateAuthority(t *testing.T) {
	t.Parallel()
	fixtures := loadDiscoveryCandidateFixtures(t)
	for _, fixture := range fixtures.DelegationParity {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			matcher := matcherFromFixture(fixture)
			candidate := candidateFromFixture(fixture.Candidate)
			candidateMatch := matcher.MatchDiscoveryCandidate(candidate, fixture.AutoNewBranches)
			legacyMatch := matcher.MatchDiscovery(candidate.Harness, candidate.GitRemote, candidate.ProjectName, candidate.Branch, candidate.SessionID, fixture.AutoNewBranches)
			want := branchMatchFromFixture(fixture.Expected)
			if candidateMatch != want || legacyMatch != want {
				t.Fatalf("candidate/legacy matches = %v/%v, want %v", candidateMatch, legacyMatch, want)
			}

			candidateDecision := matcher.MatchDiscoveryCandidateDecision(candidate, fixture.AutoNewBranches)
			legacyDecision := matcher.MatchDiscoveryDecision(candidate.Harness, candidate.GitRemote, candidate.ProjectName, candidate.Branch, candidate.SessionID, fixture.AutoNewBranches)
			if !reflect.DeepEqual(candidateDecision, legacyDecision) {
				t.Fatalf("candidate and positional decisions differ:\ncandidate: %+v\npositional: %+v", candidateDecision, legacyDecision)
			}
		})
	}
}

func matcherFromFixture(fixture discoveryCandidateFixture) ingest.SelectionMatcher {
	harness := fixture.ConfiguredHarness
	if harness == "" {
		harness = fixture.Candidate.Harness
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
	return config.CompileSelectionMatcher(config.SelectionConfig{
		Mode: config.SelectionModeSelected,
		Harnesses: map[string]config.SelectionHarnessConfig{
			harness: {
				Projects: projects,
				Sessions: append([]string(nil), fixture.ExplicitSessions...),
			},
		},
	})
}

func candidateFromFixture(input discoveryCandidateInput) ingest.DiscoveryCandidate {
	return ingest.DiscoveryCandidate{
		Harness:            ingest.Harness(input.Harness),
		GitRemote:          input.GitRemote,
		ProjectName:        input.ProjectName,
		ClonePath:          ingest.ClonePath(input.ClonePath),
		Branch:             input.Branch,
		SessionID:          ingest.SessionID(input.SessionID),
		RemoteMultiplicity: multiplicityFromFixture(input.RemoteMultiplicity),
		NameMultiplicity:   multiplicityFromFixture(input.NameMultiplicity),
	}
}

func multiplicityFromFixture(value string) ingest.DiscoveryIdentityMultiplicity {
	if value == "ambiguous" {
		return ingest.DiscoveryIdentityAmbiguous
	}
	return ingest.DiscoveryIdentityUnique
}

func branchMatchFromFixture(expected discoveryExpectedMatch) ingest.BranchMatch {
	switch expected {
	case discoveryExpectedSelected:
		return ingest.BranchMatchYes
	case discoveryExpectedWithheld:
		return ingest.BranchMatchWithheldConflict
	default:
		return ingest.BranchMatchNo
	}
}
