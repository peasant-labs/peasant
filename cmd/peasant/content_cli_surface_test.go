package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
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

// contentCLISurfaceCase is one CLI surface case: the section-10 name
// plus, for the migration cases, the production command path and the
// flags the runner resolves from the live command tree. The JSON cases
// also carry the output keys the machine-readable shapes must contain.
// Cases without a command stay name placeholders their owners fill.
type contentCLISurfaceCase struct {
	Name     string   `yaml:"name"`
	Command  string   `yaml:"command,omitempty"`
	Flags    []string `yaml:"flags,omitempty"`
	JSONKeys []string `yaml:"jsonKeys,omitempty"`
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
func loadContentCLISurfaceFixture(t *testing.T) []contentCLISurfaceCase {
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
	return fixture.Cases
}

// TestContentCLISurfaceFixtureManifest pins the CLI surface case inventory:
// the loader compiles, the manifest loads, and every required name is present.
func TestContentCLISurfaceFixtureManifest(t *testing.T) {
	t.Parallel()
	loadContentCLISurfaceFixture(t)
}

// assertCLISurfaceJSONKeys proves the case's JSON shape contains its
// required keys by rendering the production printer over synthetic
// input: the keys are pinned against the real output path, not a copy.
func assertCLISurfaceJSONKeys(t *testing.T, c contentCLISurfaceCase) {
	t.Helper()
	var decoded map[string]any
	switch c.Name {
	case "migrate-json":
		cmd := BuildMigrateCommand()
		var buf bytes.Buffer
		cmd.SetOut(&buf)
		result := store.MigrateResult{
			Rollbacks:     []store.MigrateRollback{},
			Warnings:      []string{},
			Preconditions: []store.RetirementPreconditionStatus{},
		}
		if err := writeMigrateResult(cmd, result, true, "/tmp/peasant.db"); err != nil {
			t.Fatalf("render migrate JSON: %v", err)
		}
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatalf("decode migrate JSON: %v\n%s", err, buf.String())
		}
	default:
		t.Fatalf("no JSON renderer for case %q", c.Name)
	}
	for _, key := range c.JSONKeys {
		if _, ok := decoded[key]; !ok {
			keys := make([]string, 0, len(decoded))
			for k := range decoded {
				keys = append(keys, k)
			}
			t.Errorf("migrate JSON misses key %q (has %s)", key, strings.Join(keys, ", "))
		}
	}
}

// TestContentCLISurfaceInventory resolves every cased command from the
// production command tree and proves its flags exist. The JSON cases
// additionally prove their output shapes contain the required keys.
func TestContentCLISurfaceInventory(t *testing.T) {
	t.Parallel()
	for _, c := range loadContentCLISurfaceFixture(t) {
		if c.Command == "" {
			continue
		}
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			root := buildRootCommand()
			target, _, err := root.Find(strings.Split(c.Command, " "))
			if err != nil || target == nil {
				t.Fatalf("cannot resolve `peasant %s`: %v", c.Command, err)
			}
			for _, flag := range c.Flags {
				name := flag
				if flag == defaults.JSONFlagName {
					name = defaults.JSONFlagName
				}
				if target.Flags().Lookup(name) == nil && target.PersistentFlags().Lookup(name) == nil {
					t.Errorf("`peasant %s` is missing --%s", c.Command, flag)
				}
			}
			if len(c.JSONKeys) > 0 {
				assertCLISurfaceJSONKeys(t, c)
			}
		})
	}
}
