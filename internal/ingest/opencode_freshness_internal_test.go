package ingest

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"gopkg.in/yaml.v3"
)

const expectedOpenCodeSQLiteFreshnessCases = 4

type openCodeSQLiteWALFixtureState string

const (
	openCodeSQLiteWALPresent openCodeSQLiteWALFixtureState = "present"
	openCodeSQLiteWALMissing openCodeSQLiteWALFixtureState = "missing"
	openCodeSQLiteWALError   openCodeSQLiteWALFixtureState = "error"
)

type openCodeSQLiteFreshnessCase struct {
	Name                     string                        `yaml:"name"`
	DatabaseMTimeMs          int64                         `yaml:"database_mtime_ms"`
	WALState                 openCodeSQLiteWALFixtureState `yaml:"wal_state"`
	WALMTimeMs               int64                         `yaml:"wal_mtime_ms"`
	WALSizeBytes             int64                         `yaml:"wal_size_bytes"`
	SHMMTimeMs               int64                         `yaml:"shm_mtime_ms"`
	ExpectedMTimeMs          int64                         `yaml:"expected_mtime_ms"`
	ExpectedErrorContains    string                        `yaml:"expected_error_contains"`
	ExpectedStatSuffixes     []string                      `yaml:"expected_stat_suffixes"`
	ProvesDatabaseOnlyMutant bool                          `yaml:"proves_database_only_mutant"`
	ProvesSHMInclusiveMutant bool                          `yaml:"proves_shm_inclusive_mutant"`
}

type openCodeSQLiteFreshnessDocument struct {
	DeclaredCases int                           `yaml:"declared_cases"`
	Cases         []openCodeSQLiteFreshnessCase `yaml:"cases"`
}

//go:embed testdata/opencode_sqlite_freshness.yaml
var openCodeSQLiteFreshnessYAML []byte

func loadOpenCodeSQLiteFreshnessDocument(data []byte) (openCodeSQLiteFreshnessDocument, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var document openCodeSQLiteFreshnessDocument
	if err := decoder.Decode(&document); err != nil {
		return document, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return document, errors.New("OpenCode SQLite freshness fixture must contain exactly one YAML document")
	}
	if document.DeclaredCases != expectedOpenCodeSQLiteFreshnessCases || len(document.Cases) != expectedOpenCodeSQLiteFreshnessCases {
		return document, errors.New("OpenCode SQLite freshness fixture count guard failed")
	}
	seen := make(map[string]struct{}, len(document.Cases))
	for _, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || testCase.DatabaseMTimeMs <= 0 || len(testCase.ExpectedStatSuffixes) != 2 {
			return document, errors.New("OpenCode SQLite freshness fixture contains an incomplete row")
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return document, errors.New("OpenCode SQLite freshness fixture contains a duplicate case name")
		}
		seen[testCase.Name] = struct{}{}
		switch testCase.WALState {
		case openCodeSQLiteWALPresent:
			if testCase.WALMTimeMs <= 0 || testCase.WALSizeBytes < 0 || testCase.ExpectedErrorContains != "" {
				return document, errors.New("present WAL freshness fixture has inconsistent evidence")
			}
		case openCodeSQLiteWALMissing:
			if testCase.WALMTimeMs != 0 || testCase.WALSizeBytes != 0 || testCase.ExpectedErrorContains != "" {
				return document, errors.New("missing WAL freshness fixture has inconsistent evidence")
			}
		case openCodeSQLiteWALError:
			if testCase.ExpectedErrorContains == "" || testCase.ExpectedMTimeMs != 0 {
				return document, errors.New("errored WAL freshness fixture lacks an expected diagnostic")
			}
		default:
			return document, errors.New("OpenCode SQLite freshness fixture contains an unknown WAL state")
		}
	}
	return document, nil
}

// openCodeSQLiteFreshnessFS and openCodeSQLiteFreshnessInfo live in
// fsfault_test.go (Owner B).

