package codemap_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/clamp_limit.yaml
var clampLimitYAML []byte

//go:embed testdata/clamp_limit.manifest.yaml
var clampLimitManifestYAML []byte

type clampLimitCase struct {
	Name  string `yaml:"name"`
	Input int    `yaml:"input"`
	Want  int    `yaml:"want"`
}

type clampLimitFamily struct {
	Cases []clampLimitCase `yaml:"cases"`
}

// TestClampSearchLimitFixtures pins the one page-size rule every search consumer
// shares. Deleting either arm of the rule — the default or the cap — fails here.
func TestClampSearchLimitFixtures(t *testing.T) {
	t.Parallel()
	var family clampLimitFamily
	if err := yaml.Unmarshal(clampLimitYAML, &family); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(family.Cases))
	for _, fixture := range family.Cases {
		names = append(names, fixture.Name)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(clampLimitManifestYAML, "clamp limit")
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "clamp limit"); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range family.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			if got := codemap.ClampSearchLimit(fixture.Input); got != fixture.Want {
				t.Fatalf("ClampSearchLimit(%d) = %d, want %d", fixture.Input, got, fixture.Want)
			}
		})
	}
}

// TestSearchPageWindowFitsTheCap keeps the grouped view's read window inside the
// range the ranked reader accepts; a window above the cap fails every grouped
// search at the reader's guard.
func TestSearchPageWindowFitsTheCap(t *testing.T) {
	t.Parallel()
	if codemap.SearchHTTPMaxLimit > codemap.SearchMaxLimit {
		t.Fatalf("documented HTTP cap %d exceeds the engine cap %d", codemap.SearchHTTPMaxLimit, codemap.SearchMaxLimit)
	}
	if codemap.SearchDefaultLimit > codemap.SearchMaxLimit {
		t.Fatalf("default page %d exceeds the cap %d", codemap.SearchDefaultLimit, codemap.SearchMaxLimit)
	}
}
