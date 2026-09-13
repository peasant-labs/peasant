package testutil

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_contract/cases.yaml
var profileContractCasesYAML []byte

//go:embed testdata/profile_contract/manifest.yaml
var profileContractManifestYAML []byte

//go:embed testdata/profile_push/cases.yaml
var profilePushCasesYAML []byte

//go:embed testdata/profile_push/manifest.yaml
var profilePushManifestYAML []byte

//go:embed testdata/profile_redaction/cases.yaml
var profileRedactionCasesYAML []byte

//go:embed testdata/profile_redaction/manifest.yaml
var profileRedactionManifestYAML []byte

//go:embed testdata/profile_cli/cases.yaml
var profileCLICasesYAML []byte

//go:embed testdata/profile_cli/manifest.yaml
var profileCLIManifestYAML []byte

// ProfileFixtureFamily identifies one shared profile fixture family. It lets
// consumer packages choose the family they need without importing each other.
type ProfileFixtureFamily string

const (
	ProfileFixtureFamilyContract  ProfileFixtureFamily = "profile_contract"
	ProfileFixtureFamilyPush      ProfileFixtureFamily = "profile_push"
	ProfileFixtureFamilyRedaction ProfileFixtureFamily = "profile_redaction"
	ProfileFixtureFamilyCLI       ProfileFixtureFamily = "profile_cli"
)

// ProfileFixtureSet is one strictly decoded profile fixture family plus its
// name-only manifest. The cases intentionally use safe strings and structural
// expectations so profiling evidence tests do not depend on wall-clock limits.
type ProfileFixtureSet struct {
	Family   ProfileFixtureFamily
	Manifest RequiredNamesManifest
	Cases    []ProfileFixtureCase
}

// ProfileFixtureCase is a profile fixture skeleton shared by later contract,
// push, redaction, and CLI tests.
type ProfileFixtureCase struct {
	Name               string   `yaml:"name"`
	Description        string   `yaml:"description"`
	Subject            string   `yaml:"subject"`
	ExpectedStages     []string `yaml:"expectedStages"`
	ExpectedCounters   []string `yaml:"expectedCounters"`
	ExpectedOutputKeys []string `yaml:"expectedOutputKeys"`
	EvidenceRules      []string `yaml:"evidenceRules"`
	ForbiddenInputs    []string `yaml:"forbiddenInputs"`
}

type profileFixtureDocument struct {
	Cases []ProfileFixtureCase `yaml:"cases"`
}

// LoadProfileContractFixtures loads required profile output contract examples.
func LoadProfileContractFixtures() (ProfileFixtureSet, error) {
	return loadProfileFixtures(ProfileFixtureFamilyContract, profileContractCasesYAML, profileContractManifestYAML)
}

// LoadProfilePushFixtures loads required village push profile examples.
func LoadProfilePushFixtures() (ProfileFixtureSet, error) {
	return loadProfileFixtures(ProfileFixtureFamilyPush, profilePushCasesYAML, profilePushManifestYAML)
}

// LoadProfileRedactionFixtures loads required redaction profile examples.
func LoadProfileRedactionFixtures() (ProfileFixtureSet, error) {
	return loadProfileFixtures(ProfileFixtureFamilyRedaction, profileRedactionCasesYAML, profileRedactionManifestYAML)
}

// LoadProfileCLIFixtures loads required CLI profile examples.
func LoadProfileCLIFixtures() (ProfileFixtureSet, error) {
	return loadProfileFixtures(ProfileFixtureFamilyCLI, profileCLICasesYAML, profileCLIManifestYAML)
}

func loadProfileFixtures(family ProfileFixtureFamily, casesYAML, manifestYAML []byte) (ProfileFixtureSet, error) {
	manifest, err := DecodeRequiredNamesManifest(manifestYAML, string(family))
	if err != nil {
		return ProfileFixtureSet{}, err
	}

	var document profileFixtureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(casesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return ProfileFixtureSet{}, fmt.Errorf("decode %s profile fixture cases: %w", family, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ProfileFixtureSet{}, fmt.Errorf("%s profile fixture cases must contain exactly one YAML document: %v", family, err)
	}

	set := ProfileFixtureSet{Family: family, Manifest: manifest, Cases: document.Cases}
	if err := set.validate(); err != nil {
		return ProfileFixtureSet{}, err
	}
	return set, nil
}

func (s ProfileFixtureSet) validate() error {
	names := make([]string, 0, len(s.Cases))
	for index, profileCase := range s.Cases {
		if strings.TrimSpace(profileCase.Name) == "" {
			return fmt.Errorf("%s profile fixture case[%d] has an empty name", s.Family, index)
		}
		if strings.TrimSpace(profileCase.Description) == "" {
			return fmt.Errorf("%s profile fixture case %q has an empty description", s.Family, profileCase.Name)
		}
		if strings.TrimSpace(profileCase.Subject) == "" {
			return fmt.Errorf("%s profile fixture case %q has an empty subject", s.Family, profileCase.Name)
		}
		if len(profileCase.EvidenceRules) == 0 {
			return fmt.Errorf("%s profile fixture case %q has no evidence rules", s.Family, profileCase.Name)
		}
		if err := validateSafeProfileFixtureCase(s.Family, profileCase); err != nil {
			return err
		}
		names = append(names, profileCase.Name)
	}
	if err := ValidateRequiredNames(s.Manifest, names, string(s.Family)); err != nil {
		return fmt.Errorf("validate %s profile fixture manifest: %w", s.Family, err)
	}
	return nil
}

func validateSafeProfileFixtureCase(family ProfileFixtureFamily, profileCase ProfileFixtureCase) error {
	for _, value := range append(append(append([]string{}, profileCase.ExpectedStages...), profileCase.ExpectedCounters...), profileCase.ExpectedOutputKeys...) {
		if strings.Contains(value, "/") || strings.Contains(value, "http://") || strings.Contains(value, "https://") || strings.Contains(value, "git@") {
			return fmt.Errorf("%s profile fixture case %q has unsafe structural value %q; use a closed token, safe identifier, or sentinel", family, profileCase.Name, value)
		}
	}
	return nil
}

// Names returns the fixture case names in file order.
func (s ProfileFixtureSet) Names() []string {
	names := make([]string, len(s.Cases))
	for index, profileCase := range s.Cases {
		names[index] = profileCase.Name
	}
	return names
}
