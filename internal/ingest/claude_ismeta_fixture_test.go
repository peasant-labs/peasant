package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/claude_ismeta.yaml
var claudeIsMetaFixtureYAML []byte

//go:embed testdata/claude_ismeta.manifest.yaml
var claudeIsMetaManifestYAML []byte

// claudeIsMetaReclassifiedCase is a user-role entry with "isMeta":true. The
// test wraps Content in a transcript line.
type claudeIsMetaReclassifiedCase struct {
	Name    string `yaml:"name"`
	Content string `yaml:"content"`
}

// claudeIsMetaStaysUserCase is a complete transcript line whose isMeta is
// absent or false.
type claudeIsMetaStaysUserCase struct {
	Name string `yaml:"name"`
	Line string `yaml:"line"`
}

type claudeIsMetaFixture struct {
	Reclassified []claudeIsMetaReclassifiedCase `yaml:"reclassified"`
	StaysUser    []claudeIsMetaStaysUserCase    `yaml:"staysUser"`
}

func (f claudeIsMetaFixture) names() []string {
	names := make([]string, 0, len(f.Reclassified)+len(f.StaysUser))
	for _, c := range f.Reclassified {
		names = append(names, c.Name)
	}
	for _, c := range f.StaysUser {
		names = append(names, c.Name)
	}
	return names
}

func decodeClaudeIsMetaFixture(data []byte) (claudeIsMetaFixture, error) {
	var fixture claudeIsMetaFixture
	if err := testutil.DecodeFixtureYAML(data, &fixture); err != nil {
		return claudeIsMetaFixture{}, fmt.Errorf("decode Claude isMeta fixture: %w", err)
	}
	seen := make(map[string]struct{}, len(fixture.Reclassified)+len(fixture.StaysUser))
	for _, name := range fixture.names() {
		if name == "" {
			return claudeIsMetaFixture{}, errors.New("Claude isMeta fixture has a case with an empty name")
		}
		if _, duplicate := seen[name]; duplicate {
			return claudeIsMetaFixture{}, fmt.Errorf("Claude isMeta fixture repeats case name %q", name)
		}
		seen[name] = struct{}{}
	}
	for _, c := range fixture.Reclassified {
		if c.Content == "" {
			return claudeIsMetaFixture{}, fmt.Errorf("Claude isMeta fixture reclassified case %q has no content", c.Name)
		}
	}
	for _, c := range fixture.StaysUser {
		var probe map[string]any
		if err := json.Unmarshal([]byte(c.Line), &probe); err != nil {
			return claudeIsMetaFixture{}, fmt.Errorf("Claude isMeta fixture staysUser case %q line is invalid JSON: %w", c.Name, err)
		}
	}
	return fixture, nil
}

// validateClaudeIsMetaNames fails when a manifest name is missing from the
// fixture. Extra fixture cases are allowed.
func validateClaudeIsMetaNames(manifest testutil.RequiredNamesManifest, fixture claudeIsMetaFixture) error {
	present := make(map[string]bool)
	for _, name := range fixture.names() {
		present[name] = true
	}
	return testutil.RequireFixtureNames("Claude isMeta fixture", "case", manifest.RequiredNames, present)
}

func loadClaudeIsMetaFixture(t *testing.T) claudeIsMetaFixture {
	t.Helper()
	fixture, err := decodeClaudeIsMetaFixture(claudeIsMetaFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeIsMetaManifestYAML, "Claude isMeta")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateClaudeIsMetaNames(manifest, fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestClaudeIsMetaFixtureGuards proves the manifest check bites: renaming any
// required case in the fixture must fail validation.
func TestClaudeIsMetaFixtureGuards(t *testing.T) {
	t.Parallel()
	loadClaudeIsMetaFixture(t)
	manifest, err := testutil.DecodeRequiredNamesManifest(claudeIsMetaManifestYAML, "Claude isMeta")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(claudeIsMetaFixtureYAML, []byte("name: "+required+"\n"), []byte("name: replacement case\n"), 1)
		if bytes.Equal(mutated, claudeIsMetaFixtureYAML) {
			t.Fatalf("required case %q not found as a name line in the fixture", required)
		}
		fixture, err := decodeClaudeIsMetaFixture(mutated)
		if err != nil {
			t.Fatalf("required case %q replacement unexpectedly failed to decode: %v", required, err)
		}
		if err := validateClaudeIsMetaNames(manifest, fixture); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}
