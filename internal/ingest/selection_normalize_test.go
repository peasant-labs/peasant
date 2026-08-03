package ingest_test

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

// Fixture-backed coverage for the two selection-matching
// normalizers introduced to fix the real-data regression (config remotes in
// SSH/SCP form vs the projects table's normalized bare form; config project
// names vs the store's path-shaped "project name"). See
// testdata/selection_normalize.yaml for the case data and its provenance.

//go:embed testdata/selection_normalize.yaml
var selectionNormalizeFixtureBytes []byte

//go:embed testdata/selection_normalize_manifest.yaml
var selectionNormalizeManifestBytes []byte

type selectionNormalizeFixture struct {
	RemoteCases []normalizeCase `yaml:"remoteCases"`
	NameCases   []normalizeCase `yaml:"nameCases"`
}

type normalizeCase struct {
	Name  string `yaml:"name"`
	Input string `yaml:"input"`
	Want  string `yaml:"want"`
}

// selectionNormalizeManifest is an INDEPENDENT redundant inventory of the
// fixture's case names and counts (mirroring the exact-manifest pattern
// internal/codemap/testdata/project_resolution_manifest.yaml already uses):
// a count-preserving swap that silently drops a real case and adds a filler
// with the same name shape is invisible to a bare `len(cases) > 0` guard,
// but not to a manifest that names every required case independently.
type selectionNormalizeManifest struct {
	ExpectedRemoteCaseCount int      `yaml:"expectedRemoteCaseCount"`
	RequiredRemoteNames     []string `yaml:"requiredRemoteNames"`
	ExpectedNameCaseCount   int      `yaml:"expectedNameCaseCount"`
	RequiredNameNames       []string `yaml:"requiredNameNames"`
}

func loadSelectionNormalizeManifest(data []byte) (selectionNormalizeManifest, error) {
	var manifest selectionNormalizeManifest
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return selectionNormalizeManifest{}, fmt.Errorf("decode selection-normalize manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return selectionNormalizeManifest{}, fmt.Errorf("selection-normalize manifest must contain exactly one YAML document: %v", err)
	}
	return manifest, nil
}

func loadSelectionNormalizeFixture(data []byte) (selectionNormalizeFixture, error) {
	var fixture selectionNormalizeFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return selectionNormalizeFixture{}, fmt.Errorf("decode selection-normalize fixture first document: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return selectionNormalizeFixture{}, fmt.Errorf("selection-normalize fixture must contain exactly one YAML document: %v", err)
	}
	if len(fixture.RemoteCases) == 0 || len(fixture.NameCases) == 0 {
		return selectionNormalizeFixture{}, fmt.Errorf("selection-normalize fixture must have at least one remoteCases row and one nameCases row, got %d/%d", len(fixture.RemoteCases), len(fixture.NameCases))
	}
	seen := make(map[string]struct{}, len(fixture.RemoteCases)+len(fixture.NameCases))
	for _, group := range [][]normalizeCase{fixture.RemoteCases, fixture.NameCases} {
		for _, c := range group {
			if c.Name == "" {
				return selectionNormalizeFixture{}, fmt.Errorf("selection-normalize fixture has a case with an empty name")
			}
			if _, dup := seen[c.Name]; dup {
				return selectionNormalizeFixture{}, fmt.Errorf("selection-normalize fixture repeats case name %q", c.Name)
			}
			seen[c.Name] = struct{}{}
		}
	}

	manifest, err := loadSelectionNormalizeManifest(selectionNormalizeManifestBytes)
	if err != nil {
		return selectionNormalizeFixture{}, err
	}
	if err := validateAgainstManifest(fixture.RemoteCases, manifest.ExpectedRemoteCaseCount, manifest.RequiredRemoteNames, "remoteCases"); err != nil {
		return selectionNormalizeFixture{}, err
	}
	if err := validateAgainstManifest(fixture.NameCases, manifest.ExpectedNameCaseCount, manifest.RequiredNameNames, "nameCases"); err != nil {
		return selectionNormalizeFixture{}, err
	}
	return fixture, nil
}

// validateAgainstManifest requires an EXACT, unique match between cases and
// the manifest's independently authored name/count inventory for one group.
func validateAgainstManifest(cases []normalizeCase, expectedCount int, requiredNames []string, group string) error {
	if len(cases) != expectedCount || len(requiredNames) != expectedCount {
		return fmt.Errorf("selection-normalize %s: fixture has %d case(s), manifest expects %d", group, len(cases), expectedCount)
	}
	names := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		names[c.Name] = struct{}{}
	}
	if len(names) != len(cases) {
		return fmt.Errorf("selection-normalize %s: fixture repeats a case name", group)
	}
	required := make(map[string]struct{}, len(requiredNames))
	for _, name := range requiredNames {
		if name == "" {
			return fmt.Errorf("selection-normalize %s: manifest has an empty required name", group)
		}
		if _, dup := required[name]; dup {
			return fmt.Errorf("selection-normalize %s: manifest repeats required name %q", group, name)
		}
		required[name] = struct{}{}
		if _, ok := names[name]; !ok {
			return fmt.Errorf("selection-normalize %s: fixture is missing required family %q", group, name)
		}
	}
	for name := range names {
		if _, ok := required[name]; !ok {
			return fmt.Errorf("selection-normalize %s: fixture has case %q that is not in the manifest's required names", group, name)
		}
	}
	return nil
}

