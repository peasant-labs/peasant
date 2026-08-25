package testutil

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

// InjectedCommandTurnFixturePath names the shared corpus used by local detail
// and push projection tests.
const InjectedCommandTurnFixturePath = "internal/testutil/testdata/injected_command_turns.yaml"

//go:embed testdata/injected_command_turns.yaml
var injectedCommandTurnFixtureYAML []byte

// InjectedCommandTurnFixture is the strictly decoded command-wrapper corpus.
type InjectedCommandTurnFixture struct {
	// RequiredNames lists every case name the corpus must retain. It is a
	// deletion-protection manifest, not a row count: adding a new case does
	// not require touching this list, but removing a required case (or
	// renaming it without updating this list) fails the load.
	RequiredNames []string                  `yaml:"requiredNames"`
	Cases         []InjectedCommandTurnCase `yaml:"cases"`
}

// InjectedCommandTurnCase describes one stored entry and its projected role.
type InjectedCommandTurnCase struct {
	Name              string         `yaml:"name"`
	Harness           schema.Harness `yaml:"harness"`
	SourceRole        schema.Role    `yaml:"sourceRole"`
	Content           string         `yaml:"content"`
	PadToPreviewLimit bool           `yaml:"padToPreviewLimit,omitempty"`
	ExpectedRole      schema.Role    `yaml:"expectedRole"`
}

// StoredContent returns the exact ContentPreview bytes the case sends through
// production. Padding is fixture metadata for the ambiguous truncation-limit
// case; callers must not reproduce that transformation independently.
func (testCase InjectedCommandTurnCase) StoredContent() string {
	if !testCase.PadToPreviewLimit || len(testCase.Content) >= defaults.ContentPreviewLimit {
		return testCase.Content
	}
	return strings.Repeat(" ", defaults.ContentPreviewLimit-len(testCase.Content)) + testCase.Content
}

// LoadInjectedCommandTurnFixture returns the one embedded, strictly validated
// corpus shared by every projection boundary test.
func LoadInjectedCommandTurnFixture() (InjectedCommandTurnFixture, error) {
	return decodeInjectedCommandTurnFixture(injectedCommandTurnFixtureYAML)
}

func decodeInjectedCommandTurnFixture(source []byte) (InjectedCommandTurnFixture, error) {
	var fixture InjectedCommandTurnFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: typed fields do not match the corpus schema: %w; fix the named YAML field", InjectedCommandTurnFixturePath, err)
	}

	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: expected exactly one YAML document; remove the trailing document", InjectedCommandTurnFixturePath)
		}
		return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s trailing content: %w; remove or repair the trailing document", InjectedCommandTurnFixturePath, err)
	}

	seenNames := make(map[string]struct{}, len(fixture.Cases))
	for index, testCase := range fixture.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: cases[%d] has a blank name; give every row a stable unique name", InjectedCommandTurnFixturePath, index)
		}
		if _, duplicate := seenNames[testCase.Name]; duplicate {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: case name %q is duplicated; give every row a unique name", InjectedCommandTurnFixturePath, testCase.Name)
		}
		seenNames[testCase.Name] = struct{}{}
		if !testCase.Harness.IsKnown() {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: case %q has unknown harness %q; use a schema harness value", InjectedCommandTurnFixturePath, testCase.Name, testCase.Harness)
		}
		if !testCase.SourceRole.IsValid() {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: case %q has unknown source role %q; use a schema role value", InjectedCommandTurnFixturePath, testCase.Name, testCase.SourceRole)
		}
		if !testCase.ExpectedRole.IsValid() {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: case %q has unknown expected role %q; use a schema role value", InjectedCommandTurnFixturePath, testCase.Name, testCase.ExpectedRole)
		}
		if testCase.Content == "" {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: case %q has empty content; provide the stored content under test", InjectedCommandTurnFixturePath, testCase.Name)
		}
		if testCase.PadToPreviewLimit && len(testCase.Content) >= defaults.ContentPreviewLimit {
			return InjectedCommandTurnFixture{}, fmt.Errorf("decode injected command turn fixture %s: case %q cannot pad content length %d to preview limit %d; shorten the fixture content", InjectedCommandTurnFixturePath, testCase.Name, len(testCase.Content), defaults.ContentPreviewLimit)
		}
	}

	presentNames := make(map[string]bool, len(seenNames))
	for name := range seenNames {
		presentNames[name] = true
	}
	if err := RequireFixtureNames(fmt.Sprintf("injected command turn fixture %s", InjectedCommandTurnFixturePath), "case", fixture.RequiredNames, presentNames); err != nil {
		return InjectedCommandTurnFixture{}, err
	}

	return fixture, nil
}
