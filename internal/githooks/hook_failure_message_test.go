package githooks_test

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/hook_failure_message.yaml
var hookFailureMessageYAML []byte

type hookFailureMessageCase struct {
	Name           string   `yaml:"name"`
	MustContain    []string `yaml:"mustContain"`
	MustNotContain []string `yaml:"mustNotContain"`
}

// TestHookFailureMessage pins the text a generated hook prints when its
// upload command exits non-zero, for every managed event.
func TestHookFailureMessage(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string                 `yaml:"requiredNames"`
		Cases         []hookFailureMessageCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(hookFailureMessageYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode hook_failure_message.yaml: %v", err)
	}
	present := map[string]bool{}
	for _, c := range fixture.Cases {
		present[c.Name] = true
	}
	for _, name := range fixture.RequiredNames {
		if !present[name] {
			t.Fatalf("required case %q is missing from hook_failure_message.yaml", name)
		}
	}
	for _, c := range fixture.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			for _, event := range githooks.AllEvents {
				script, err := githooks.Script(event, "/tmp/repo", "/tmp/repo/.git/hooks/"+event.String(), githooks.Binding{})
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range c.MustContain {
					if !strings.Contains(script, want) {
						t.Errorf("%s hook misses %q", event, want)
					}
				}
				for _, banned := range c.MustNotContain {
					if strings.Contains(script, banned) {
						t.Errorf("%s hook still says %q", event, banned)
					}
				}
			}
		})
	}
}
