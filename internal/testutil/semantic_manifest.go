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

// RequiredNamesManifest is a NAME-ONLY fixture manifest: it declares the set
// of case names a fixture must carry and nothing else. Unlike
// SemanticManifest, it has no expectedCaseCount field — a bare integer count
// churns on every fixture addition and is a merge-conflict magnet across
// parallel slices (user ruling 2026-08-24: no count guards). Exact membership
// is enforced by comparing NAME SETS in ValidateRequiredNames, which does not
// need a count to do that.
type RequiredNamesManifest struct {
	RequiredNames []string `yaml:"requiredNames"`
}

// DecodeRequiredNamesManifest strictly decodes one name-only manifest
// document: unknown fields (including a stray expectedCaseCount) are
// rejected, exactly one YAML document is required, and every name must be
// non-empty and unique.
func DecodeRequiredNamesManifest(data []byte, label string) (RequiredNamesManifest, error) {
	var manifest RequiredNamesManifest
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return RequiredNamesManifest{}, fmt.Errorf("decode %s required-names manifest: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return RequiredNamesManifest{}, fmt.Errorf("%s required-names manifest must contain exactly one YAML document: %v", label, err)
	}
	if len(manifest.RequiredNames) == 0 {
		return RequiredNamesManifest{}, fmt.Errorf("%s required-names manifest declares no requiredNames", label)
	}
	seen := make(map[string]struct{}, len(manifest.RequiredNames))
	for index, name := range manifest.RequiredNames {
		if name == "" {
			return RequiredNamesManifest{}, fmt.Errorf("%s required-names manifest requiredNames[%d] is empty", label, index)
		}
		if _, duplicate := seen[name]; duplicate {
			return RequiredNamesManifest{}, fmt.Errorf("%s required-names manifest repeats required name %q", label, name)
		}
		seen[name] = struct{}{}
	}
	return manifest, nil
}

// ValidateRequiredNames requires the fixture's case names and the manifest's
// declared names to be the EXACT same set — every declared name present in
// the fixture, and no fixture row outside the declared set — WITHOUT relying
// on a count. A fixture that adds or removes a case therefore fails here
// until the manifest is updated to name it, and a manifest that names a row
// nobody wrote fails the same way; neither direction can go unnoticed by a
// bare integer changing.
func ValidateRequiredNames(manifest RequiredNamesManifest, actual []string, label string) error {
	seenActual := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if _, duplicate := seenActual[name]; duplicate {
			return fmt.Errorf("%s fixture repeats case name %q", label, name)
		}
		seenActual[name] = struct{}{}
	}
	seenRequired := make(map[string]struct{}, len(manifest.RequiredNames))
	for _, name := range manifest.RequiredNames {
		seenRequired[name] = struct{}{}
	}
	for _, name := range manifest.RequiredNames {
		if _, found := seenActual[name]; !found {
			return fmt.Errorf("%s fixture is missing required case %q", label, name)
		}
	}
	for _, name := range actual {
		if _, declared := seenRequired[name]; !declared {
			return fmt.Errorf("%s fixture carries undeclared case %q; add it to the required-names manifest", label, name)
		}
	}
	return nil
}
