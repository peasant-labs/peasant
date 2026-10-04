package ingest

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	schema "github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/thinking_level_mapping.yaml
var thinkingLevelMappingFixtureYAML []byte

//go:embed testdata/thinking_level_mapping.manifest.yaml
var thinkingLevelMappingManifestYAML []byte

type thinkingLevelMappingCase struct {
	Name       string `yaml:"name"`
	Harness    string `yaml:"harness"`
	Native     string `yaml:"native"`
	NativeGo   string `yaml:"nativeGo"`
	Level      string `yaml:"level"`
	Raw        string `yaml:"raw"`
	RawDropped bool   `yaml:"rawDropped"`
	Note       string `yaml:"note"`
}

type thinkingLevelMappingFixture struct {
	Cases []thinkingLevelMappingCase `yaml:"cases"`
}

type thinkingLevelMappingManifest struct {
	RequiredNames []string `yaml:"requiredNames"`
}

var thinkingLevelMappers = map[string]func(string) (thinkingLevelMapping, *DiagnosticEntry){
	string(defaults.HarnessCodex):        mapCodexEffort,
	string(defaults.LegacyHarnessClaude): mapClaudeEffort,
	"claude_legacy":                      mapClaudeLegacyLevel,
	string(defaults.HarnessOpenCode):     mapOpenCodeVariant,
	"pi":                                 mapPiThinkingLevel,
}

func decodeStrictYAML(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("expected exactly one YAML document: %v", err)
	}
	return nil
}

func (c thinkingLevelMappingCase) native() (string, error) {
	if c.NativeGo == "" {
		return c.Native, nil
	}
	return strconv.Unquote(c.NativeGo)
}

// loadThinkingLevelMappingFixtures decodes the corpus and enforces the
// required-name manifest: every required case must be present and every case
// must be named in the manifest.
func loadThinkingLevelMappingFixtures(fixtureYAML, manifestYAML []byte) (thinkingLevelMappingFixture, error) {
	var fixture thinkingLevelMappingFixture
	if err := decodeStrictYAML(fixtureYAML, &fixture); err != nil {
		return fixture, fmt.Errorf("decode thinking-level mapping corpus: %w", err)
	}
	var manifest thinkingLevelMappingManifest
	if err := decodeStrictYAML(manifestYAML, &manifest); err != nil {
		return fixture, fmt.Errorf("decode thinking-level mapping manifest: %w", err)
	}
	if len(manifest.RequiredNames) == 0 {
		return fixture, errors.New("thinking-level mapping manifest declares no requiredNames")
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, c := range fixture.Cases {
		if c.Name == "" {
			return fixture, errors.New("thinking-level mapping corpus has a case without a name")
		}
		if present[c.Name] {
			return fixture, fmt.Errorf("thinking-level mapping corpus repeats case %q", c.Name)
		}
		present[c.Name] = true
		if _, ok := thinkingLevelMappers[c.Harness]; !ok {
			return fixture, fmt.Errorf("thinking-level mapping case %q names unknown harness %q", c.Name, c.Harness)
		}
		if c.Native != "" && c.NativeGo != "" {
			return fixture, fmt.Errorf("thinking-level mapping case %q sets both native and nativeGo", c.Name)
		}
		if _, err := c.native(); err != nil {
			return fixture, fmt.Errorf("thinking-level mapping case %q nativeGo is not a Go string literal: %w", c.Name, err)
		}
	}
	required := make(map[string]bool, len(manifest.RequiredNames))
	for _, name := range manifest.RequiredNames {
		if required[name] {
			return fixture, fmt.Errorf("thinking-level mapping manifest repeats %q", name)
		}
		required[name] = true
		if !present[name] {
			return fixture, fmt.Errorf("thinking-level mapping corpus is missing required case %q", name)
		}
	}
	for name := range present {
		if !required[name] {
			return fixture, fmt.Errorf("thinking-level mapping case %q is not in the manifest; add it to requiredNames", name)
		}
	}
	return fixture, nil
}

func TestThinkingLevelMappingCorpus(t *testing.T) {
	fixture, err := loadThinkingLevelMappingFixtures(thinkingLevelMappingFixtureYAML, thinkingLevelMappingManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			native, _ := c.native()
			got, diagnostic := thinkingLevelMappers[c.Harness](native)
			want := thinkingLevelMapping{Level: schema.ThinkingLevel(c.Level), Raw: schema.ThinkingLevelRaw(c.Raw)}
			if got != want {
				t.Fatalf("%s(%q) = %+v, want %+v", c.Harness, native, got, want)
			}
			if got.Level != "" && !got.Level.IsValid() {
				t.Fatalf("mapped level %q is not canonical", got.Level)
			}
			if got.Raw != "" && !got.Raw.IsValid() {
				t.Fatalf("mapped raw %q violates the raw rule", got.Raw)
			}
			if string(got.Level) == native && got.Raw != "" {
				t.Fatalf("identity mapping %q must omit raw", native)
			}
			if !c.RawDropped {
				if diagnostic != nil {
					t.Fatalf("unexpected diagnostic: %+v", *diagnostic)
				}
				return
			}
			if diagnostic == nil {
				t.Fatal("expected a thinking_level_raw_dropped diagnostic")
			}
			if diagnostic.ErrorType != ThinkingLevelRawDroppedErrorType || diagnostic.Location == "" || diagnostic.Message == "" || diagnostic.Remediation == "" {
				t.Fatalf("diagnostic is not actionable: %+v", *diagnostic)
			}
		})
	}
}

