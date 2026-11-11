package harnesslayout

import (
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/cline_title_tags.yaml
var clineTitleTagFixtures []byte

func TestClineTitleTags(t *testing.T) {
	var cases []struct {
		Name  string `yaml:"name"`
		Input string `yaml:"input"`
		Want  string `yaml:"want"`
	}
	if err := yaml.Unmarshal(clineTitleTagFixtures, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if got := clineTitle(tc.Input); got != tc.Want {
				t.Errorf("clineTitle(%q) = %q, want %q", tc.Input, got, tc.Want)
			}
		})
	}
}
