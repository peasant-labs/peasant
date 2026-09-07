package main

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvest_harness_resolution.yaml
var harvestHarnessResolutionYAML []byte

type harvestHarnessResolutionFixture struct {
	Name          string           `yaml:"name"`
	Raw           string           `yaml:"raw"`
	Want          defaults.Harness `yaml:"want"`
	ErrorContains []string         `yaml:"error_contains"`
}

func LoadHarvestHarnessResolutionFixtures(t testing.TB) []harvestHarnessResolutionFixture {
	t.Helper()
	var document struct {
		RequiredNames []string                          `yaml:"required_names"`
		Cases         []harvestHarnessResolutionFixture `yaml:"cases"`
	}
	if err := yaml.Unmarshal(harvestHarnessResolutionYAML, &document); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, fixture := range document.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate harness resolution fixture name %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	if err := testutil.RequireFixtureNames("harvest harness resolution", "case", document.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func TestResolveHarnessFlag(t *testing.T) {
	t.Parallel()
	for _, fixture := range LoadHarvestHarnessResolutionFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			got, err := resolveHarnessFlag(fixture.Raw)
			if len(fixture.ErrorContains) > 0 {
				if err == nil {
					t.Fatalf("resolveHarnessFlag(%q): expected error, got %q", fixture.Raw, got)
				}
				for _, want := range fixture.ErrorContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q should contain %q", err.Error(), want)
					}
				}
				return
			}
			if err != nil || got != fixture.Want {
				t.Fatalf("resolveHarnessFlag(%q) = %q, %v; want %q", fixture.Raw, got, err, fixture.Want)
			}
		})
	}
}
