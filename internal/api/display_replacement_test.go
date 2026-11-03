package api

import (
	"embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/display_replacement*.yaml
var displayReplacementFixtures embed.FS

type displayReplacementCase struct {
	Name  string `yaml:"name"`
	Input string `yaml:"input"`
	Want  string `yaml:"want"`
}

func loadDisplayReplacementFixtures(t *testing.T) []displayReplacementCase {
	t.Helper()
	data, err := displayReplacementFixtures.ReadFile("testdata/display_replacement.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []displayReplacementCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(data, &doc); err != nil {
		t.Fatal(err)
	}
	data, err = displayReplacementFixtures.ReadFile("testdata/display_replacement.manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "display replacement")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range doc.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "display replacement"); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

func TestDisplayReplacementStripsBackrefs(t *testing.T) {
	t.Parallel()
	for _, c := range loadDisplayReplacementFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if got := displayReplacement(c.Input); got != c.Want {
				t.Fatalf("displayReplacement(%q) = %q, want %q", c.Input, got, c.Want)
			}
		})
	}
}
