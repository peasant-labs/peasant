package ingest

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	expectedCanonicalTieCases     = 3
	expectedCanonicalPermutations = 6
	expectedCanonicalTieMutations = 4
)

type canonicalTieRepresentation string

const (
	canonicalTieCurrent canonicalTieRepresentation = "current_sqlite"
	canonicalTieLegacy  canonicalTieRepresentation = "legacy_sqlite"
	canonicalTieJSON    canonicalTieRepresentation = "legacy_json"
)

type canonicalTieFixture struct {
	DeclaredCases           int                          `yaml:"declared_cases"`
	Cases                   []canonicalTieCase           `yaml:"cases"`
	DeclaredLoaderMutations int                          `yaml:"declared_loader_mutations"`
	LoaderMutations         []canonicalTieLoaderMutation `yaml:"loader_mutations"`
}

type canonicalTieCase struct {
	Name              string                     `yaml:"name"`
	Representation    canonicalTieRepresentation `yaml:"representation"`
	Paths             []string                   `yaml:"paths"`
	ExpectedPath      string                     `yaml:"expected_path"`
	ExpectedCleanPath string                     `yaml:"expected_clean_path"`
	Permutations      [][]int                    `yaml:"permutations"`
}

type canonicalTieLoaderMutation struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"`
}

//go:embed testdata/opencode_canonical_tiebreak.yaml
var canonicalTieYAML []byte

func loadCanonicalTieFixture(data []byte) (canonicalTieFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixture canonicalTieFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode canonical OpenCode tie-break fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, errors.New("canonical OpenCode tie-break fixture must contain exactly one YAML document")
	}
	if fixture.DeclaredCases != expectedCanonicalTieCases || len(fixture.Cases) != expectedCanonicalTieCases || fixture.DeclaredLoaderMutations != expectedCanonicalTieMutations || len(fixture.LoaderMutations) != expectedCanonicalTieMutations {
		return fixture, errors.New("canonical OpenCode tie-break fixture count guard failed")
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || seen[testCase.Name] || len(testCase.Paths) != 3 || len(testCase.Permutations) != expectedCanonicalPermutations || !filepath.IsAbs(testCase.ExpectedPath) || !filepath.IsAbs(testCase.ExpectedCleanPath) {
			return fixture, fmt.Errorf("canonical OpenCode tie-break fixture contains incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		if _, err := testCase.Representation.production(); err != nil {
			return fixture, err
		}
		permutations := make(map[string]bool, len(testCase.Permutations))
		for _, permutation := range testCase.Permutations {
			if len(permutation) != len(testCase.Paths) {
				return fixture, fmt.Errorf("canonical tie case %q has incomplete permutation %v", testCase.Name, permutation)
			}
			positions := make(map[int]bool, len(permutation))
			for _, position := range permutation {
				if position < 0 || position >= len(testCase.Paths) || positions[position] {
					return fixture, fmt.Errorf("canonical tie case %q has invalid permutation %v", testCase.Name, permutation)
				}
				positions[position] = true
			}
			key := fmt.Sprint(permutation)
			if permutations[key] {
				return fixture, fmt.Errorf("canonical tie case %q duplicates permutation %v", testCase.Name, permutation)
			}
			permutations[key] = true
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		switch mutation.Kind {
		case "unknown_field", "wrong_count", "duplicate_name", "unknown_representation":
		default:
			return fixture, fmt.Errorf("canonical tie fixture has unknown mutation %q", mutation.Kind)
		}
	}
	return fixture, nil
}

func (representation canonicalTieRepresentation) production() (OpenCodeCanonicalRepresentation, error) {
	switch representation {
	case canonicalTieCurrent:
		return OpenCodeRepresentationCurrentSQLite, nil
	case canonicalTieLegacy:
		return OpenCodeRepresentationLegacySQLite, nil
	case canonicalTieJSON:
		return OpenCodeRepresentationLegacyJSON, nil
	default:
		return 0, fmt.Errorf("unknown canonical tie representation %q", representation)
	}
}

func TestCanonicalOpenCodeEqualRankTieBreakIsPermutationStable(t *testing.T) {
	fixture, err := loadCanonicalTieFixture(canonicalTieYAML)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			representation, _ := testCase.Representation.production()
			base := make([]openCodeSessionCandidate, len(testCase.Paths))
			for index, path := range testCase.Paths {
				base[index] = openCodeSessionCandidate{
					session:  DiscoveredSession{SessionID: sessionID, SourcePath: ResolvedPath(path)},
					identity: OpenCodeSelectedSourceIdentity{SessionID: sessionID, Representation: representation, Path: ResolvedPath(path)},
				}
			}
			for _, permutation := range testCase.Permutations {
				candidates := make([]openCodeSessionCandidate, len(permutation))
				for index, sourceIndex := range permutation {
					candidates[index] = base[sourceIndex]
				}
				selected, selectErr := selectCanonicalOpenCodeCandidates(candidates)
				if selectErr != nil || len(selected) != 1 || selected[0].identity.Path.String() != testCase.ExpectedPath || filepath.Clean(selected[0].identity.Path.String()) != testCase.ExpectedCleanPath {
					t.Fatalf("permutation %v selected %+v error=%v, want path %q (clean %q)", permutation, selected, selectErr, testCase.ExpectedPath, testCase.ExpectedCleanPath)
				}
			}
		})
	}
}

func TestCanonicalOpenCodeTieFixtureRejectsMutations(t *testing.T) {
	fixture, err := loadCanonicalTieFixture(canonicalTieYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range fixture.LoaderMutations {
		mutated := append([]byte(nil), canonicalTieYAML...)
		switch mutation.Kind {
		case "unknown_field":
			mutated = bytes.Replace(mutated, []byte("expected_clean_path:"), []byte("unexpected:"), 1)
		case "wrong_count":
			mutated = bytes.Replace(mutated, []byte("declared_cases: 3"), []byte("declared_cases: 2"), 1)
		case "duplicate_name":
			mutated = bytes.Replace(mutated, []byte("legacy-sqlite-path-order"), []byte("current-sqlite-path-order"), 1)
		case "unknown_representation":
			mutated = bytes.Replace(mutated, []byte("representation: current_sqlite"), []byte("representation: event_history"), 1)
		}
		if _, err := loadCanonicalTieFixture(mutated); err == nil || strings.TrimSpace(mutation.Name) == "" {
			t.Errorf("canonical tie loader mutation %q was accepted", mutation.Name)
		}
	}
}
