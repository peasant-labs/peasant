package ingest

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/claim_sessions_cases.yaml
var claimSessionsCasesYAML []byte

type claimSessionsFixture struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []claimSessionsCase `yaml:"cases"`
}

type claimSessionsCase struct {
	Name    string                `yaml:"name"`
	Results []claimSessionsResult `yaml:"results"`
	Want    []string              `yaml:"want"`
}

type claimSessionsResult struct {
	ID     string `yaml:"id"`
	Status string `yaml:"status"`
}

func TestClaimableSessionIDs(t *testing.T) {
	var fixture claimSessionsFixture
	decoder := yaml.NewDecoder(bytes.NewReader(claimSessionsCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode claim_sessions_cases.yaml: %v", err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, test := range fixture.Cases {
		names = append(names, test.Name)
	}
	if err := validateClaimSessionsNames(fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.Cases {
		test := test
		t.Run(test.Name, func(t *testing.T) {
			results := make([]SessionResult, 0, len(test.Results))
			for _, result := range test.Results {
				status := DiffUnchanged
				switch result.Status {
				case "new":
					status = DiffNew
				case "updated":
					status = DiffUpdated
				case "unchanged":
					status = DiffUnchanged
				default:
					t.Fatalf("unknown fixture status %q", result.Status)
				}
				results = append(results, SessionResult{SessionID: SessionID(result.ID), Status: status})
			}
			got := claimableSessionIDs(results)
			if strings.Join(got, ",") != strings.Join(test.Want, ",") {
				t.Fatalf("claimableSessionIDs = %v, want %v", got, test.Want)
			}
		})
	}
}

// validateClaimSessionsNames requires the fixture's case names and the
// requiredNames list to be the exact same set, so neither a deleted case nor an
// undeclared new one passes unnoticed.
func validateClaimSessionsNames(required, actual []string) error {
	requiredSet := make(map[string]bool, len(required))
	for _, name := range required {
		if name == "" || requiredSet[name] {
			return fmt.Errorf("claim_sessions_cases.yaml requiredNames has a blank or duplicate name %q", name)
		}
		requiredSet[name] = true
	}
	actualSet := make(map[string]bool, len(actual))
	for _, name := range actual {
		if actualSet[name] {
			return fmt.Errorf("claim_sessions_cases.yaml repeats case %q", name)
		}
		actualSet[name] = true
	}
	for name := range requiredSet {
		if !actualSet[name] {
			return fmt.Errorf("claim_sessions_cases.yaml is missing required case %q", name)
		}
	}
	for name := range actualSet {
		if !requiredSet[name] {
			return fmt.Errorf("claim_sessions_cases.yaml has undeclared case %q", name)
		}
	}
	return nil
}
