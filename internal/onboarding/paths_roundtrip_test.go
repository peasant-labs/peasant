package onboarding_test

import (
	_ "embed"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/paths_roundtrip.yaml
var pathsRoundtripData []byte

type pathFieldFixture struct {
	Key             string   `yaml:"key"`
	Path            []string `yaml:"path"`
	Destination     string   `yaml:"destination"`
	YAMLType        string   `yaml:"yamlType"`
	Selection       bool     `yaml:"selection"`
	Bools           []bool   `yaml:"bools"`
	Options         []string `yaml:"options"`
	NeverInDocument bool     `yaml:"neverInDocument"`
}

type pathDocument struct {
	RequiredNames []string           `yaml:"requiredNames"`
	Fields        []pathFieldFixture `yaml:"fields"`
	Cases         []struct {
		Name string `yaml:"name"`
	} `yaml:"cases"`
}

func TestPathsRoundTrip(t *testing.T) {
	t.Parallel()
	var document pathDocument
	decodeFixture(t, "paths_roundtrip.yaml", pathsRoundtripData, &document)
	names := make([]string, 0, len(document.Fields)+len(document.Cases))
	for _, field := range document.Fields {
		names = append(names, field.Key)
	}
	for _, row := range document.Cases {
		names = append(names, row.Name)
	}
	requireNames(t, document.RequiredNames, names)
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	if len(model.Fields()) < len(document.Fields) {
		t.Fatalf("model fields = %d, fixture config fields = %d", len(model.Fields()), len(document.Fields))
	}
	byKey := map[string]onboarding.Field{}
	for _, field := range model.Fields() {
		byKey[field.Key().String()] = field
	}
	for _, fixture := range document.Fields {
		field, ok := byKey[fixture.Key]
		if !ok {
			t.Fatalf("model has no field %s", fixture.Key)
		}
		if field.Destination().String() != fixture.Destination {
			t.Errorf("%s destination = %s, want %s", fixture.Key, field.Destination(), fixture.Destination)
		}
		if err := yamlEqual(field.YAMLPath(), fixture.Path); err != nil {
			t.Errorf("%s path: %v", fixture.Key, err)
		}
		if fixture.Selection {
			assertSelectionRoundTrip(t, model, field, fixture.YAMLType)
			continue
		}
		for _, token := range fixture.Options {
			assertTokenRoundTrip(t, model, field, token, fixture)
		}
		for _, on := range fixture.Bools {
			assertBoolRoundTrip(t, model, field, on, fixture.YAMLType)
		}
	}
	assertLicenseNone(t, model)
	assertRetentionNeverTransient(t, model, byKey["claude-retention-days"])
}

// baseDocument is a document with one key the projection must not disturb. It
// carries the version key so the projected document parses as a configuration.
func baseDocument() yaml.Node {
	return yaml.Node{
		Kind: yaml.DocumentNode,
		Content: []*yaml.Node{{
			Kind: yaml.MappingNode,
			Tag:  "!!map",
			Content: []*yaml.Node{
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "version"},
				{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(defaults.ConfigVersion)},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "untouched"},
				{Kind: yaml.ScalarNode, Tag: "!!str", Value: "keep-me", LineComment: " sibling comment"},
			},
		}},
	}
}

