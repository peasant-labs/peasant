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
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/selection_clone_paths.yaml
var selectionClonePathFixtureYAML []byte

//go:embed testdata/selection_clone_paths.manifest.yaml
var selectionClonePathManifestYAML []byte

type selectionClonePathFixtures struct {
	Cases []selectionClonePathFixture `yaml:"cases"`
}

type selectionClonePathFixture struct {
	Name                 string               `yaml:"name"`
	Document             string               `yaml:"document"`
	ExpectedMode         config.SelectionMode `yaml:"expected_mode"`
	ExpectedProjectCount int                  `yaml:"expected_project_count"`
	ExpectedClonePaths   []string             `yaml:"expected_clone_paths"`
	ExpectClonePathsKey  bool                 `yaml:"expect_clone_paths_key"`
}

func loadSelectionClonePathFixtures(t *testing.T) selectionClonePathFixtures {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(selectionClonePathFixtureYAML))
	decoder.KnownFields(true)
	var fixtures selectionClonePathFixtures
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode clone-path configuration fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("clone-path configuration fixture must contain exactly one YAML document: %v", err)
	}
	seen := make(map[string]struct{}, len(fixtures.Cases))
	names := make([]string, 0, len(fixtures.Cases))
	for index, fixture := range fixtures.Cases {
		if strings.TrimSpace(fixture.Name) == "" || strings.TrimSpace(fixture.Document) == "" {
			t.Fatalf("clone-path configuration fixture row %d needs a name and document", index)
		}
		if _, duplicate := seen[fixture.Name]; duplicate {
			t.Fatalf("clone-path configuration fixture repeats name %q", fixture.Name)
		}
		seen[fixture.Name] = struct{}{}
		names = append(names, fixture.Name)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(selectionClonePathManifestYAML, "clone-path configuration")
	if err != nil {
		t.Fatalf("decode clone-path configuration manifest: %v", err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "clone-path configuration"); err != nil {
		t.Fatalf("clone-path configuration fixture/manifest mismatch: %v", err)
	}
	return fixtures
}

func TestSelectionClonePaths_CompatibilityAndRoundTrip(t *testing.T) {
	t.Parallel()
	fixtures := loadSelectionClonePathFixtures(t)
	for _, fixture := range fixtures.Cases {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewMemFS()
			const path = "/config/config.yaml"
			original := []byte(fixture.Document)
			if err := fs.WriteFile(path, original, 0o600); err != nil {
				t.Fatalf("write configuration fixture: %v", err)
			}

			loaded, err := config.Load(path, fs, nil)
			if err != nil {
				t.Fatalf("load configuration fixture: %v", err)
			}
			afterLoad, err := fs.ReadFile(path)
			if err != nil {
				t.Fatalf("read configuration after load: %v", err)
			}
			if !bytes.Equal(afterLoad, original) {
				t.Fatal("loading the configuration changed its bytes before the user committed a new selection")
			}
			if loaded.Selection.Mode != fixture.ExpectedMode {
				t.Fatalf("selection mode = %q, want %q", loaded.Selection.Mode, fixture.ExpectedMode)
			}

			projects := allSelectionProjects(loaded.Selection)
			if len(projects) != fixture.ExpectedProjectCount {
				t.Fatalf("project count = %d, want %d", len(projects), fixture.ExpectedProjectCount)
			}
			var gotClonePaths []string
			if len(projects) == 1 {
				gotClonePaths = projects[0].ClonePaths
			}
			if !slices.Equal(gotClonePaths, fixture.ExpectedClonePaths) {
				t.Fatalf("clone paths = %v, want %v", gotClonePaths, fixture.ExpectedClonePaths)
			}

			encoded, err := yaml.Marshal(loaded.Selection)
			if err != nil {
				t.Fatalf("marshal selection after load: %v", err)
			}
			if strings.Contains(string(encoded), "clonePaths:") != fixture.ExpectClonePathsKey {
				t.Fatalf("marshaled selection clonePaths presence = %v, want %v\n%s", strings.Contains(string(encoded), "clonePaths:"), fixture.ExpectClonePathsKey, encoded)
			}
			roundTripped, err := decodeSelectionStrict(encoded)
			if err != nil {
				t.Fatalf("decode marshaled selection: %v", err)
			}
			if !reflect.DeepEqual(roundTripped, loaded.Selection) {
				t.Fatalf("selection changed during YAML round trip:\nloaded: %+v\nround trip: %+v", loaded.Selection, roundTripped)
			}
		})
	}
}

func allSelectionProjects(selection config.SelectionConfig) []config.ProjectSelection {
	var projects []config.ProjectSelection
	for _, harness := range selection.Harnesses {
		projects = append(projects, harness.Projects...)
	}
	return projects
}

func decodeSelectionStrict(data []byte) (config.SelectionConfig, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var selection config.SelectionConfig
	if err := decoder.Decode(&selection); err != nil {
		return config.SelectionConfig{}, fmt.Errorf("decode selection: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return config.SelectionConfig{}, fmt.Errorf("selection must contain exactly one YAML document: %v", err)
	}
	return selection, nil
}
