package store

import (
	_ "embed"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/upgrade_stamp_effects.yaml
var upgradeStampEffectsYAML []byte

//go:embed testdata/upgrade_stamp_effects.manifest.yaml
var upgradeStampEffectsManifestYAML []byte

func TestUpgradeStampEffects(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name    string      `yaml:"name"`
			Version int         `yaml:"version"`
			Effect  StampEffect `yaml:"effect"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(upgradeStampEffectsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(upgradeStampEffectsManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	effects := make(map[StampEffect]bool)
	for _, c := range fixture.Cases {
		if names[c.Name] {
			t.Fatalf("duplicate case %q", c.Name)
		}
		names[c.Name] = true
		effects[c.Effect] = true
		entry, ok := upgradeStepCatalog[c.Version]
		if !ok || entry.stampEffect != c.Effect {
			t.Errorf("migration %d effect = %q (present %t), want %q", c.Version, entry.stampEffect, ok, c.Effect)
		}
	}
	for _, name := range manifest.RequiredNames {
		if !names[name] {
			t.Errorf("required case %q missing", name)
		}
	}
	for _, effect := range AllStampEffects {
		if !effects[effect] {
			t.Errorf("no case for effect %q", effect)
		}
	}
	for version, entry := range upgradeStepCatalog {
		if !slices.Contains(AllStampEffects, entry.stampEffect) {
			t.Errorf("migration %d has unknown effect %q", version, entry.stampEffect)
		}
	}
}
