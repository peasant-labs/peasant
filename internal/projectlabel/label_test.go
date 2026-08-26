package projectlabel_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/schema/testcase"
	"github.com/peasant-labs/schema/testcase/assert"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/label_cases.yaml
var labelCasesYAML []byte

//go:embed testdata/label_manifest.yaml
var labelManifestYAML []byte

type labelInput struct {
	Remote   string `yaml:"remote"`
	Fallback string `yaml:"fallback"`
}

type labelExpected struct {
	Label string `yaml:"label"`
}

// labelFixtureManifest is the deletion-protection inventory for the
// projectlabel corpus: every name it lists must be present in the corpus, so
// a deleted row (rather than a renamed or replaced one) is caught even
// though the corpus is otherwise free to grow. It deliberately carries no
// case-count field: count and minimum guards are banned this epoch.
type labelFixtureManifest struct {
	RequiredCaseNames []string
}

type labelFixtureManifestYAML struct {
	RequiredCaseNames []labelManifestCaseName `yaml:"requiredCaseNames"`
}

// labelManifestCaseName rejects a non-string requiredCaseNames entry at
// decode time instead of silently coercing it.
type labelManifestCaseName string

func (name *labelManifestCaseName) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
		return fmt.Errorf("required case name must be a YAML string")
	}
	*name = labelManifestCaseName(value.Value)
	return nil
}

func loadLabelCorpus(t *testing.T) testcase.Corpus[labelInput, labelExpected] {
	t.Helper()
	corpus, _, err := loadLabelFixturesFromYAML(labelCasesYAML, labelManifestYAML)
	if err != nil {
		t.Fatalf("load projectlabel fixture and inventory: %v", err)
	}
	assert.RequireValid(t, corpus)
	return corpus
}

func loadLabelFixturesFromYAML(corpusYAML, manifestYAML []byte) (testcase.Corpus[labelInput, labelExpected], labelFixtureManifest, error) {
	corpus, err := testcase.LoadCorpus[labelInput, labelExpected](corpusYAML)
	if err != nil {
		return testcase.Corpus[labelInput, labelExpected]{}, labelFixtureManifest{}, fmt.Errorf("decode projectlabel corpus: %w", err)
	}
	manifest, err := decodeLabelFixtureManifest(manifestYAML)
	if err != nil {
		return testcase.Corpus[labelInput, labelExpected]{}, labelFixtureManifest{}, err
	}
	if err := validateLabelFixtureInventory(corpus, manifest); err != nil {
		return testcase.Corpus[labelInput, labelExpected]{}, labelFixtureManifest{}, err
	}
	return corpus, manifest, nil
}

func decodeLabelFixtureManifest(data []byte) (labelFixtureManifest, error) {
	var decoded labelFixtureManifestYAML
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil {
		return labelFixtureManifest{}, fmt.Errorf("decode projectlabel manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return labelFixtureManifest{}, fmt.Errorf("decode trailing projectlabel manifest document: %w", err)
		}
		return labelFixtureManifest{}, fmt.Errorf("decode projectlabel manifest: multiple YAML documents are not allowed")
	}
	manifest := labelFixtureManifest{
		RequiredCaseNames: make([]string, len(decoded.RequiredCaseNames)),
	}
	for index, name := range decoded.RequiredCaseNames {
		manifest.RequiredCaseNames[index] = string(name)
	}
	if len(manifest.RequiredCaseNames) == 0 {
		return labelFixtureManifest{}, fmt.Errorf("projectlabel manifest has no requiredCaseNames; name at least one case the corpus must never drop")
	}
	seen := make(map[string]struct{}, len(manifest.RequiredCaseNames))
	for index, name := range manifest.RequiredCaseNames {
		if strings.TrimSpace(name) == "" {
			return labelFixtureManifest{}, fmt.Errorf("projectlabel manifest requiredCaseNames[%d] is blank", index)
		}
		if _, exists := seen[name]; exists {
			return labelFixtureManifest{}, fmt.Errorf("projectlabel manifest repeats required case name %q", name)
		}
		seen[name] = struct{}{}
	}
	return manifest, nil
}

// validateLabelFixtureInventory fails when a required case name is missing
// from the corpus (a deleted row) or the corpus itself is invalid. It
// deliberately does not compare a case count: the corpus may grow freely as
// long as every named case remains present.
func validateLabelFixtureInventory(corpus testcase.Corpus[labelInput, labelExpected], manifest labelFixtureManifest) error {
	if err := corpus.Validate(); err != nil {
		return fmt.Errorf("validate projectlabel corpus: %w", err)
	}
	present := make(map[string]struct{}, len(corpus.Cases))
	for _, c := range corpus.Cases {
		present[c.Name] = struct{}{}
	}
	for _, required := range manifest.RequiredCaseNames {
		if _, ok := present[required]; !ok {
			return fmt.Errorf("projectlabel corpus is missing required case %q named in the manifest; a case was deleted or renamed", required)
		}
	}
	return nil
}

