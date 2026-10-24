package store

import (
	"bytes"
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/generation_title_line.yaml
var generationTitleLineYAML []byte

// TestFirstTitleLine pins the list title derived from a generation's title
// turn, the value every session row and search hit shows.
func TestFirstTitleLine(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name string `yaml:"name"`
			Text string `yaml:"text"`
			Want string `yaml:"want"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(generationTitleLineYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		if names[c.Name] {
			t.Fatalf("fixture case %q is listed twice", c.Name)
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.RequiredNames {
		if !names[name] {
			t.Fatalf("required fixture case %q is missing", name)
		}
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if got := firstTitleLine(c.Text); got != c.Want {
				t.Fatalf("firstTitleLine(%q) = %q, want %q", c.Text, got, c.Want)
			}
		})
	}
}
