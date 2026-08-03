package api

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/map_error_responses.yaml
var mapGuardErrorsYAML []byte

var requiredMapGuardFixtureNames = map[string]struct{}{
	"selection visibility fails closed": {}, "missing project is stable": {},
	"ambiguous legacy project is stable": {}, "malformed project identity is stable": {},
	"missing node is stable": {}, "missing branch is stable": {},
	"missing repository is stable": {}, "provider details are sanitized": {},
	"missing guard hash is actionable": {}, "malformed guard hash is actionable": {},
	"unavailable guard provider is actionable":       {},
	"search selection visibility is operation aware": {},
	"search provider failure is operation aware":     {},
}

type mapGuardFixture struct {
	ExpectedCaseCount int      `yaml:"expectedCaseCount"`
	RequiredNames     []string `yaml:"requiredNames"`
	Cases             []struct {
		Name               string   `yaml:"name"`
		ErrorKind          string   `yaml:"errorKind"`
		Endpoint           string   `yaml:"endpoint"`
		Status             int      `yaml:"status"`
		Code               string   `yaml:"code"`
		RequiredFragments  []string `yaml:"requiredFragments"`
		ForbiddenFragments []string `yaml:"forbiddenFragments"`
	} `yaml:"cases"`
}

func decodeMapGuardFixture(source []byte) (mapGuardFixture, error) {
	var fixture mapGuardFixture
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode map guard fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fixture, fmt.Errorf("map guard fixture must contain exactly one YAML document: %v", err)
	}
	if fixture.ExpectedCaseCount != len(requiredMapGuardFixtureNames) || len(fixture.RequiredNames) != fixture.ExpectedCaseCount || len(fixture.Cases) != fixture.ExpectedCaseCount {
		return fixture, fmt.Errorf("map guard fixture cardinality mismatch")
	}
	required := make(map[string]struct{}, len(fixture.RequiredNames))
	for _, name := range fixture.RequiredNames {
		if name == "" {
			return fixture, fmt.Errorf("map guard fixture has an empty required name")
		}
		if _, ok := requiredMapGuardFixtureNames[name]; !ok {
			return fixture, fmt.Errorf("map guard fixture has unknown required name %q", name)
		}
		if _, duplicate := required[name]; duplicate {
			return fixture, fmt.Errorf("map guard fixture repeats required name %q", name)
		}
		required[name] = struct{}{}
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || testCase.ErrorKind == "" || testCase.Endpoint == "" || testCase.Status == 0 || testCase.Code == "" || len(testCase.RequiredFragments) == 0 || len(testCase.ForbiddenFragments) == 0 {
			return fixture, fmt.Errorf("map guard fixture has an incomplete case %q", testCase.Name)
		}
		if _, duplicate := seen[testCase.Name]; duplicate {
			return fixture, fmt.Errorf("map guard fixture repeats case %q", testCase.Name)
		}
		if _, ok := required[testCase.Name]; !ok {
			return fixture, fmt.Errorf("map guard fixture has unknown case %q", testCase.Name)
		}
		seen[testCase.Name] = struct{}{}
		for _, fragment := range append(append([]string{}, testCase.RequiredFragments...), testCase.ForbiddenFragments...) {
			if fragment == "" {
				return fixture, fmt.Errorf("map guard fixture case %q has an empty message fragment", testCase.Name)
			}
		}
	}
	return fixture, nil
}

func TestExactRequestGuardActionableFixtureCases(t *testing.T) {
	fixture, err := decodeMapGuardFixture(mapGuardErrorsYAML)
	if err != nil {
		t.Fatal(err)
	}
	guardCases := 0
	for _, testCase := range fixture.Cases {
		if !strings.HasPrefix(testCase.ErrorKind, "guard-") {
			continue
		}
		guardCases++
		t.Run(testCase.Name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if testCase.ErrorKind == "guard-invalid" {
				r.SetPathValue("projectHash", "NOT-A-HASH")
			} else if testCase.ErrorKind == "guard-provider" {
				r.SetPathValue("projectHash", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			}
			w := httptest.NewRecorder()
			if _, ok := (&Server{}).exactRequestGuard(w, r, exactRequestSpec{operation: "map graph", projectHash: true, provider: true, query: map[string]bool{}}); ok {
				t.Fatal("guard unexpectedly allowed request")
			}
			if w.Code != testCase.Status {
				t.Fatalf("status = %d, want %d", w.Code, testCase.Status)
			}
			var body struct{ Error, Code string }
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Code != testCase.Code {
				t.Fatalf("code = %q, want %q", body.Code, testCase.Code)
			}
			for _, fragment := range testCase.RequiredFragments {
				if !strings.Contains(body.Error, fragment) {
					t.Fatalf("error %q is missing required actionable fragment %q", body.Error, fragment)
				}
			}
			for _, fragment := range testCase.ForbiddenFragments {
				if strings.Contains(body.Error, fragment) {
					t.Fatalf("error %q includes forbidden fragment %q", body.Error, fragment)
				}
			}
		})
	}
	if guardCases != 3 {
		t.Fatalf("guard fixture cases = %d, want 3", guardCases)
	}
}

func TestMapGuardFixtureRejectsStructuralMutations(t *testing.T) {
	unknown := bytes.Replace(mapGuardErrorsYAML, []byte("expectedCaseCount:"), []byte("unknown: true\nexpectedCaseCount:"), 1)
	if _, err := decodeMapGuardFixture(unknown); err == nil {
		t.Fatal("unknown field mutation unexpectedly validated")
	}
	trailing := append(append([]byte{}, mapGuardErrorsYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeMapGuardFixture(trailing); err == nil {
		t.Fatal("trailing document mutation unexpectedly validated")
	}
}
