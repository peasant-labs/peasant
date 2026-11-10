package onboarding_test

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/peasant/internal/onboarding"
)

//go:embed testdata/registry_parity.yaml
var registryParityData []byte

type registryOptionFixture struct {
	Token       string `yaml:"token"`
	Label       string `yaml:"label"`
	Description string `yaml:"description"`
}

type registrySectionFixture struct {
	Key                  string                  `yaml:"key"`
	Title                string                  `yaml:"title"`
	FieldKey             string                  `yaml:"fieldKey"`
	FieldKind            string                  `yaml:"fieldKind"`
	ModelKind            string                  `yaml:"modelKind"`
	FieldText            string                  `yaml:"fieldText"`
	HasGuide             bool                    `yaml:"hasGuide"`
	FieldTextBeforeGuide bool                    `yaml:"fieldTextBeforeGuide"`
	GuideIntro           string                  `yaml:"guideIntro"`
	HasExample           bool                    `yaml:"hasExample"`
	Conditional          bool                    `yaml:"conditional"`
	Options              []registryOptionFixture `yaml:"options"`
}

type registryParityDocument struct {
	ExpectedSectionCount int                      `yaml:"expectedSectionCount"`
	RequiredNames        []string                 `yaml:"requiredNames"`
	Sections             []registrySectionFixture `yaml:"sections"`
	Forbidden            map[string]any           `yaml:"forbiddenRegistryOptions"`
	ExpectedRowCount     int                      `yaml:"expectedRowCount"`
	Rows                 []map[string]any         `yaml:"rows"`
}

func TestRegistryParity(t *testing.T) {
	t.Parallel()
	var document registryParityDocument
	decodeFixture(t, "registry_parity.yaml", registryParityData, &document)
	names := make([]string, len(document.Sections))
	for i, section := range document.Sections {
		names[i] = section.Key
	}
	requireNames(t, document.RequiredNames, names)
	if document.ExpectedSectionCount != 8 || len(document.Sections) != 8 {
		t.Fatalf("section count declared=%d actual=%d", document.ExpectedSectionCount, len(document.Sections))
	}
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeFull})
	sections := model.SectionStops()
	if len(sections) != len(document.Sections) {
		t.Fatalf("model sections = %d, want %d", len(sections), len(document.Sections))
	}
	for i, fixture := range document.Sections {
		stop := sections[i]
		if stop.Key.String() != fixture.Key || stop.Title != fixture.Title {
			t.Errorf("section %d = %s/%q, want %s/%q", i, stop.Key, stop.Title, fixture.Key, fixture.Title)
		}
		if len(stop.Fields) != 1 {
			t.Fatalf("section %s has %d fields", fixture.Key, len(stop.Fields))
		}
		field := stop.Fields[0]
		if field.Key().String() != fixture.FieldKey {
			t.Errorf("section %s field = %s, want %s", fixture.Key, field.Key(), fixture.FieldKey)
		}
		if field.Kind().String() != fixture.ModelKind {
			t.Errorf("section %s kind = %s, want %s", fixture.Key, field.Kind(), fixture.ModelKind)
		}
		options := field.Options()
		if len(options) != len(fixture.Options) {
			t.Errorf("section %s options = %d, want %d", fixture.Key, len(options), len(fixture.Options))
			continue
		}
		for j, want := range fixture.Options {
			got := options[j]
			if got.Value != want.Token || got.Label != want.Label || got.Description != want.Description {
				t.Errorf("section %s option %d = %#v, want %#v", fixture.Key, j, got, want)
			}
		}
	}
}
