package indexformat

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/outcome_membership.yaml
var outcomeMembershipYAML []byte

func TestOutcomeClosedSet(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"required_names"`
		Cases         []struct {
			Name    string `yaml:"name"`
			Outcome uint8  `yaml:"outcome"`
			Text    string `yaml:"text"`
			Valid   bool   `yaml:"valid"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(outcomeMembershipYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("outcome fixture must have one document: %v", err)
	}
	actual := make(map[string]bool, len(fixture.Cases))
	for _, row := range fixture.Cases {
		actual[row.Name] = true
		outcome := Outcome(row.Outcome)
		if outcome.IsValid() != row.Valid || outcome.String() != row.Text {
			t.Errorf("%s: got valid=%v text=%q, want valid=%v text=%q", row.Name, outcome.IsValid(), outcome.String(), row.Valid, row.Text)
		}
	}
	for _, name := range fixture.RequiredNames {
		if !actual[name] {
			t.Errorf("required outcome fixture missing: %s", name)
		}
		delete(actual, name)
	}
	if len(actual) != 0 {
		t.Errorf("outcome fixtures missing from required-name manifest: %v", actual)
	}
	if got := AllOutcomes(); len(got) != 6 {
		t.Fatalf("closed outcome set has %d members, want 6", len(got))
	}
}