func TestLegacySQLiteContentModTimeUsesDatabaseAndWALOnly(t *testing.T) {
	document, err := loadOpenCodeSQLiteFreshnessDocument(openCodeSQLiteFreshnessYAML)
	if err != nil {
		t.Fatalf("load OpenCode SQLite freshness fixture: %v", err)
	}
	const databasePath = "/synthetic/opencode.db"
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			filesystem := &openCodeSQLiteFreshnessFS{databasePath: databasePath, testCase: testCase}
			modified, statErr := sqliteContentModTime(filesystem, databasePath)
			if testCase.ExpectedErrorContains != "" {
				if statErr == nil || !strings.Contains(statErr.Error(), testCase.ExpectedErrorContains) {
					t.Fatalf("freshness error=%v, want actionable diagnostic containing %q", statErr, testCase.ExpectedErrorContains)
				}
			} else if statErr != nil || modified.UnixMilli() != testCase.ExpectedMTimeMs {
				t.Fatalf("effective content modification time=%d error=%v, want %d", modified.UnixMilli(), statErr, testCase.ExpectedMTimeMs)
			}
			expectedPaths := make([]string, len(testCase.ExpectedStatSuffixes))
			for index, suffix := range testCase.ExpectedStatSuffixes {
				expectedPaths[index] = databasePath + suffix
			}
			if !reflect.DeepEqual(filesystem.statPaths, expectedPaths) {
				t.Fatalf("freshness stat paths=%v, want %v; shared-memory coordination time must be excluded", filesystem.statPaths, expectedPaths)
			}
			if testCase.ProvesDatabaseOnlyMutant && testCase.DatabaseMTimeMs == testCase.ExpectedMTimeMs {
				t.Fatal("fixture does not distinguish the forbidden database-only freshness alternative")
			}
			shmInclusive := max(testCase.DatabaseMTimeMs, testCase.WALMTimeMs, testCase.SHMMTimeMs)
			if testCase.ProvesSHMInclusiveMutant && shmInclusive == testCase.ExpectedMTimeMs {
				t.Fatal("fixture does not distinguish the forbidden shared-memory-inclusive freshness alternative")
			}
		})
	}
}

func TestOpenCodeSQLiteFreshnessFixtureLoaderMutationsAreRejected(t *testing.T) {
	unknownField := bytes.Replace(openCodeSQLiteFreshnessYAML, []byte("database_mtime_ms:"), []byte("unknown_database_mtime_ms:"), 1)
	if _, err := loadOpenCodeSQLiteFreshnessDocument(unknownField); err == nil {
		t.Fatal("OpenCode SQLite freshness fixture accepted an unknown field mutation")
	}
	wrongCount := bytes.Replace(openCodeSQLiteFreshnessYAML, []byte("declared_cases: 4"), []byte("declared_cases: 3"), 1)
	if _, err := loadOpenCodeSQLiteFreshnessDocument(wrongCount); err == nil {
		t.Fatal("OpenCode SQLite freshness fixture accepted an incorrect declared count")
	}
	duplicateName := bytes.Replace(openCodeSQLiteFreshnessYAML, []byte("name: missing-wal-uses-database"), []byte("name: committed-wal-is-newer"), 1)
	if _, err := loadOpenCodeSQLiteFreshnessDocument(duplicateName); err == nil {
		t.Fatal("OpenCode SQLite freshness fixture accepted a duplicate case name")
	}
	unknownState := bytes.Replace(openCodeSQLiteFreshnessYAML, []byte("wal_state: missing"), []byte("wal_state: unknown"), 1)
	if _, err := loadOpenCodeSQLiteFreshnessDocument(unknownState); err == nil {
		t.Fatal("OpenCode SQLite freshness fixture accepted an unknown WAL state")
	}
}

// TestFreshnessBySessionOmitsSessionsWithNoRows proves that the per-session
// freshness aggregate leaves a session with no rows out of the returned map, so
// a caller can tell an empty projection from a real timestamp and fall back to
// the mtime floor. A populated session carries a real, non-zero time.
func TestFreshnessBySessionOmitsSessionsWithNoRows(t *testing.T) {
	t.Run("current", func(t *testing.T) {
		materialized := testfixture.MaterializeByName(t, "current-session-message")
		path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
		if err != nil {
			t.Fatal(err)
		}
		source, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = source.Close(t.Context()) }()

		freshness, err := source.CurrentFreshnessBySession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, present := freshness["ses_absent000000000000000000"]; present {
			t.Fatal("current freshness map contains a session with no rows, want it omitted so the caller falls back to the floor")
		}
		present, ok := freshness["ses_3cd91f52effeXd3QAJ54jOyzv5"]
		if !ok || present.IsZero() {
			t.Fatalf("populated current session freshness = %v ok=%t, want a real timestamp", present, ok)
		}
	})

	t.Run("legacy", func(t *testing.T) {
		materialized := testfixture.MaterializeByName(t, "legacy-message-part")
		path, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
		if err != nil {
			t.Fatal(err)
		}
		source, err := OpenOpenCodeSQLiteSource(t.Context(), path, DefaultOpenCodeSQLiteSourceOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = source.Close(t.Context()) }()

		freshness, err := source.LegacyFreshnessBySession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, present := freshness["ses_absent000000000000000000"]; present {
			t.Fatal("legacy freshness map contains a session with no rows, want it omitted so the caller falls back to the floor")
		}
	})
}
