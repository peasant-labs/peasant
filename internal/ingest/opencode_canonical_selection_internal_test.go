package ingest

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	expectedCanonicalTieCases     = 3
	expectedCanonicalPermutations = 6
	expectedCanonicalMountedCases = 1
	expectedCanonicalTieMutations = 6
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
	DeclaredMountedCases    int                          `yaml:"declared_mounted_cases"`
	MountedCases            []canonicalMountedTieCase    `yaml:"mounted_cases"`
	DeclaredLoaderMutations int                          `yaml:"declared_loader_mutations"`
	LoaderMutations         []canonicalTieLoaderMutation `yaml:"loader_mutations"`
}

type canonicalMountedTieCase struct {
	Name                string   `yaml:"name"`
	SourceFixture       string   `yaml:"source_fixture"`
	SessionID           string   `yaml:"session_id"`
	MarkerRowID         string   `yaml:"marker_row_id"`
	RootNames           []string `yaml:"root_names"`
	Enumeration         []int    `yaml:"enumeration"`
	ExpectedWinnerIndex int      `yaml:"expected_winner_index"`
	WinningMarker       string   `yaml:"winning_marker"`
	LosingMarker        string   `yaml:"losing_marker"`
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
	if fixture.DeclaredCases != expectedCanonicalTieCases || len(fixture.Cases) != expectedCanonicalTieCases || fixture.DeclaredMountedCases != expectedCanonicalMountedCases || len(fixture.MountedCases) != expectedCanonicalMountedCases || fixture.DeclaredLoaderMutations != expectedCanonicalTieMutations || len(fixture.LoaderMutations) != expectedCanonicalTieMutations {
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
	for _, testCase := range fixture.MountedCases {
		if testCase.Name == "" || seen[testCase.Name] || testCase.SourceFixture == "" || testCase.SessionID == "" || testCase.MarkerRowID == "" || len(testCase.RootNames) != 2 || len(testCase.Enumeration) != 2 || testCase.ExpectedWinnerIndex < 0 || testCase.ExpectedWinnerIndex >= len(testCase.RootNames) || testCase.WinningMarker == "" || testCase.LosingMarker == "" || testCase.WinningMarker == testCase.LosingMarker {
			return fixture, fmt.Errorf("canonical mounted tie fixture contains incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		if testCase.RootNames[0] == testCase.RootNames[1] || filepath.IsAbs(testCase.RootNames[0]) || filepath.IsAbs(testCase.RootNames[1]) || filepath.Clean(testCase.RootNames[testCase.ExpectedWinnerIndex]) >= filepath.Clean(testCase.RootNames[1-testCase.ExpectedWinnerIndex]) {
			return fixture, fmt.Errorf("canonical mounted tie case %q does not identify the lexical root winner", testCase.Name)
		}
		if testCase.Enumeration[0] == testCase.ExpectedWinnerIndex || testCase.Enumeration[0] == testCase.Enumeration[1] || testCase.Enumeration[0] < 0 || testCase.Enumeration[0] > 1 || testCase.Enumeration[1] < 0 || testCase.Enumeration[1] > 1 {
			return fixture, fmt.Errorf("canonical mounted tie case %q does not enumerate the losing root first", testCase.Name)
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if mutation.Name == "" || seen[mutation.Name] {
			return fixture, fmt.Errorf("canonical tie fixture has incomplete or duplicate mutation %q", mutation.Name)
		}
		seen[mutation.Name] = true
		switch mutation.Kind {
		case "unknown_field", "wrong_count", "duplicate_name", "unknown_representation", "wrong_mounted_count", "winner_enumerated_first":
		default:
			return fixture, fmt.Errorf("canonical tie fixture has unknown mutation %q", mutation.Kind)
		}
	}
	return fixture, nil
}

type canonicalTieEnvironment map[string]string

func (environment canonicalTieEnvironment) LookupEnv(key string) (string, bool) {
	value, ok := environment[key]
	return value, ok
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

func TestCanonicalOpenCodeEqualRankTieBreakThroughMountedDiscover(t *testing.T) {
	fixture, err := loadCanonicalTieFixture(canonicalTieYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.MountedCases {
		t.Run(testCase.Name, func(t *testing.T) {
			workspace := t.TempDir()
			roots := make([]ResolvedPath, len(testCase.RootNames))
			databasePaths := make([]string, len(testCase.RootNames))
			for index, rootName := range testCase.RootNames {
				source := testfixture.MaterializeByName(t, testCase.SourceFixture)
				rootPath := filepath.Join(workspace, rootName)
				if err := os.MkdirAll(rootPath, 0o700); err != nil {
					t.Fatal(err)
				}
				databasePaths[index] = filepath.Join(rootPath, "opencode.db")
				data, err := os.ReadFile(source.Path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(databasePaths[index], data, 0o600); err != nil {
					t.Fatal(err)
				}
				marker := testCase.LosingMarker
				if index == testCase.ExpectedWinnerIndex {
					marker = testCase.WinningMarker
				}
				setCanonicalMountedMarker(t, databasePaths[index], testCase.MarkerRowID, marker)
				roots[index], err = NewResolvedPath(rootPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			enumerated := []ResolvedPath{roots[testCase.Enumeration[0]], roots[testCase.Enumeration[1]]}
			filesystem := &OSFileSystem{}
			adapter, err := NewOpenCodeAdapterWithCandidateProbe(filesystem, noGitResolver{}, salt.Salt{}, "latest", canonicalTieEnvironment{}, filesystem, OpenOpenCodeSQLiteSource, DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
			discovered, err := adapter.Discover(t.Context(), SourceConfig{Enabled: true, Paths: enumerated})
			if err != nil {
				t.Fatal(err)
			}
			expectedPath := filepath.Clean(databasePaths[testCase.ExpectedWinnerIndex])
			if len(discovered) != 1 || string(discovered[0].SessionID) != testCase.SessionID || filepath.Clean(discovered[0].SourcePath.String()) != expectedPath {
				t.Fatalf("mounted equal-rank discovery selected %+v, want one session %q from lexical path %q", discovered, testCase.SessionID, expectedPath)
			}
			_, transcript, err := adapter.MaterializeTranscript(t.Context(), discovered[0])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(transcript, []byte(testCase.WinningMarker)) || bytes.Contains(transcript, []byte(testCase.LosingMarker)) {
				t.Fatalf("mounted equal-rank winner payload=%s, want marker %q without %q", transcript, testCase.WinningMarker, testCase.LosingMarker)
			}
		})
	}
}

func setCanonicalMountedMarker(t testing.TB, path, rowID, marker string) {
	t.Helper()
	connection, err := sqlite.OpenConn(path, sqlite.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	data := fmt.Sprintf(`{"id":%q,"text":%q,"files":[],"agents":[],"metadata":{"cwd":"/synthetic/tie"},"time":{"created":1000}}`, rowID, marker)
	updateErr := sqlitex.Execute(connection, `UPDATE session_message SET data = ?1 WHERE id = ?2`, &sqlitex.ExecOptions{Args: []any{data, rowID}})
	closeErr := connection.Close()
	if updateErr != nil || closeErr != nil {
		t.Fatalf("write mounted tie marker: %v", errors.Join(updateErr, closeErr))
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
		case "wrong_mounted_count":
			mutated = bytes.Replace(mutated, []byte("declared_mounted_cases: 1"), []byte("declared_mounted_cases: 2"), 1)
		case "winner_enumerated_first":
			mutated = bytes.Replace(mutated, []byte("enumeration: [0, 1]"), []byte("enumeration: [1, 0]"), 1)
		}
		if _, err := loadCanonicalTieFixture(mutated); err == nil || strings.TrimSpace(mutation.Name) == "" {
			t.Errorf("canonical tie loader mutation %q was accepted", mutation.Name)
		}
	}
}
