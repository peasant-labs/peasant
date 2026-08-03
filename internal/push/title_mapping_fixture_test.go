package push_test

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/title_mapping.yaml
var titleMappingFixtureYAML []byte

type titleMappingFixture struct {
	Name                      string   `yaml:"name"`
	CanonicalTitle            string   `yaml:"canonical_title"`
	CustomMatch               string   `yaml:"custom_match"`
	CustomReplacement         string   `yaml:"custom_replacement"`
	XDGConfigHome             string   `yaml:"xdg_config_home"`
	ExpectedTitle             string   `yaml:"expected_title"`
	ExpectedNonTitleFragments []string `yaml:"expected_non_title_fragments"`
}

func loadTitleMappingFixture(t *testing.T) titleMappingFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(titleMappingFixtureYAML))
	decoder.KnownFields(true)
	var fixture titleMappingFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode title mapping fixture: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("title mapping fixture must contain one document: %v", err)
	}
	if fixture.Name == "" || fixture.CanonicalTitle == "" || fixture.CustomMatch == "" || fixture.XDGConfigHome == "" || len(fixture.ExpectedNonTitleFragments) != 2 {
		t.Fatalf("title mapping fixture is incomplete: %#v", fixture)
	}
	return fixture
}

func TestMapMetadata_RuntimeRulesCoverCanonicalTitle(t *testing.T) {
	fixture := loadTitleMappingFixture(t)
	redactor, err := redact.NewRedactor(redact.Standard, []redact.UserPattern{{
		ID: "configured-project-name", Category: redact.CategoryProject,
		Pattern: fixture.CustomMatch, Replacement: fixture.CustomReplacement,
	}}, redact.XDGPaths{ConfigHome: fixture.XDGConfigHome})
	if err != nil {
		t.Fatalf("construct runtime document redactor: %v", err)
	}
	meta := fixtureMetadata()
	meta.Project.Name = fixture.CustomMatch
	meta.Source.FilePath = fixture.XDGConfigHome + "/settings.yaml"
	metrics := &schema.QualityMetrics{TitleGenerated: &fixture.CanonicalTitle}
	opts := mapOpts(meta, metrics, nil)
	opts.Redactor = redactor
	payload, err := push.MapMetadata(opts)
	if err != nil {
		t.Fatalf("MapMetadata: %v", err)
	}
	request := parsePublishRequest(t, payload)
	quality := request["quality"].(map[string]any)
	if got := quality["titleGenerated"]; got != fixture.ExpectedTitle {
		t.Fatalf("canonical title = %v, want %q", got, fixture.ExpectedTitle)
	}
	project := request["project"].(map[string]any)
	source := request["source"].(map[string]any)
	if project["name"] != fixture.ExpectedNonTitleFragments[0] {
		t.Fatalf("custom rule result = %v, want %q", project["name"], fixture.ExpectedNonTitleFragments[0])
	}
	if got, _ := source["filePath"].(string); got == fixture.XDGConfigHome+"/settings.yaml" || !bytes.Contains([]byte(got), []byte(fixture.ExpectedNonTitleFragments[1])) {
		t.Fatalf("XDG path result = %q, want redacted path containing %q", got, fixture.ExpectedNonTitleFragments[1])
	}
}