func TestThinkingLevelMappingCorpusGuardsRequiredCases(t *testing.T) {
	var manifest thinkingLevelMappingManifest
	if err := decodeStrictYAML(thinkingLevelMappingManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, name := range manifest.RequiredNames {
		mutated := bytes.Replace(thinkingLevelMappingFixtureYAML, []byte("name: "+name+"\n"), []byte("name: renamed_case\n"), 1)
		if bytes.Equal(mutated, thinkingLevelMappingFixtureYAML) {
			t.Fatalf("required case %q not found verbatim in corpus", name)
		}
		_, err := loadThinkingLevelMappingFixtures(mutated, thinkingLevelMappingManifestYAML)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("renaming required case %q was not refused by name: %v", name, err)
		}
	}
}

func TestThinkingLevelSeedEarliestObservationWins(t *testing.T) {
	fixture, err := loadThinkingLevelMappingFixtures(thinkingLevelMappingFixtureYAML, thinkingLevelMappingManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			native, _ := c.native()
			mapping, _ := thinkingLevelMappers[c.Harness](native)
			meta := NewUnifiedMetadata()
			applied := applyThinkingLevelSeed(&meta, mapping)
			if applied == mapping.IsZero() {
				t.Fatalf("applyThinkingLevelSeed on empty metadata applied=%v for mapping %+v", applied, mapping)
			}
			if meta.ThinkingLevel != mapping.Level || meta.ThinkingLevelRaw != mapping.Raw {
				t.Fatalf("seed = {%q %q}, want %+v", meta.ThinkingLevel, meta.ThinkingLevelRaw, mapping)
			}
			if mapping.IsZero() {
				return
			}
			later := thinkingLevelMapping{Level: schema.ThinkingLevelMedium}
			if mapping == later {
				later = thinkingLevelMapping{Level: schema.ThinkingLevelHigh}
			}
			if applyThinkingLevelSeed(&meta, later) {
				t.Fatal("a later observation overwrote the earliest seed")
			}
			if meta.ThinkingLevel != mapping.Level || meta.ThinkingLevelRaw != mapping.Raw {
				t.Fatalf("seed changed to {%q %q}", meta.ThinkingLevel, meta.ThinkingLevelRaw)
			}
		})
	}
}
