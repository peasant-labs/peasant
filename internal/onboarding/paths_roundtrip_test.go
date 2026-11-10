package onboarding_test

import (
	_ "embed"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
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
			assertSelectionRoundTrip(t, model, field)
			continue
		}
		for _, token := range fixture.Options {
			assertTokenRoundTrip(t, model, field, token, fixture.NeverInDocument)
		}
		for _, on := range fixture.Bools {
			assertBoolRoundTrip(t, model, field, on)
		}
	}
	assertLicenseNone(t, model)
	assertRetentionNeverTransient(t, model, byKey["claude-retention-days"])
}

func assertSelectionRoundTrip(t *testing.T, model onboarding.Model, field onboarding.Field) {
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
	var doc yaml.Node
	if err := model.ProjectAnswered(&doc, cfg, []onboarding.FieldKey{field.Key()}); err != nil {
		t.Fatal(err)
	}
	node, ok := onboarding.DocumentPath(&doc, field.YAMLPath())
	if !ok {
		t.Fatal("selection path missing from the document")
	}
	if node.Kind != yaml.MappingNode {
		t.Fatalf("selection document kind = %v, want one mapping", node.Kind)
	}
}

func assertTokenRoundTrip(t *testing.T, model onboarding.Model, field onboarding.Field, token string, neverInDocument bool) {
	t.Helper()
	cfg := config.BaseConfig()
	raw := onboarding.StringValue(token)
	if _, err := strconv.Atoi(token); err == nil && field.Destination() == onboarding.DestClaudeSettings {
		raw = onboarding.StringValue(token)
	}
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
	var doc yaml.Node
	doc.Kind = yaml.DocumentNode
	doc.Content = []*yaml.Node{{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "untouched"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "keep-me", LineComment: " sibling comment"},
		},
	}}
	if err := model.ProjectAnswered(&doc, cfg, []onboarding.FieldKey{field.Key()}); err != nil {
		t.Fatal(err)
	}
	if neverInDocument {
		if _, ok := onboarding.DocumentPath(&doc, []string{"claudeRetentionDays"}); ok {
			t.Fatalf("%s was written into the document", field.Key())
		}
		return
	}
	node, ok := onboarding.DocumentPath(&doc, field.YAMLPath())
	if !ok {
		t.Fatalf("%s path %v missing after projection", field.Key(), field.YAMLPath())
	}
	if node.Value != token && token != "" {
		t.Fatalf("%s document value = %q, want %q", field.Key(), node.Value, token)
	}
	sibling, ok := onboarding.DocumentPath(&doc, []string{"untouched"})
	if !ok || sibling.Value != "keep-me" {
		t.Fatalf("projection disturbed an untouched key: %#v", doc.Content[0].Content)
	}
}

func assertBoolRoundTrip(t *testing.T, model onboarding.Model, field onboarding.Field, on bool) {
	t.Helper()
	cfg := config.BaseConfig()
	if err := field.Write(cfg, onboarding.WriteContext{Config: cfg}, onboarding.BoolValue(on)); err != nil {
		t.Fatal(err)
	}
	got, err := field.Read(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := onboarding.BoolToken(got)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != on {
		t.Fatalf("%s = %t, want %t", field.Key(), parsed, on)
	}
	var doc yaml.Node
	if err := model.ProjectAnswered(&doc, cfg, []onboarding.FieldKey{field.Key()}); err != nil {
		t.Fatal(err)
	}
	node, ok := onboarding.DocumentPath(&doc, field.YAMLPath())
	if !ok {
		t.Fatalf("%s path missing", field.Key())
	}
	want := "false"
	if on {
		want = "true"
	}
	if node.Value != want {
		t.Fatalf("%s document = %q, want %q", field.Key(), node.Value, want)
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
	if err := model.ProjectAnswered(&doc, cfg, []onboarding.FieldKey{field.Key()}); err != nil {
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
	if err := model.ProjectAnswered(&doc, cfg, []onboarding.FieldKey{field.Key()}); err != nil {
		t.Fatal(err)
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "claude") || strings.Contains(string(encoded), "90") {
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
