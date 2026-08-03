package testutil

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// SemanticManifest keeps the required behavior names for a fixture in data,
// next to the cases it governs, instead of hiding that contract in test code.
type SemanticManifest struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
}

// DecodeSemanticManifest strictly decodes one semantic manifest document.
func DecodeSemanticManifest(data []byte, label string) (SemanticManifest, error) {
	var manifest SemanticManifest
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return SemanticManifest{}, fmt.Errorf("decode %s semantic manifest: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return SemanticManifest{}, fmt.Errorf("%s semantic manifest must contain exactly one YAML document: %v", label, err)
	}
	if manifest.ExpectedCaseCount <= 0 || manifest.ExpectedCaseCount != len(manifest.RequiredNames) {
		return SemanticManifest{}, fmt.Errorf("%s semantic manifest expectedCaseCount = %d, want exactly %d required names", label, manifest.ExpectedCaseCount, len(manifest.RequiredNames))
	}
	seen := make(map[string]struct{}, len(manifest.RequiredNames))
	for index, name := range manifest.RequiredNames {
		if name == "" {
			return SemanticManifest{}, fmt.Errorf("%s semantic manifest requiredNames[%d] is empty", label, index)
		}
		if _, duplicate := seen[name]; duplicate {
			return SemanticManifest{}, fmt.Errorf("%s semantic manifest repeats required name %q", label, name)
		}
		seen[name] = struct{}{}
	}
	return manifest, nil
}

// ValidateSemanticNames requires an exact, unique match with the manifest.
func ValidateSemanticNames(manifest SemanticManifest, actual []string, label string) error {
	if len(actual) != manifest.ExpectedCaseCount {
		return fmt.Errorf("%s fixture has %d cases, want exactly %d", label, len(actual), manifest.ExpectedCaseCount)
	}
	seen := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%s fixture repeats case name %q", label, name)
		}
		seen[name] = struct{}{}
	}
	for _, name := range manifest.RequiredNames {
		if _, found := seen[name]; !found {
			return fmt.Errorf("%s fixture is missing required family %q", label, name)
		}
	}
	return nil
}
