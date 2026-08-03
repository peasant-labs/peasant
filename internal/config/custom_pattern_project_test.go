package config

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/redact"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/custom_pattern_project.yaml
var customPatternProjectData []byte

type customPatternProjectExpectation struct {
	ID             string                `yaml:"id"`
	ConfigCategory PatternCategory       `yaml:"configCategory"`
	EngineCategory redact.Category       `yaml:"engineCategory"`
	DefaultLevel   redact.RedactionLevel `yaml:"defaultLevel"`
	Input          string                `yaml:"input"`
	MinimalOutput  string                `yaml:"minimalOutput"`
	StandardOutput string                `yaml:"standardOutput"`
}

type customPatternProjectFixture struct {
	Config   string                          `yaml:"config"`
	Expected customPatternProjectExpectation `yaml:"expected"`
}

func loadCustomPatternProjectFixture() (customPatternProjectFixture, error) {
	fixture, err := decodeCustomPatternProjectFixture(customPatternProjectData)
	if err != nil {
		return customPatternProjectFixture{}, err
	}
	expected := fixture.Expected
	if fixture.Config == "" || expected.ID == "" || expected.ConfigCategory != CategoryProject ||
		expected.EngineCategory != redact.CategoryProject || expected.DefaultLevel != redact.Standard ||
		expected.Input == "" || expected.MinimalOutput == "" || expected.StandardOutput == "" {
		return customPatternProjectFixture{}, fmt.Errorf("config: internal/config/testdata/custom_pattern_project.yaml is incomplete or misstates CategoryProject defaults; config, id, project categories, Standard default, input, and outputs are required; restore the load-bearing fixture values")
	}
	return fixture, nil
}

func decodeCustomPatternProjectFixture(data []byte) (customPatternProjectFixture, error) {
	var fixture customPatternProjectFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return customPatternProjectFixture{}, fmt.Errorf("config: could not parse internal/config/testdata/custom_pattern_project.yaml while loading the CategoryProject conversion case: %w; config conversion coverage cannot run; fix the YAML syntax", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return customPatternProjectFixture{}, fmt.Errorf("config: internal/config/testdata/custom_pattern_project.yaml must contain exactly one YAML document")
	}
	return fixture, nil
}

func TestCustomPatternProjectFixtureStrictDecoding(t *testing.T) {
	unknownField := append([]byte("unexpected_fixture_field: true\n"), customPatternProjectData...)
	if _, err := decodeCustomPatternProjectFixture(unknownField); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}

	trailingDocument := append(append([]byte{}, customPatternProjectData...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeCustomPatternProjectFixture(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}

func TestCustomPatternProjectParseConversionAndActivation(t *testing.T) {
	fixture, err := loadCustomPatternProjectFixture()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Parse([]byte(fixture.Config))
	if err != nil {
		t.Fatalf("Parse project custom pattern: %v", err)
	}
	if len(cfg.Redaction.CustomPatterns) != 1 {
		t.Fatalf("len(CustomPatterns) = %d, want 1", len(cfg.Redaction.CustomPatterns))
	}
	configured := cfg.Redaction.CustomPatterns[0]
	if configured.ID != fixture.Expected.ID || configured.Category != fixture.Expected.ConfigCategory {
		t.Errorf("parsed custom pattern ID/category = %q/%q, want %q/%q", configured.ID, configured.Category, fixture.Expected.ID, fixture.Expected.ConfigCategory)
	}

	patterns, err := CustomPatternsToUserPatterns(cfg.Redaction.CustomPatterns)
	if err != nil {
		t.Fatalf("CustomPatternsToUserPatterns: %v", err)
	}
	if len(patterns) != 1 || patterns[0].Category != fixture.Expected.EngineCategory {
		t.Fatalf("converted patterns = %+v, want one pattern with category %q", patterns, fixture.Expected.EngineCategory)
	}

	minimal, err := redact.NewRedactor(redact.Minimal, patterns, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("NewRedactor(minimal): %v", err)
	}
	if got := minimal.RedactText(fixture.Expected.Input); got != fixture.Expected.MinimalOutput {
		t.Errorf("Minimal output = %q, want %q", got, fixture.Expected.MinimalOutput)
	}

	standard, err := redact.NewRedactor(fixture.Expected.DefaultLevel, patterns, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("NewRedactor(%s): %v", fixture.Expected.DefaultLevel, err)
	}
	if got := standard.RedactText(fixture.Expected.Input); got != fixture.Expected.StandardOutput {
		t.Errorf("Standard output = %q, want %q", got, fixture.Expected.StandardOutput)
	}
}
