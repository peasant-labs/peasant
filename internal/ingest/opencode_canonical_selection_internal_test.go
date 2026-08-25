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

const expectedCanonicalPermutations = 6

type canonicalTieRepresentation string

const (
	canonicalTieCurrent canonicalTieRepresentation = "current_sqlite"
	canonicalTieLegacy  canonicalTieRepresentation = "legacy_sqlite"
	canonicalTieJSON    canonicalTieRepresentation = "legacy_json"
)

// canonicalTieMutationKind is the closed set of loader mutations the
// tie-break fixture proves non-vacuous.
type canonicalTieMutationKind string

const (
	canonicalTieMutationUnknownField          canonicalTieMutationKind = "unknown_field"
	canonicalTieMutationWrongCount            canonicalTieMutationKind = "wrong_count"
	canonicalTieMutationDuplicateName         canonicalTieMutationKind = "duplicate_name"
	canonicalTieMutationUnknownRepresentation canonicalTieMutationKind = "unknown_representation"
	canonicalTieMutationWrongMountedCount     canonicalTieMutationKind = "wrong_mounted_count"
	canonicalTieMutationWinnerEnumeratedFirst canonicalTieMutationKind = "winner_enumerated_first"
	canonicalTieMutationOverrideSortsFirst    canonicalTieMutationKind = "override_sorts_first"
	canonicalTieMutationUnknownProvenance     canonicalTieMutationKind = "unknown_provenance"
)

func (kind canonicalTieMutationKind) validate() error {
	switch kind {
	case canonicalTieMutationUnknownField, canonicalTieMutationWrongCount, canonicalTieMutationDuplicateName, canonicalTieMutationUnknownRepresentation, canonicalTieMutationWrongMountedCount, canonicalTieMutationWinnerEnumeratedFirst, canonicalTieMutationOverrideSortsFirst, canonicalTieMutationUnknownProvenance:
		return nil
	default:
		return fmt.Errorf("canonical tie fixture has unknown mutation %q", kind)
	}
}

