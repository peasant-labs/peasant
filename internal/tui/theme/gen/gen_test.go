package gen_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/theme/gen"
)

//go:embed testdata/field_names.yaml
var fieldNameFixtureData []byte

type fieldNameDocument struct {
	ExpectedCaseCount int             `yaml:"expectedCaseCount"`
	Cases             []fieldNameCase `yaml:"cases"`
}

type fieldNameCase struct {
	Name      string `yaml:"name"`
	TokenName string `yaml:"tokenName"`
	Want      string `yaml:"want"`
}

// loadFieldNameFixture decodes and validates testdata/field_names.yaml,
// mirroring the embed+KnownFields+single-document+declared-count idiom
// internal/config/level_phrases_test.go establishes.
func loadFieldNameFixture(data []byte) (fieldNameDocument, error) {
	var doc fieldNameDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/field_names.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/field_names.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf(
			"testdata/field_names.yaml: expectedCaseCount=%d but found %d cases (and must be non-zero)",
			doc.ExpectedCaseCount, len(doc.Cases))
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("testdata/field_names.yaml: case name %q is missing or duplicated", c.Name)
		}
		seen[c.Name] = true
	}
	return doc, nil
}

func TestFieldName(t *testing.T) {
	t.Parallel()
	doc, err := loadFieldNameFixture(fieldNameFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			if got := gen.FieldName(c.TokenName); got != c.Want {
				t.Errorf("FieldName(%q) = %q, want %q", c.TokenName, got, c.Want)
			}
		})
	}
}

func TestParseColorTokens_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	_, err := gen.ParseColorTokens([]byte(`not json`))
	if err == nil || !strings.Contains(err.Error(), "malformed fairtrade tokens.json") {
		t.Fatalf("error = %v, want rejection of malformed JSON", err)
	}
}

func TestParseColorTokens_RejectsEmptyColorGroup(t *testing.T) {
	t.Parallel()
	_, err := gen.ParseColorTokens([]byte(`{"color": {}}`))
	if err == nil || !strings.Contains(err.Error(), "color\" group is empty") {
		t.Fatalf("error = %v, want rejection of an empty color group", err)
	}
	_, err = gen.ParseColorTokens([]byte(`{"space": {"sp-1": {"$value": "4px"}}}`))
	if err == nil || !strings.Contains(err.Error(), "color\" group is empty") {
		t.Fatalf("error = %v, want rejection when the color group is entirely absent", err)
	}
}

func TestParseColorTokens_FailsClosedOnMissingDarkValue(t *testing.T) {
	t.Parallel()
	_, err := gen.ParseColorTokens([]byte(`{
		"color": {
			"canvas": {
				"$type": "color",
				"$value": "",
				"$extensions": {"fairtrade.theme": {"light": "#fbfaf7"}}
			}
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "no dark value") {
		t.Fatalf("error = %v, want fail-closed rejection of a token missing its dark $value", err)
	}
}

func TestParseColorTokens_FailsClosedOnMissingLightValue(t *testing.T) {
	t.Parallel()
	_, err := gen.ParseColorTokens([]byte(`{
		"color": {
			"canvas": {
				"$type": "color",
				"$value": "#070706",
				"$extensions": {"fairtrade.theme": {}}
			}
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "no light value") {
		t.Fatalf("error = %v, want fail-closed rejection of a token missing its light extension", err)
	}
}

func TestParseColorTokens_SortsDeterministicallyByName(t *testing.T) {
	t.Parallel()
	input := []byte(`{
		"color": {
			"zeta": {"$value": "#000000", "$extensions": {"fairtrade.theme": {"light": "#ffffff"}}},
			"alpha": {"$value": "#010101", "$extensions": {"fairtrade.theme": {"light": "#fefefe"}}}
		}
	}`)
	got, err := gen.ParseColorTokens(input)
	if err != nil {
		t.Fatalf("ParseColorTokens: %v", err)
	}
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("ParseColorTokens did not sort tokens by name: %+v", got)
	}
}

func TestGeneratePaletteGo_RejectsEmptyTokens(t *testing.T) {
	t.Parallel()
	_, err := gen.GeneratePaletteGo(nil)
	if err == nil || !strings.Contains(err.Error(), "zero tokens") {
		t.Fatalf("error = %v, want rejection of an empty token slice", err)
	}
}

func TestGeneratePaletteGo_IsDeterministic(t *testing.T) {
	t.Parallel()
	tokens := []gen.Token{
		{Name: "canvas", Dark: "#070706", Light: "#fbfaf7"},
		{Name: "amber-fill-ink", Dark: "#141003", Light: "#1a1206"},
	}
	first, err := gen.GeneratePaletteGo(tokens)
	if err != nil {
		t.Fatalf("GeneratePaletteGo: %v", err)
	}
	second, err := gen.GeneratePaletteGo(tokens)
	if err != nil {
		t.Fatalf("GeneratePaletteGo: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("GeneratePaletteGo is not deterministic across identical inputs:\n---first---\n%s\n---second---\n%s", first, second)
	}
	if !strings.Contains(string(first), "type Palette struct") {
		t.Fatalf("generated source does not declare the Palette type: %s", first)
	}
	if !strings.Contains(string(first), "var GeneratedPalette = Palette{") {
		t.Fatalf("generated source does not declare GeneratedPalette: %s", first)
	}
	if !strings.Contains(string(first), "Code generated by cmd/gen-terminal-palette. DO NOT EDIT.") {
		t.Fatalf("generated source is missing the DO NOT EDIT header: %s", first)
	}
}

func TestGeneratePaletteGo_RejectsFieldNameCollision(t *testing.T) {
	t.Parallel()
	// "amber-fill" and "amber--fill" both map to the Go field "AmberFill" (an
	// empty hyphen-separated segment is simply skipped), so this proves
	// collisions are caught rather than silently overwriting a struct field.
	tokens := []gen.Token{
		{Name: "amber-fill", Dark: "#111111", Light: "#eeeeee"},
		{Name: "amber--fill", Dark: "#222222", Light: "#dddddd"},
	}
	_, err := gen.GeneratePaletteGo(tokens)
	if err == nil || !strings.Contains(err.Error(), "both map to the Go field") {
		t.Fatalf("error = %v, want rejection of a Go field name collision", err)
	}
}
