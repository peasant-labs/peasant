package mdrender

import (
	"embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/placeholders*.yaml
var placeholderFixtures embed.FS

type placeholderCase struct {
	Name  string `yaml:"name"`
	Input string `yaml:"input"`
	Want  string `yaml:"want"`
}

func loadPlaceholderFixtures(t *testing.T) []placeholderCase {
	t.Helper()
	data, err := placeholderFixtures.ReadFile("testdata/placeholders.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Cases []placeholderCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(data, &doc); err != nil {
		t.Fatal(err)
	}
	data, err = placeholderFixtures.ReadFile("testdata/placeholders.manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, "placeholders")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range doc.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "placeholders"); err != nil {
		t.Fatal(err)
	}
	return doc.Cases
}

// The renderer decodes these entities again; test the production preprocessing
// boundary to observe capture expansion rather than only its final visible text.
func TestPreservePlaceholdersExpandsCapture(t *testing.T) {
	t.Parallel()
	for _, c := range loadPlaceholderFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if got := preservePlaceholders(c.Input); got != c.Want {
				t.Fatalf("preservePlaceholders(%q) = %q, want %q", c.Input, got, c.Want)
			}
		})
	}
}
