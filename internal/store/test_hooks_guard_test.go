package store

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/hookguard"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/test_hooks.yaml
var storeTestHooksYAML []byte

// TestStoreTestHooksStayTestOnly fails when a production file assigns one of
// the store's context-validation test hooks or a function outside its named
// readers reads one.
func TestStoreTestHooksStayTestOnly(t *testing.T) {
	var fixture struct {
		Hooks []hookguard.Hook `yaml:"hooks"`
	}
	if err := yaml.Unmarshal(storeTestHooksYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	findings, err := hookguard.Check(".", fixture.Hooks)
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range findings {
		t.Error(finding)
	}
}
