package ingest_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/testkit/hookguard"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/test_hooks.yaml
var ingestTestHooksYAML []byte

// TestIngestTestHooksStayTestOnly fails when a production file assigns the
// Claude metadata-half mutation hook, or any function other than
// BuildGenerationMetadata reads it.
func TestIngestTestHooksStayTestOnly(t *testing.T) {
	var fixture struct {
		Hooks []hookguard.Hook `yaml:"hooks"`
	}
	if err := yaml.Unmarshal(ingestTestHooksYAML, &fixture); err != nil {
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