// TestLabel_FixtureCorpus drives projectlabel.Label over every fixture case:
// bare canonical_remote forms across well-known and self-hosted/enterprise
// hosts (all rendered with their FULL hostname — there is no short-prefix
// table and no host allowlist), defensive HTTPS/SSH remote forms peasant
// does not normally store but should not choke on, a ported remote (the
// port is dropped by design), and the empty/malformed-remote fallback path.
// A missing remote must fall back to the caller's path/hash display value,
// never fail or return an empty label.
func TestLabel_FixtureCorpus(t *testing.T) {
	corpus := loadLabelCorpus(t)
	for _, fixtureCase := range corpus.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			got := projectlabel.Label(fixtureCase.Input.Remote, fixtureCase.Input.Fallback)
			if got != fixtureCase.Expected.Label {
				t.Errorf("Label(%q, %q) = %q, want %q", fixtureCase.Input.Remote, fixtureCase.Input.Fallback, got, fixtureCase.Expected.Label)
			}
		})
	}
}

// TestFromRemote_OkFlag directly exercises the ok return value that Label
// hides: FromRemote must report ok=false (not just an empty string) whenever
// the fixture corpus's fallback path is expected to win, so Label's fallback
// behavior is provably driven by FromRemote's failure signal rather than an
// incidental empty-string coincidence.
func TestFromRemote_OkFlag(t *testing.T) {
	corpus := loadLabelCorpus(t)
	for _, fixtureCase := range corpus.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			label, ok := projectlabel.FromRemote(fixtureCase.Input.Remote)
			fellBack := fixtureCase.Expected.Label == fixtureCase.Input.Fallback && fixtureCase.Input.Remote != fixtureCase.Expected.Label
			if fellBack && ok {
				t.Errorf("FromRemote(%q) = (%q, true), want ok=false so Label falls back to %q", fixtureCase.Input.Remote, label, fixtureCase.Input.Fallback)
			}
			if !fellBack && !ok {
				t.Errorf("FromRemote(%q) = (%q, false), want ok=true producing %q", fixtureCase.Input.Remote, label, fixtureCase.Expected.Label)
			}
		})
	}
}

// TestFromRemote_NoHostAllowlist replaces the deleted
// TestFromRemote_HostPrefixTableIsNotExhaustive: this asserts the NEW
// property directly, that a host — whether well-known (github.com) or
// self-hosted/enterprise (git.example.com) — always keeps its FULL hostname
// in the rendered label. There is no allowlist and no short-prefix table:
// a known host is not special-cased, so both a well-known host and an
// unrecognized one render identically in shape (full-host + owner/repo).
func TestFromRemote_NoHostAllowlist(t *testing.T) {
	wellKnown, ok := projectlabel.FromRemote("github.com/example-org/garden-app")
	if !ok {
		t.Fatalf("FromRemote well-known host: ok = false, want true")
	}
	if wellKnown != "github.com:example-org/garden-app" {
		t.Fatalf("FromRemote well-known host = %q, want the full hostname preserved, no short prefix", wellKnown)
	}

	unrecognized, ok := projectlabel.FromRemote("git.example.com/acme/widgets")
	if !ok {
		t.Fatalf("FromRemote unrecognized host: ok = false, want true")
	}
	if unrecognized != "git.example.com:acme/widgets" {
		t.Fatalf("FromRemote unrecognized host = %q, want the full hostname preserved as the prefix", unrecognized)
	}
}

// TestLabelFixtureInventory proves the deletion-protection manifest is
// load-bearing: a manifest naming a case absent from the corpus must fail to
// load, and the shipped corpus + manifest pair must load cleanly together.
func TestLabelFixtureInventory(t *testing.T) {
	if _, _, err := loadLabelFixturesFromYAML(labelCasesYAML, labelManifestYAML); err != nil {
		t.Fatalf("shipped projectlabel corpus and manifest must load together: %v", err)
	}

	t.Run("manifest_naming_an_absent_case_is_rejected", func(t *testing.T) {
		mutatedManifest := []byte("requiredCaseNames:\n  - this_case_does_not_exist_in_the_corpus\n")
		if _, _, err := loadLabelFixturesFromYAML(labelCasesYAML, mutatedManifest); err == nil {
			t.Fatal("expected an error when the manifest names a case absent from the corpus")
		}
	})

	t.Run("deleting_a_required_row_is_rejected", func(t *testing.T) {
		// Simulate a fixture-row deletion by decoding the real corpus, then
		// dropping the first case named in the manifest, and confirming the
		// manifest that shipped alongside it now rejects the mutated corpus.
		corpus, err := testcase.LoadCorpus[labelInput, labelExpected](labelCasesYAML)
		if err != nil {
			t.Fatalf("decode projectlabel corpus: %v", err)
		}
		manifest, err := decodeLabelFixtureManifest(labelManifestYAML)
		if err != nil {
			t.Fatalf("decode projectlabel manifest: %v", err)
		}
		if len(manifest.RequiredCaseNames) == 0 {
			t.Fatal("manifest has no required case names to delete")
		}
		victim := manifest.RequiredCaseNames[0]
		mutated := make([]testcase.Case[labelInput, labelExpected], 0, len(corpus.Cases))
		for _, c := range corpus.Cases {
			if c.Name == victim {
				continue
			}
			mutated = append(mutated, c)
		}
		if len(mutated) != len(corpus.Cases)-1 {
			t.Fatalf("expected to delete exactly one case named %q", victim)
		}
		if err := validateLabelFixtureInventory(testcase.Corpus[labelInput, labelExpected]{Cases: mutated}, manifest); err == nil {
			t.Fatalf("expected validateLabelFixtureInventory to reject a corpus missing required case %q", victim)
		}
	})
}
