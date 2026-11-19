package store_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/metrics_duration_roundtrip.yaml
var durationFixtureYAML []byte

//go:embed testdata/metrics_duration_roundtrip.manifest.yaml
var durationFixtureManifestYAML []byte

// This external test validates the same fixture consumed by the store's
// white-box generation test without introducing a store -> testutil cycle.
func TestMetricsDurationFixtureManifest(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name         string  `yaml:"name"`
			Minutes      float64 `yaml:"minutes"`
			Milliseconds int64   `yaml:"milliseconds"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := testutil.DecodeFixtureYAML(durationFixtureYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := testutil.DecodeFixtureYAML(durationFixtureManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate duration fixture %q", row.Name)
		}
		seen[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("metrics_duration_roundtrip.yaml", "cases", manifest.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
}