func TestNormalizeRemoteForMatch_Fixture(t *testing.T) {
	t.Parallel()
	fixture, err := loadSelectionNormalizeFixture(selectionNormalizeFixtureBytes)
	if err != nil {
		t.Fatalf("load selection-normalize fixture: %v", err)
	}
	for _, tc := range fixture.RemoteCases {
		t.Run(tc.Name, func(t *testing.T) {
			got := ingest.NormalizeRemoteForMatch(tc.Input)
			if got != tc.Want {
				t.Errorf("NormalizeRemoteForMatch(%q) = %q, want %q", tc.Input, got, tc.Want)
			}
		})
	}
}

func TestNormalizeRemoteForMatch_SSHConfigAndNormalizedStoredFormsAgree(t *testing.T) {
	t.Parallel()
	fixture, err := loadSelectionNormalizeFixture(selectionNormalizeFixtureBytes)
	if err != nil {
		t.Fatalf("load selection-normalize fixture: %v", err)
	}
	// The real bug: an SSH/SCP config form and the already-normalized bare
	// stored form for the SAME repo must land on the identical canonical
	// value, or the matcher can never bridge the two. Cross-check every pair
	// that names the same repo (by "want").
	byWant := make(map[string][]string)
	for _, tc := range fixture.RemoteCases {
		if tc.Want == "" {
			continue
		}
		byWant[tc.Want] = append(byWant[tc.Want], tc.Input)
	}
	for want, inputs := range byWant {
		if len(inputs) < 2 {
			continue
		}
		for _, in := range inputs {
			if got := ingest.NormalizeRemoteForMatch(in); got != want {
				t.Errorf("form %q normalized to %q, want the shared canonical form %q", in, got, want)
			}
		}
	}
}

func TestNormalizeProjectNameForMatch_Fixture(t *testing.T) {
	t.Parallel()
	fixture, err := loadSelectionNormalizeFixture(selectionNormalizeFixtureBytes)
	if err != nil {
		t.Fatalf("load selection-normalize fixture: %v", err)
	}
	for _, tc := range fixture.NameCases {
		t.Run(tc.Name, func(t *testing.T) {
			got := ingest.NormalizeProjectNameForMatch(tc.Input)
			if got != tc.Want {
				t.Errorf("NormalizeProjectNameForMatch(%q) = %q, want %q", tc.Input, got, tc.Want)
			}
		})
	}
}

func TestSelectionNormalizeFixtureLoaderRejectsStructuralDrift(t *testing.T) {
	t.Parallel()
	unknownField := bytes.Replace(selectionNormalizeFixtureBytes, []byte("remoteCases:"), []byte("remoteCases:\n  - unexpected: true"), 1)
	if _, err := loadSelectionNormalizeFixture(unknownField); err == nil {
		t.Fatal("expected an unknown-field mutation to be rejected")
	}
	duplicateName := bytes.Replace(selectionNormalizeFixtureBytes, []byte("name: empty_name_stays_empty"), []byte("name: scp_form_garden_app"), 1)
	if _, err := loadSelectionNormalizeFixture(duplicateName); err == nil {
		t.Fatal("expected a duplicate-name mutation to be rejected")
	}

	// The exact-manifest guard: a count-PRESERVING swap that silently drops a
	// real remote case and adds a filler with a name the manifest doesn't
	// know about must be rejected — a bare len()>0 check (what this fixture
	// had before) cannot catch this, since it doesn't change the count.
	swappedRemoteCase := bytes.Replace(selectionNormalizeFixtureBytes, []byte("name: scp_form_garden_app"), []byte("name: replacement_case"), 1)
	if _, err := loadSelectionNormalizeFixture(swappedRemoteCase); err == nil {
		t.Fatal("expected a count-preserving remote-case swap to be rejected by the manifest guard")
	}
	swappedNameCase := bytes.Replace(selectionNormalizeFixtureBytes, []byte("name: short_name_passthrough"), []byte("name: replacement_case"), 1)
	if _, err := loadSelectionNormalizeFixture(swappedNameCase); err == nil {
		t.Fatal("expected a count-preserving name-case swap to be rejected by the manifest guard")
	}

	unknownManifestField := bytes.Replace(selectionNormalizeManifestBytes, []byte("expectedRemoteCaseCount:"), []byte("unexpected: true\nexpectedRemoteCaseCount:"), 1)
	if _, err := loadSelectionNormalizeManifest(unknownManifestField); err == nil {
		t.Fatal("expected an unknown-field manifest mutation to be rejected")
	}
	trailingManifestDoc := append(append([]byte{}, selectionNormalizeManifestBytes...), []byte("\n---\nextra: true\n")...)
	if _, err := loadSelectionNormalizeManifest(trailingManifestDoc); err == nil {
		t.Fatal("expected a trailing-document manifest mutation to be rejected")
	}
}
