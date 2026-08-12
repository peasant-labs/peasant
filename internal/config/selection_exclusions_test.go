package config_test

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
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_exclusions.yaml
var selectionExclusionFixtureYAML []byte

type selectionExclusionFixtures struct {
	DeclaredRows int                         `yaml:"declared_rows"`
	Cases        []selectionExclusionFixture `yaml:"cases"`
}

type selectionExclusionFixture struct {
	Name                      string   `yaml:"name"`
	Document                  string   `yaml:"document"`
	Valid                     bool     `yaml:"valid"`
	ErrorContains             []string `yaml:"error_contains"`
	ExpectedHarnesses         int      `yaml:"expected_harnesses"`
	ExpectedBranchExclusions  int      `yaml:"expected_branch_exclusions"`
	ExpectedSessionExclusions int      `yaml:"expected_session_exclusions"`
	ExpectExclusionsKey       bool     `yaml:"expect_exclusions_key"`
}

var expectedSelectionExclusionFixtureNames = []string{
	"legacy-selected-yaml-without-exclusions",
	"selected-exact-exclusions-round-trip",
	"identical-evidence-is-valid-across-harnesses",
	"exclusions-require-selected-mode",
	"empty-session-id-is-rejected",
	"malformed-session-id-is-rejected",
	"non-normalized-session-id-is-rejected",
	"duplicate-session-id-is-rejected",
	"empty-clone-path-is-rejected",
	"relative-clone-path-is-rejected",
	"unclean-clone-path-is-rejected",
	"empty-branch-list-is-rejected",
	"empty-branch-name-is-rejected",
	"non-normalized-branch-name-is-rejected",
	"duplicate-branch-name-is-rejected",
	"malformed-git-branch-name-is-rejected",
	"duplicate-clone-path-row-is-rejected",
	"empty-harness-cannot-scope-an-exclusion",
	"unknown-harness-cannot-scope-an-exclusion",
}

func loadSelectionExclusionFixtures(t *testing.T) selectionExclusionFixtures {
	t.Helper()
	fixtures, err := decodeSelectionExclusionFixtures(selectionExclusionFixtureYAML)
	if err != nil {
		t.Fatalf("decode exact selection exclusion fixtures: %v", err)
	}
	if fixtures.DeclaredRows != len(expectedSelectionExclusionFixtureNames) || len(fixtures.Cases) != len(expectedSelectionExclusionFixtureNames) {
		t.Fatalf("exact selection exclusion fixture row guard failed: declared=%d actual=%d expected=%d", fixtures.DeclaredRows, len(fixtures.Cases), len(expectedSelectionExclusionFixtureNames))
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	actualNames := make([]string, 0, len(fixtures.Cases))
	for index, fixture := range fixtures.Cases {
		if strings.TrimSpace(fixture.Name) == "" || strings.TrimSpace(fixture.Document) == "" {
			t.Fatalf("exact selection exclusion fixture row %d needs a name and document", index)
		}
		if _, duplicate := seen[fixture.Name]; duplicate {
			t.Fatalf("exact selection exclusion fixture repeats name %q", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
		actualNames = append(actualNames, fixture.Name)
		if fixture.Valid && len(fixture.ErrorContains) != 0 {
			t.Fatalf("valid exact selection exclusion fixture %q must not declare errors", fixture.Name)
		}
		if !fixture.Valid && len(fixture.ErrorContains) == 0 {
			t.Fatalf("invalid exact selection exclusion fixture %q must declare actionable error fragments", fixture.Name)
		}
	}
	if !slices.Equal(actualNames, expectedSelectionExclusionFixtureNames) {
		t.Fatalf("exact selection exclusion fixture names = %v, want %v", actualNames, expectedSelectionExclusionFixtureNames)
	}
	return fixtures
}

func decodeSelectionExclusionFixtures(data []byte) (selectionExclusionFixtures, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var fixtures selectionExclusionFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		return selectionExclusionFixtures{}, fmt.Errorf("decode fixture fields: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return selectionExclusionFixtures{}, fmt.Errorf("fixture must contain exactly one YAML document: %v", err)
	}
	return fixtures, nil
}

func TestSelectionExclusions_ValidateAndRoundTrip(t *testing.T) {
	t.Parallel()
	fixtures := loadSelectionExclusionFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			cfg, err := config.Parse([]byte(fixture.Document))
			if !fixture.Valid {
				if err == nil {
					t.Fatal("configuration unexpectedly accepted invalid exact exclusion evidence")
				}
				for _, fragment := range fixture.ErrorContains {
					if !strings.Contains(err.Error(), fragment) {
						t.Fatalf("configuration error %q does not contain actionable field fragment %q", err, fragment)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("parse valid exact exclusion configuration: %v", err)
			}

			branchCount, sessionCount := countSelectionExclusions(cfg.Selection)
			if len(cfg.Selection.Harnesses) != fixture.ExpectedHarnesses || branchCount != fixture.ExpectedBranchExclusions || sessionCount != fixture.ExpectedSessionExclusions {
				t.Fatalf("parsed exact exclusion counts = harnesses:%d branches:%d sessions:%d, want %d/%d/%d", len(cfg.Selection.Harnesses), branchCount, sessionCount, fixture.ExpectedHarnesses, fixture.ExpectedBranchExclusions, fixture.ExpectedSessionExclusions)
			}
			encoded, err := yaml.Marshal(cfg.Selection)
			if err != nil {
				t.Fatalf("marshal validated exact exclusion selection: %v", err)
			}
			if strings.Contains(string(encoded), "exclusions:") != fixture.ExpectExclusionsKey {
				t.Fatalf("marshaled selection exclusions presence = %v, want %v\n%s", strings.Contains(string(encoded), "exclusions:"), fixture.ExpectExclusionsKey, encoded)
			}
			roundTripped, err := decodeSelectionStrict(encoded)
			if err != nil {
				t.Fatalf("strictly decode exact exclusion round trip: %v", err)
			}
			if !reflect.DeepEqual(roundTripped, cfg.Selection) {
				t.Fatalf("exact exclusion selection changed during YAML round trip:\nparsed: %+v\nround trip: %+v", cfg.Selection, roundTripped)
			}
		})
	}
}

func TestSelectionExclusionFixtures_RejectUnknownFields(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(selectionExclusionFixtureYAML, []byte("declared_rows:"), []byte("unknown_field: true\ndeclared_rows:"), 1)
	if _, err := decodeSelectionExclusionFixtures(mutated); err == nil || !strings.Contains(err.Error(), "field unknown_field") {
		t.Fatalf("strict exact exclusion fixture mutation error = %v, want unknown-field rejection", err)
	}
}

func countSelectionExclusions(selection config.SelectionConfig) (int, int) {
	branchCount := 0
	sessionCount := 0
	for _, harness := range selection.Harnesses {
		branchCount += len(harness.Exclusions.Branches)
		sessionCount += len(harness.Exclusions.Sessions)
	}
	return branchCount, sessionCount
}