type canonicalTieFixture struct {
	RequiredCases                  []string                            `yaml:"required_cases"`
	Cases                          []canonicalTieCase                  `yaml:"cases"`
	RequiredProvenanceCases        []string                            `yaml:"required_provenance_cases"`
	ProvenanceCases                []canonicalProvenanceTieCase        `yaml:"provenance_cases"`
	RequiredMountedCases           []string                            `yaml:"required_mounted_cases"`
	MountedCases                   []canonicalMountedTieCase           `yaml:"mounted_cases"`
	RequiredMountedProvenanceCases []string                            `yaml:"required_mounted_provenance_cases"`
	MountedProvenanceCases         []canonicalMountedProvenanceTieCase `yaml:"mounted_provenance_cases"`
	RequiredLoaderMutations        []string                            `yaml:"required_loader_mutations"`
	LoaderMutations                []canonicalTieLoaderMutation        `yaml:"loader_mutations"`
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

// canonicalMountedProvenanceTieCase mounts one root that holds the channel
// database and an override database whose name sorts after it.
type canonicalMountedProvenanceTieCase struct {
	Name          string `yaml:"name"`
	SourceFixture string `yaml:"source_fixture"`
	SessionID     string `yaml:"session_id"`
	MarkerRowID   string `yaml:"marker_row_id"`
	OverrideName  string `yaml:"override_name"`
	WinningMarker string `yaml:"winning_marker"`
	LosingMarker  string `yaml:"losing_marker"`
}

type canonicalTieCase struct {
	Name              string                     `yaml:"name"`
	Representation    canonicalTieRepresentation `yaml:"representation"`
	Paths             []string                   `yaml:"paths"`
	ExpectedPath      string                     `yaml:"expected_path"`
	ExpectedCleanPath string                     `yaml:"expected_clean_path"`
	Permutations      [][]int                    `yaml:"permutations"`
}

type canonicalProvenanceCandidate struct {
	Path       string                      `yaml:"path"`
	Provenance OpenCodeCandidateProvenance `yaml:"provenance"`
}

type canonicalProvenanceTieCase struct {
	Name           string                         `yaml:"name"`
	Representation canonicalTieRepresentation     `yaml:"representation"`
	Candidates     []canonicalProvenanceCandidate `yaml:"candidates"`
	ExpectedPath   string                         `yaml:"expected_path"`
	Permutations   [][]int                        `yaml:"permutations"`
}

type canonicalTieLoaderMutation struct {
	Name string                   `yaml:"name"`
	Kind canonicalTieMutationKind `yaml:"kind"`
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
	if len(fixture.RequiredCases) == 0 || len(fixture.RequiredProvenanceCases) == 0 || len(fixture.RequiredMountedCases) == 0 || len(fixture.RequiredMountedProvenanceCases) == 0 || len(fixture.RequiredLoaderMutations) == 0 {
		return fixture, errors.New("canonical OpenCode tie-break fixture declares an empty required manifest")
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
		if err := validateCanonicalPermutations(testCase.Name, len(testCase.Paths), testCase.Permutations); err != nil {
			return fixture, err
		}
	}
	for _, testCase := range fixture.ProvenanceCases {
		if testCase.Name == "" || seen[testCase.Name] || len(testCase.Candidates) != 2 || len(testCase.Permutations) != 2 || !filepath.IsAbs(testCase.ExpectedPath) {
			return fixture, fmt.Errorf("canonical OpenCode provenance tie fixture contains incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		if _, err := testCase.Representation.production(); err != nil {
			return fixture, err
		}
		if err := validateCanonicalPermutations(testCase.Name, len(testCase.Candidates), testCase.Permutations); err != nil {
			return fixture, err
		}
		override, channel := testCase.Candidates[0], testCase.Candidates[1]
		for _, candidate := range testCase.Candidates {
			if err := candidate.Provenance.Validate(); err != nil {
				return fixture, fmt.Errorf("canonical OpenCode provenance tie case %q: %w", testCase.Name, err)
			}
		}
		if override.Provenance != OpenCodeCandidateOverride || channel.Provenance != OpenCodeCandidateChannel || override.Path != testCase.ExpectedPath || filepath.Clean(override.Path) <= filepath.Clean(channel.Path) {
			return fixture, fmt.Errorf("canonical OpenCode provenance tie case %q must expect the override path that sorts after the channel path", testCase.Name)
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
	for _, testCase := range fixture.MountedProvenanceCases {
		if testCase.Name == "" || seen[testCase.Name] || testCase.SourceFixture == "" || testCase.SessionID == "" || testCase.MarkerRowID == "" || testCase.OverrideName == "" || filepath.IsAbs(testCase.OverrideName) || testCase.WinningMarker == "" || testCase.LosingMarker == "" || testCase.WinningMarker == testCase.LosingMarker {
			return fixture, fmt.Errorf("canonical mounted provenance tie fixture contains incomplete or duplicate case %+v", testCase)
		}
		seen[testCase.Name] = true
		if filepath.Clean(testCase.OverrideName) <= "opencode.db" {
			return fixture, fmt.Errorf("canonical mounted provenance tie case %q must name an override database that sorts after the channel database", testCase.Name)
		}
	}
	for _, mutation := range fixture.LoaderMutations {
		if mutation.Name == "" || seen[mutation.Name] {
			return fixture, fmt.Errorf("canonical tie fixture has incomplete or duplicate mutation %q", mutation.Name)
		}
		seen[mutation.Name] = true
		if err := mutation.Kind.validate(); err != nil {
			return fixture, err
		}
	}
	for label, required := range map[string][]string{
		"case":                    fixture.RequiredCases,
		"provenance case":         fixture.RequiredProvenanceCases,
		"mounted case":            fixture.RequiredMountedCases,
		"mounted provenance case": fixture.RequiredMountedProvenanceCases,
		"loader mutation":         fixture.RequiredLoaderMutations,
	} {
		for _, name := range required {
			if !seen[name] {
				return fixture, fmt.Errorf("canonical OpenCode tie-break fixture is missing required %s %q", label, name)
			}
		}
	}
	return fixture, nil
}

func validateCanonicalPermutations(name string, size int, permutations [][]int) error {
	seen := make(map[string]bool, len(permutations))
	for _, permutation := range permutations {
		if len(permutation) != size {
			return fmt.Errorf("canonical tie case %q has incomplete permutation %v", name, permutation)
		}
		positions := make(map[int]bool, len(permutation))
		for _, position := range permutation {
			if position < 0 || position >= size || positions[position] {
				return fmt.Errorf("canonical tie case %q has invalid permutation %v", name, permutation)
			}
			positions[position] = true
		}
		key := fmt.Sprint(permutation)
		if seen[key] {
			return fmt.Errorf("canonical tie case %q duplicates permutation %v", name, permutation)
		}
		seen[key] = true
	}
	return nil
}

type canonicalTieEnvironment map[string]string

var _ OpenCodeEnvironmentLookup = canonicalTieEnvironment{}

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
					session:    DiscoveredSession{SessionID: sessionID, SourcePath: ResolvedPath(path)},
					identity:   OpenCodeSelectedSourceIdentity{SessionID: sessionID, Representation: representation, Path: ResolvedPath(path)},
					provenance: OpenCodeCandidateChannel,
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

// TestCanonicalOpenCodeEqualRankProvenanceOutranksPath proves that within one
// representation the environment override beats the channel database even when
// the override path sorts after the channel path.
func TestCanonicalOpenCodeEqualRankProvenanceOutranksPath(t *testing.T) {
	fixture, err := loadCanonicalTieFixture(canonicalTieYAML)
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.ProvenanceCases {
		t.Run(testCase.Name, func(t *testing.T) {
			representation, _ := testCase.Representation.production()
			base := make([]openCodeSessionCandidate, len(testCase.Candidates))
			for index, candidate := range testCase.Candidates {
				base[index] = openCodeSessionCandidate{
					session:    DiscoveredSession{SessionID: sessionID, SourcePath: ResolvedPath(candidate.Path)},
					identity:   OpenCodeSelectedSourceIdentity{SessionID: sessionID, Representation: representation, Path: ResolvedPath(candidate.Path)},
					provenance: candidate.Provenance,
				}
			}
			for _, permutation := range testCase.Permutations {
				candidates := make([]openCodeSessionCandidate, len(permutation))
				for index, sourceIndex := range permutation {
					candidates[index] = base[sourceIndex]
				}
				selected, selectErr := selectCanonicalOpenCodeCandidates(candidates)
				if selectErr != nil || len(selected) != 1 || selected[0].identity.Path.String() != testCase.ExpectedPath {
					t.Fatalf("permutation %v selected %+v error=%v, want override path %q", permutation, selected, selectErr, testCase.ExpectedPath)
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
				rootPath := filepath.Join(workspace, rootName)
				marker := testCase.LosingMarker
				if index == testCase.ExpectedWinnerIndex {
					marker = testCase.WinningMarker
				}
				databasePaths[index] = copyCanonicalMountedDatabase(t, testCase.SourceFixture, rootPath, "opencode.db", testCase.MarkerRowID, marker)
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
			assertCanonicalMountedWinner(t, adapter, discovered, testCase.SessionID, expectedPath, testCase.WinningMarker, testCase.LosingMarker)
		})
	}
}

// TestCanonicalOpenCodeOverrideOutranksChannelThroughMountedDiscover mounts
// the channel database and an OPENCODE_DB override in one root. The override
// wins even though its file name sorts after opencode.db.
func TestCanonicalOpenCodeOverrideOutranksChannelThroughMountedDiscover(t *testing.T) {
	fixture, err := loadCanonicalTieFixture(canonicalTieYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.MountedProvenanceCases {
		t.Run(testCase.Name, func(t *testing.T) {
			rootPath := t.TempDir()
			copyCanonicalMountedDatabase(t, testCase.SourceFixture, rootPath, "opencode.db", testCase.MarkerRowID, testCase.LosingMarker)
			overridePath := copyCanonicalMountedDatabase(t, testCase.SourceFixture, rootPath, testCase.OverrideName, testCase.MarkerRowID, testCase.WinningMarker)
			root, err := NewResolvedPath(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			filesystem := &OSFileSystem{}
			adapter, err := NewOpenCodeAdapterWithCandidateProbe(filesystem, noGitResolver{}, salt.Salt{}, "latest", canonicalTieEnvironment{openCodeDatabaseOverrideEnv: overridePath}, filesystem, OpenOpenCodeSQLiteSource, DefaultOpenCodeSQLiteSourceOptions())
			if err != nil {
				t.Fatal(err)
			}
			discovered, err := adapter.Discover(t.Context(), SourceConfig{Enabled: true, Paths: []ResolvedPath{root}})
			if err != nil {
				t.Fatal(err)
			}
			assertCanonicalMountedWinner(t, adapter, discovered, testCase.SessionID, filepath.Clean(overridePath), testCase.WinningMarker, testCase.LosingMarker)
		})
	}
}

func assertCanonicalMountedWinner(t testing.TB, adapter *OpenCodeAdapter, discovered []DiscoveredSession, sessionID, expectedPath, winningMarker, losingMarker string) {
	t.Helper()
	if len(discovered) != 1 || string(discovered[0].SessionID) != sessionID || filepath.Clean(discovered[0].SourcePath.String()) != expectedPath {
		t.Fatalf("mounted equal-rank discovery selected %+v, want one session %q from path %q", discovered, sessionID, expectedPath)
	}
	_, transcript, err := adapter.MaterializeTranscript(t.Context(), discovered[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(transcript, []byte(winningMarker)) || bytes.Contains(transcript, []byte(losingMarker)) {
		t.Fatalf("mounted equal-rank winner payload=%s, want marker %q without %q", transcript, winningMarker, losingMarker)
	}
}

// copyCanonicalMountedDatabase materializes the named corpus, copies it into
// rootPath under fileName, and stamps the marker row. It returns the copy path.
func copyCanonicalMountedDatabase(t testing.TB, sourceFixture, rootPath, fileName, rowID, marker string) string {
	t.Helper()
	source := testfixture.MaterializeByName(t, sourceFixture)
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(rootPath, fileName)
	data, err := os.ReadFile(source.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(databasePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	setCanonicalMountedMarker(t, databasePath, rowID, marker)
	return databasePath
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
		case canonicalTieMutationUnknownField:
			mutated = bytes.Replace(mutated, []byte("expected_clean_path:"), []byte("unexpected:"), 1)
		case canonicalTieMutationWrongCount:
			mutated = bytes.Replace(mutated, []byte("\n  - legacy-json-path-order\n"), []byte("\n  - legacy-json-renamed-away\n"), 1)
		case canonicalTieMutationDuplicateName:
			mutated = bytes.Replace(mutated, []byte("name: legacy-sqlite-path-order"), []byte("name: current-sqlite-path-order"), 1)
		case canonicalTieMutationUnknownRepresentation:
			mutated = bytes.Replace(mutated, []byte("representation: current_sqlite"), []byte("representation: event_history"), 1)
		case canonicalTieMutationWrongMountedCount:
			mutated = bytes.Replace(mutated, []byte("\n  - mounted-current-sqlite-lexical-winner\n"), []byte("\n  - mounted-current-renamed-away\n"), 1)
		case canonicalTieMutationWinnerEnumeratedFirst:
			mutated = bytes.Replace(mutated, []byte("enumeration: [0, 1]"), []byte("enumeration: [1, 0]"), 1)
		case canonicalTieMutationOverrideSortsFirst:
			mutated = bytes.Replace(mutated, []byte("override_name: z-override.db"), []byte("override_name: a-override.db"), 1)
		case canonicalTieMutationUnknownProvenance:
			mutated = bytes.Replace(mutated, []byte("provenance: environment_override"), []byte("provenance: event_history"), 1)
		}
		if _, err := loadCanonicalTieFixture(mutated); err == nil || strings.TrimSpace(mutation.Name) == "" {
			t.Errorf("canonical tie loader mutation %q was accepted", mutation.Name)
		}
	}
}
