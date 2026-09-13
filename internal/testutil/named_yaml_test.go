package testutil_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/named_yaml.yaml
var namedYAML []byte

func TestDecodeNamedFixtureYAML(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name     string `yaml:"name"`
			Document string `yaml:"document"`
			Reject   bool   `yaml:"reject"`
		} `yaml:"cases"`
	}
	if err := testutil.DecodeNamedFixtureYAML(namedYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			var target struct {
				RequiredNames []string `yaml:"requiredNames"`
				Cases         []struct {
					Name string `yaml:"name"`
				} `yaml:"cases"`
			}
			err := testutil.DecodeNamedFixtureYAML([]byte(c.Document), &target)
			if (err != nil) != c.Reject {
				t.Fatalf("decode error = %v, want rejection %t", err, c.Reject)
			}
		})
	}
}
