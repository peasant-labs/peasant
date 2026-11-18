package ingest_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_lane_invalid.yaml
var invalidRefreshLaneYAML []byte

// TestRefreshLaneFixtureRejectsUnknownAxes proves a typo in any mutation or
// expectation is rejected rather than silently testing a different case.
func TestRefreshLaneFixtureRejectsUnknownAxes(t *testing.T) {
	var invalid struct {
		RequiredNames []string `yaml:"required_names"`
		Cases         []struct {
			Name          string    `yaml:"name"`
			Patch         yaml.Node `yaml:"patch"`
			ErrorContains string    `yaml:"error_contains"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(invalidRefreshLaneYAML, &invalid); err != nil {
		t.Fatalf("decode invalid refresh lane fixtures: %v", err)
	}
	if len(invalid.RequiredNames) == 0 {
		t.Fatal("invalid refresh lane fixture must list its required cases")
	}
	baseline := loadRefreshLaneFixture(t).Cases[0]
	names := make(map[string]bool, len(invalid.Cases))
	for _, testCase := range invalid.Cases {
		if testCase.Name == "" || names[testCase.Name] {
			t.Fatalf("invalid refresh lane fixtures have an empty or repeated name %q", testCase.Name)
		}
		names[testCase.Name] = true
		t.Run(testCase.Name, func(t *testing.T) {
			input := baseline
			if err := testCase.Patch.Decode(&input); err != nil {
				t.Fatalf("decode invalid case patch: %v", err)
			}
			data, err := yaml.Marshal(refreshLaneFixture{Cases: []refreshLaneCase{input}})
			if err != nil {
				t.Fatalf("encode invalid case: %v", err)
			}
			_, err = decodeRefreshLaneFixture(data)
			if err == nil || !strings.Contains(err.Error(), testCase.ErrorContains) {
				t.Fatalf("validation error = %v, want it to name %q", err, testCase.ErrorContains)
			}
		})
	}
	if err := testutil.RequireFixtureNames("refresh_lane_invalid.yaml", "cases", invalid.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
}
