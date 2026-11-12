package harnesslayout_test

import (
	_ "embed"
	"reflect"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/layout_only.yaml
var layoutOnlyYAML []byte

//go:embed testdata/layout_only.manifest.yaml
var layoutOnlyManifestYAML []byte

func TestLayoutOnly(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Cases []struct {
			Name    string `yaml:"name"`
			Layouts []struct {
				Tool    harnesslayout.Tool `yaml:"tool"`
				Harness schema.Harness     `yaml:"harness"`
			} `yaml:"layouts"`
			Harvestable []schema.Harness `yaml:"harvestable"`
			Want        []schema.Harness `yaml:"want"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(layoutOnlyYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(layoutOnlyManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || seen[tc.Name] {
			t.Fatalf("empty or duplicate layout-only case %q", tc.Name)
		}
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("layout_only", "case", manifest.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			layouts := make([]harnesslayout.Layout, 0, len(tc.Layouts))
			for _, l := range tc.Layouts {
				layouts = append(layouts, harnesslayout.Layout{Tool: l.Tool, Harness: l.Harness})
			}
			beforeLayouts := slices.Clone(layouts)
			beforeHarvestable := slices.Clone(tc.Harvestable)
			got := harnesslayout.LayoutOnly(layouts, tc.Harvestable)
			if !slices.Equal(got, tc.Want) {
				t.Errorf("LayoutOnly = %v, want %v", got, tc.Want)
			}
			if !reflect.DeepEqual(layouts, beforeLayouts) || !slices.Equal(tc.Harvestable, beforeHarvestable) {
				t.Error("LayoutOnly changed its inputs")
			}
		})
	}
}
