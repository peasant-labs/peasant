package api

import (
	_ "embed"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/settings_destination_paths.yaml
var settingsDestinationPaths []byte

type destinationPathFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name             string   `yaml:"name"`
		Scope            string   `yaml:"scope"`
		WantModelPaths   []string `yaml:"wantModelPaths"`
		AbsentModelPaths []string `yaml:"absentModelPaths"`
		WantCatalogPaths []string `yaml:"wantCatalogPaths"`
	} `yaml:"cases"`
}

func TestSettingsDestinationPaths(t *testing.T) {
	var fixture destinationPathFixture
	if err := testutil.DecodeFixtureYAML(settingsDestinationPaths, &fixture); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	catalog, err := buildSettingCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range fixture.Cases {
		if names[row.Name] {
			t.Fatalf("duplicate destination path case %q", row.Name)
		}
		names[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			for _, required := range []string{"push.visibility", "redaction.level"} {
				if !slices.Contains(row.WantCatalogPaths, required) {
					t.Fatalf("fixture %s omits catalog path %s", row.Name, required)
				}
				if row.Scope == "full" && !slices.Contains(row.WantModelPaths, required) {
					t.Fatalf("fixture %s omits full-model path %s", row.Name, required)
				}
				if row.Scope == "selection" && !slices.Contains(row.AbsentModelPaths, required) {
					t.Fatalf("fixture %s omits excluded selection path %s", row.Name, required)
				}
			}
			var scope onboarding.ModelScope
			switch row.Scope {
			case "full":
				scope = onboarding.ScopeFull
			case "selection":
				scope = onboarding.ScopeSelection
			default:
				t.Fatalf("unknown scope %q", row.Scope)
			}
			model := onboarding.BuildModel(onboarding.Options{Scope: scope})
			paths := map[string]bool{}
			for _, field := range model.FieldPaths() {
				if field.Destination == onboarding.DestConfig {
					paths[strings.Join(field.Path, ".")] = true
				}
			}
			for _, path := range row.WantModelPaths {
				if !paths[path] {
					t.Errorf("model lacks %s", path)
				}
			}
			for _, path := range row.AbsentModelPaths {
				if paths[path] {
					t.Errorf("selection model unexpectedly contains %s", path)
				}
			}
			keys := peasantConfigKeys(catalog)
			for _, path := range row.WantCatalogPaths {
				if !keys[path] {
					t.Errorf("catalog lacks %s", path)
				}
			}
		})
	}
	if err := testutil.RequireFixtureNames("settings_destination_paths.yaml", "case", fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"disconnected-full-model", "selection-scope-still-uses-full-catalog"} {
		if !names[name] {
			t.Fatalf("missing required case %s", name)
		}
	}
}
