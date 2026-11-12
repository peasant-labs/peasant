package main

import (
	"bytes"
	_ "embed"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harness_roster.yaml
var harnessRosterYAML []byte

//go:embed testdata/harness_roster.manifest.yaml
var harnessRosterManifestYAML []byte

type harnessRosterCase struct {
	Name     string                `yaml:"name"`
	Owner    string                `yaml:"owner"`
	Disjoint *bool                 `yaml:"disjoint"`
	SubsetOf []harnessRosterSubset `yaml:"subsetOf"`
}

type harnessRosterSubset struct {
	Label  string `yaml:"label"`
	Reason string `yaml:"reason"`
}

func documentedHarnessRoster(t *testing.T) ([]string, map[string][]string) {
	t.Helper()
	data, err := os.ReadFile("../../docs/bestiary.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	const begin = "<!-- harness-sets:begin -->"
	const end = "<!-- harness-sets:end -->"
	if strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
		t.Fatal("expected one marked harness roster")
	}
	_, rest, _ := strings.Cut(text, begin)
	block, _, ok := strings.Cut(rest, end)
	if !ok {
		t.Fatal("roster end must follow its beginning")
	}
	var labels []string
	rows := make(map[string][]string)
	for _, row := range strings.Split(strings.TrimSpace(block), "\n") {
		label, values, ok := strings.Cut(row, ": ")
		if !ok || label == "" || values == "" {
			t.Fatalf("invalid roster line %q", row)
		}
		if _, exists := rows[label]; exists {
			t.Fatalf("duplicate roster label %q", label)
		}
		labels = append(labels, label)
		rows[label] = strings.Split(values, ", ")
		unique := make(map[string]bool)
		for _, value := range rows[label] {
			if value == "" || unique[value] {
				t.Fatalf("roster %q has an empty or duplicate value %q", label, value)
			}
			unique[value] = true
		}
	}
	return labels, rows
}

func TestDocumentedHarnessRoster(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Cases []harnessRosterCase `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(harnessRosterYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(harnessRosterManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" || seen[tc.Name] {
			t.Fatalf("empty or duplicate roster case %q", tc.Name)
		}
		if tc.Disjoint == nil {
			t.Fatalf("roster case %q must explicitly declare disjoint", tc.Name)
		}
		if tc.SubsetOf == nil {
			t.Fatalf("roster case %q must explicitly declare subsetOf", tc.Name)
		}
		seen[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("harness_roster", "case", manifest.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	labels, rows := documentedHarnessRoster(t)
	if !slices.Equal(labels, manifest.RequiredNames) {
		t.Fatalf("roster labels = %v, want exactly %v in order", labels, manifest.RequiredNames)
	}
	for _, tc := range fixture.Cases {
		if _, ok := rows[tc.Name]; !ok {
			t.Fatalf("roster case %q has no documented line", tc.Name)
		}
		want, ordered := rosterOwner(t, tc.Owner)
		got := slices.Clone(rows[tc.Name])
		if !ordered {
			slices.Sort(got)
			slices.Sort(want)
		}
		if !slices.Equal(got, want) {
			t.Errorf("roster %s = %v, owner %s = %v; exclude any new test-only layout by its tool name, not its harness", tc.Name, got, tc.Owner, want)
		}
	}
	for i, tc := range fixture.Cases {
		seenTargets := make(map[string]bool, len(tc.SubsetOf))
		for _, subset := range tc.SubsetOf {
			target, ok := rows[subset.Label]
			if !ok || subset.Label == tc.Name || seenTargets[subset.Label] {
				t.Fatalf("roster case %q has an unknown, self-referential or duplicate subset target %q", tc.Name, subset.Label)
			}
			seenTargets[subset.Label] = true
			for _, value := range rows[tc.Name] {
				if !slices.Contains(target, value) {
					if subset.Reason == "" {
						t.Errorf("%s value %q must be in %s", tc.Name, value, subset.Label)
					} else {
						t.Errorf("%s value %q must be in %s: %s", tc.Name, value, subset.Label, subset.Reason)
					}
				}
			}
		}
		if !*tc.Disjoint {
			continue
		}
		for _, other := range fixture.Cases[i+1:] {
			if !*other.Disjoint {
				continue
			}
			for _, value := range rows[tc.Name] {
				if slices.Contains(rows[other.Name], value) {
					t.Errorf("%q is both %s and %s", value, tc.Name, other.Name)
				}
			}
		}
	}
}

func rosterOwner(t *testing.T, owner string) ([]string, bool) {
	t.Helper()
	var harnesses []schema.Harness
	ordered := true
	switch owner {
	case "schema-harnesses":
		harnesses = schema.Harnesses()
	case "schema-declared":
		harnesses = schema.AllHarnesses
	case "adapter-registry":
		ordered = false
		for harness := range ingest.DefaultAdapterRegistry {
			harnesses = append(harnesses, harness)
		}
	case "layout-minus-adapters":
		ordered = false
		var layouts []harnesslayout.Layout
		for _, layout := range harnesslayout.Layouts() {
			if layout.Tool != layoutTestTool && layout.Tool != layoutFailTool {
				layouts = append(layouts, layout)
			}
		}
		var harvestable []schema.Harness
		for harness := range ingest.DefaultAdapterRegistry {
			harvestable = append(harvestable, harness)
		}
		harnesses = harnesslayout.LayoutOnly(layouts, harvestable)
	case "archived-gemini":
		harnesses = []schema.Harness{schema.HarnessGeminiCLI}
	default:
		t.Fatalf("unknown harness roster owner %q", owner)
	}
	out := make([]string, 0, len(harnesses))
	for _, harness := range harnesses {
		out = append(out, harness.String())
	}
	return out, ordered
}
