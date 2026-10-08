package main

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

// Fixture scaffolding for the harmonized session content model CLI surface
// (design llm/peasant--harmonized-content-model.md section 10, ratified
// revision 17). The command and flag inventory lives in
// testdata/cli/content_cli_surface.yaml with its required-name manifest in
// testdata/cli/content_cli_surface.manifest.yaml: deletion protection by
// required NAME, never by bare count. No cases are filled yet; every entry is
// a name placeholder so the manifest guards the inventory from the start. The
// later issues extend the case shape and this loader field-for-field.

//go:embed testdata/cli/content_cli_surface.yaml
var contentCLISurfaceYAML []byte

//go:embed testdata/cli/content_cli_surface.manifest.yaml
var contentCLISurfaceManifestYAML []byte

// contentCLISurfaceCase is the minimal case shape: a name only. Later issues
// add command, flags, and output-shape expectations beside it.
type contentCLISurfaceCase struct {
	Name string `yaml:"name"`
}

type contentCLISurfaceFixture struct {
	Cases []contentCLISurfaceCase `yaml:"cases"`
}

type contentCLISurfaceManifest struct {
	RequiredNames []string `yaml:"requiredNames"`
}

// loadContentCLISurfaceFixture strictly decodes the CLI surface scaffold and
// enforces its required-names manifest: every required name must be present,
// with no blank or duplicate entry.
func loadContentCLISurfaceFixture(t *testing.T) []string {
	t.Helper()
	var fixture contentCLISurfaceFixture
	if err := yaml.Unmarshal(contentCLISurfaceYAML, &fixture); err != nil {
		t.Fatalf("decode content_cli_surface.yaml: %v", err)
	}
	var manifest contentCLISurfaceManifest
	if err := yaml.Unmarshal(contentCLISurfaceManifestYAML, &manifest); err != nil {
		t.Fatalf("decode content_cli_surface manifest: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.Name == "" || present[c.Name] {
			t.Fatalf("content_cli_surface fixture has an empty or duplicate case name: %q", c.Name)
		}
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("content_cli_surface", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	return names
}

// TestContentCLISurfaceFixtureManifest pins the CLI surface case inventory:
// the loader compiles, the manifest loads, and every required name is present.
func TestContentCLISurfaceFixtureManifest(t *testing.T) {
	t.Parallel()
	loadContentCLISurfaceFixture(t)
}
