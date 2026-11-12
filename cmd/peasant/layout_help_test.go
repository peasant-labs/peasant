package main

import (
	"bytes"
	_ "embed"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/layout_help.yaml
var layoutHelpYAML []byte

//go:embed testdata/layout_help.manifest.yaml
var layoutHelpManifestYAML []byte

func TestMountedLayoutHelp(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Cases []struct {
			Name    string `yaml:"name"`
			Harness string `yaml:"harness"`
			Present bool   `yaml:"present"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(layoutHelpYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(layoutHelpManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || seen[tc.Name] || tc.Harness == "" {
			t.Fatalf("invalid or duplicate layout help case %q", tc.Name)
		}
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("layout_help", "case", manifest.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	root := buildRootCommand()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs([]string{"layout", "--help"})
	if err := root.Execute(); err != nil {
		t.Fatalf("layout --help: %v; stderr: %s", err, stderr.String())
	}
	const prefix = "Layout-only harnesses (no adapter): "
	_, rest, ok := strings.Cut(stdout.String(), prefix)
	if !ok {
		t.Fatalf("layout help is missing the layout-only list:\n%s", stdout.String())
	}
	line, _, _ := strings.Cut(rest, "\n")
	values := strings.Split(strings.TrimSuffix(line, "."), ", ")
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := slices.Contains(values, tc.Harness); got != tc.Present {
				t.Errorf("help tokens %v contain %q = %t, want %t", values, tc.Harness, got, tc.Present)
			}
		})
	}
	_, rows := documentedHarnessRoster(t)
	if !slices.Equal(values, rows["layout-only"]) {
		t.Errorf("help tokens = %v, documented layout-only roster = %v", values, rows["layout-only"])
	}
}