// project writes the answered field's edits into a fresh document and returns
// the document, the node at the field's path, and the configuration the written
// document parses back to. A document the parser refuses fails the test: the
// projection must write what the configuration can read.
func project(t *testing.T, model onboarding.Model, field onboarding.Field, cfg *config.Config) (*yaml.Node, *yaml.Node, *config.Config) {
	t.Helper()
	doc := baseDocument()
	if err := model.ProjectEdits(&doc, config.BaseConfig(), cfg, []onboarding.FieldKey{field.Key()}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Parse(encoded)
	if err != nil {
		t.Fatalf("%s: the projected document does not parse as a configuration: %v\n%s", field.Key(), err, encoded)
	}
	node, ok := onboarding.DocumentPath(&doc, field.YAMLPath())
	if !ok {
		t.Fatalf("%s path %v missing after projection:\n%s", field.Key(), field.YAMLPath(), encoded)
	}
	sibling, ok := onboarding.DocumentPath(&doc, []string{"untouched"})
	if !ok || sibling.Value != "keep-me" || sibling.LineComment != " sibling comment" {
		t.Fatalf("%s: projection disturbed an untouched key: %#v", field.Key(), doc.Content[0].Content)
	}
	return &doc, node, parsed
}

func assertYAMLType(t *testing.T, field onboarding.Field, node *yaml.Node, want string) {
	t.Helper()
	if want == "" {
		t.Fatalf("%s: the fixture names no yamlType for a config field", field.Key())
	}
	if got := node.ShortTag(); got != "!!"+want {
		t.Errorf("%s is written as %s (%q), the configuration gives it type !!%s", field.Key(), got, node.Value, want)
	}
}

func assertSelectionRoundTrip(t *testing.T, model onboarding.Model, field onboarding.Field, yamlType string) {
	t.Helper()
	cfg := config.BaseConfig()
	value, err := onboarding.SelectionMappingValue(config.SelectionModeSelected, map[string]config.SelectionHarnessConfig{
		"claude-code": {Sessions: []string{"one"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := field.Write(cfg, onboarding.WriteContext{Config: cfg}, value); err != nil {
		t.Fatal(err)
	}
	got, err := field.Read(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(value) {
		t.Fatalf("selection read = %s, want %s", got, value)
	}
	_, node, parsed := project(t, model, field, cfg)
	if node.Kind != yaml.MappingNode {
		t.Fatalf("selection document kind = %v, want one mapping", node.Kind)
	}
	assertYAMLType(t, field, node, yamlType)
	back, err := field.Read(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(value) {
		t.Fatalf("selection parsed back as %s, want %s", back, value)
	}
}

func assertTokenRoundTrip(t *testing.T, model onboarding.Model, field onboarding.Field, token string, fixture pathFieldFixture) {
	t.Helper()
	cfg := config.BaseConfig()
	raw := onboarding.StringValue(token)
	if err := field.Write(cfg, onboarding.WriteContext{Config: cfg}, raw); err != nil {
		t.Fatalf("write %s %q: %v", field.Key(), token, err)
	}
	got, err := field.Read(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !sameToken(got, token) {
		t.Fatalf("%s read %s, want token %q", field.Key(), got, token)
	}
	if fixture.NeverInDocument {
		doc := baseDocument()
		if err := model.ProjectEdits(&doc, config.BaseConfig(), cfg, []onboarding.FieldKey{field.Key()}); err != nil {
			t.Fatal(err)
		}
		if _, ok := onboarding.DocumentPath(&doc, []string{"claudeRetentionDays"}); ok {
			t.Fatalf("%s was written into the document", field.Key())
		}
		return
	}
	_, node, parsed := project(t, model, field, cfg)
	assertYAMLType(t, field, node, fixture.YAMLType)
	back, err := field.Read(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if !sameToken(back, token) {
		t.Fatalf("%s parsed back as %s from the document, want token %q", field.Key(), back, token)
	}
}

func assertBoolRoundTrip(t *testing.T, model onboarding.Model, field onboarding.Field, on bool, yamlType string) {
	t.Helper()
	cfg := config.BaseConfig()
	if err := field.Write(cfg, onboarding.WriteContext{Config: cfg}, onboarding.BoolValue(on)); err != nil {
		t.Fatal(err)
	}
	got, err := field.Read(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsedBool, err := onboarding.BoolToken(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsedBool != on {
		t.Fatalf("%s = %t, want %t", field.Key(), parsedBool, on)
	}
	_, node, parsed := project(t, model, field, cfg)
	assertYAMLType(t, field, node, yamlType)
	want := "false"
	if on {
		want = "true"
	}
	if node.Value != want {
		t.Fatalf("%s document = %q, want %q", field.Key(), node.Value, want)
	}
	back, err := field.Read(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if backBool, err := onboarding.BoolToken(back); err != nil || backBool != on {
		t.Fatalf("%s parsed back as %s from the document, want %t", field.Key(), back, on)
	}
}

func assertLicenseNone(t *testing.T, model onboarding.Model) {
	t.Helper()
	field, ok := model.Field(onboarding.FieldLicense)
	if !ok {
		t.Fatal("missing license field")
	}
	cfg := config.BaseConfig()
	cfg.Push.License = "CC0-1.0"
	if err := field.Write(cfg, onboarding.WriteContext{Config: cfg}, onboarding.StringValue("")); err != nil {
		t.Fatal(err)
	}
	if cfg.Push.License != "" {
		t.Fatalf("license = %q, want empty", cfg.Push.License)
	}
	var doc yaml.Node
	if err := model.ProjectEdits(&doc, config.BaseConfig(), cfg, []onboarding.FieldKey{field.Key()}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "none (do not attach a license)") {
		t.Fatalf("document stored the license sentinel label:\n%s", encoded)
	}
}

func assertRetentionNeverTransient(t *testing.T, model onboarding.Model, field onboarding.Field) {
	t.Helper()
	if field == nil {
		t.Fatal("missing retention field")
	}
	if field.Destination() != onboarding.DestClaudeSettings || len(field.YAMLPath()) != 0 {
		t.Fatalf("retention destination/path = %s/%v", field.Destination(), field.YAMLPath())
	}
	cfg := config.BaseConfig()
	if err := field.Write(cfg, onboarding.WriteContext{Config: cfg}, onboarding.StringValue("90")); err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeRetentionDays != 90 {
		t.Fatalf("retention = %d, want 90", cfg.ClaudeRetentionDays)
	}
	var doc yaml.Node
	if err := model.ProjectEdits(&doc, config.BaseConfig(), cfg, []onboarding.FieldKey{field.Key()}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(defaults.LegacyHarnessClaude)) || strings.Contains(string(encoded), "90") {
		t.Fatalf("retention entered the document:\n%s", encoded)
	}
}

func sameToken(value schema.LocalSettingValue, token string) bool {
	if parsed, err := onboarding.StringToken(value); err == nil {
		return parsed == token
	}
	if number, err := onboarding.IntToken(value); err == nil {
		return strconv.Itoa(number) == token
	}
	return false
}
