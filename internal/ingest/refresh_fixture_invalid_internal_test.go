package ingest

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_decision_invalid.yaml
var invalidRefreshDecisionYAML []byte

func TestRefreshLanePriorityFixtureRejectsUnknownAxes(t *testing.T) {
	var invalid struct {
		RequiredNames []string `yaml:"required_names"`
		PriorityCases []struct {
			Name          string    `yaml:"name"`
			Patch         yaml.Node `yaml:"patch"`
			ErrorContains string    `yaml:"error_contains"`
		} `yaml:"priority_cases"`
		StatusCases []struct {
			Name          string            `yaml:"name"`
			Value         fixtureDiffStatus `yaml:"value"`
			ErrorContains string            `yaml:"error_contains"`
		} `yaml:"status_cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(invalidRefreshDecisionYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&invalid); err != nil {
		t.Fatalf("decode invalid refresh decision fixtures: %v", err)
	}
	if len(invalid.RequiredNames) == 0 {
		t.Fatal("invalid refresh decision fixture must list its required cases")
	}
	baseline, err := decodeCapturePriorityFixture(refreshCapturePriorityYAML)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, testCase := range invalid.PriorityCases {
		if testCase.Name == "" || names[testCase.Name] {
			t.Fatalf("invalid refresh decision fixtures have an empty or repeated name %q", testCase.Name)
		}
		names[testCase.Name] = true
		t.Run(testCase.Name, func(t *testing.T) {
			input := baseline.Cases[0]
			if err := testCase.Patch.Decode(&input); err != nil {
				t.Fatalf("decode invalid case patch: %v", err)
			}
			data, err := yaml.Marshal(capturePriorityFixture{RequiredNames: []string{input.Name}, Cases: []capturePriorityCase{input}})
			if err != nil {
				t.Fatalf("encode invalid case: %v", err)
			}
			_, err = decodeCapturePriorityFixture(data)
			if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) {
				t.Fatalf("validation error = %v, want it to name %q", err, testCase.ErrorContains)
			}
		})
	}
	for _, testCase := range invalid.StatusCases {
		if testCase.Name == "" || names[testCase.Name] {
			t.Fatalf("invalid refresh decision fixtures have an empty or repeated name %q", testCase.Name)
		}
		names[testCase.Name] = true
		t.Run(testCase.Name, func(t *testing.T) {
			_, err := testCase.Value.Status()
			if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) {
				t.Fatalf("validation error = %v, want it to name %q", err, testCase.ErrorContains)
			}
		})
	}
	for _, name := range invalid.RequiredNames {
		if !names[name] {
			t.Fatalf("invalid refresh decision fixture is missing required case %q", name)
		}
	}
}
