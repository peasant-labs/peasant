package testutil_test

import (
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/exact_fixture_names.yaml
var exactFixtureNamesYAML []byte

// Evidence fixture loaders use this exact-membership gate in addition to
// typed decoding. These controls distinguish it from the older subset
// helper, whose intentionally different semantics remain unchanged.
func TestExactFixtureNameMembership(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string   `yaml:"name"`
			Required []string `yaml:"required"`
			Actual   []string `yaml:"actual"`
			Reject   bool     `yaml:"reject"`
			Reason   string   `yaml:"reason"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(exactFixtureNamesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: c.Required}, c.Actual, c.Name)
			if (err != nil) != c.Reject || err != nil && !strings.Contains(err.Error(), c.Reason) {
				t.Fatalf("exact names: %v, want rejection=%t reason=%q", err, c.Reject, c.Reason)
			}
		})
	}
}
