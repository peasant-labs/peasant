package transcript

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/model_producer_validation.yaml
var modelProducerValidationFixtureYAML []byte

//go:embed testdata/model_producer_validation.manifest.yaml
var modelProducerValidationManifestYAML []byte

type modelProducerValidationCase struct {
	Name          string `yaml:"name"`
	Role          string `yaml:"role"`
	Depth         int    `yaml:"depth"`
	ObservedModel string `yaml:"observedModel"`
	Accepted      bool   `yaml:"accepted"`
}

type modelProducerValidationFixture struct {
	Cases []modelProducerValidationCase `yaml:"cases"`
}

func loadModelProducerValidationFixture(t *testing.T) modelProducerValidationFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(modelProducerValidationFixtureYAML))
	decoder.KnownFields(true)
	var fixture modelProducerValidationFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode producer validation fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("producer validation fixture must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeSemanticManifest(modelProducerValidationManifestYAML, "model producer validation")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if fixtureCase.Name == "" || !schema.Role(fixtureCase.Role).IsValid() {
			t.Fatalf("producer validation fixture case %q is invalid", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateSemanticNames(manifest, names, "model producer validation"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestValidateObservedModelEvidenceFixture(t *testing.T) {
	fixture := loadModelProducerValidationFixture(t)
	for index, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			turn := ingest.Turn{Index: index, Role: schema.Role(fixtureCase.Role), Depth: fixtureCase.Depth, ObservedModel: ingest.ObservedModelID(fixtureCase.ObservedModel)}
			err := ValidateObservedModelEvidence(turn)
			if (err == nil) != fixtureCase.Accepted {
				t.Fatalf("accepted=%t, want %t; error=%v", err == nil, fixtureCase.Accepted, err)
			}
			if err != nil {
				for _, fragment := range []string{"observed-model producer validation failed", "  what:", "  why:", "  where: transcript.ValidateObservedModelEvidence", "  when: during SessionDetailPayload construction", "  meaning:", "  fix:", "retry"} {
					if !strings.Contains(err.Error(), fragment) {
						t.Errorf("actionable error missing %q: %v", fragment, err)
					}
				}
			}
		})
	}
}
