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

//go:embed testdata/thinking_level_producer_validation.yaml
var thinkingProducerYAML []byte

//go:embed testdata/thinking_level_producer_validation.manifest.yaml
var thinkingProducerManifestYAML []byte

type thinkingProducerFixture struct {
	ErrorFragments []string `yaml:"errorFragments"`
	Cases          []struct {
		Name     string                  `yaml:"name"`
		Role     schema.Role             `yaml:"role"`
		Level    schema.ThinkingLevel    `yaml:"level"`
		Raw      schema.ThinkingLevelRaw `yaml:"raw"`
		Earlier  bool                    `yaml:"earlier"`
		Accepted bool                    `yaml:"accepted"`
	} `yaml:"cases"`
}

func loadThinkingProducerFixtures(t *testing.T) thinkingProducerFixture {
	t.Helper()
	var fixture thinkingProducerFixture
	decoder := yaml.NewDecoder(bytes.NewReader(thinkingProducerYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture must contain one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(thinkingProducerManifestYAML, "thinking producer")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "thinking producer"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestThinkingLevelProducerValidation(t *testing.T) {
	t.Parallel()
	fixture := loadThinkingProducerFixtures(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			turn := ingest.Turn{Role: c.Role, ThinkingLevel: c.Level, ThinkingLevelRaw: c.Raw}
			session := &ingest.Session{Harness: schema.HarnessClaudeCode, Turns: []ingest.Turn{turn}}
			if c.Earlier {
				session.Turns = nil
				session.EarlierHistory = []ingest.EarlierHistorySection{{Turns: []ingest.Turn{turn}}}
			}
			detail, err := SessionToDetailValidated(session)
			if (err == nil) != c.Accepted {
				t.Fatalf("accepted=%t, want %t: %v", err == nil, c.Accepted, err)
			}
			if err != nil {
				if detail != nil {
					t.Fatal("refused evidence returned a payload")
				}
				for _, fragment := range fixture.ErrorFragments {
					if !strings.Contains(err.Error(), fragment) {
						t.Errorf("error missing %q: %v", fragment, err)
					}
				}
			} else if detail.Turns[0].ThinkingLevel != c.Level || detail.Turns[0].ThinkingLevelRaw != c.Raw {
				t.Fatal("accepted evidence changed during projection")
			}
		})
	}
}
