package store

import (
	"embed"
	"io/fs"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/*.yaml
var contentFamilyFiles embed.FS

type contentFamilyManifest struct {
	RequiredNames []string `yaml:"requiredNames"`
	Deferred      []struct {
		Name   string `yaml:"name"`
		Owner  string `yaml:"owner"`
		Ruling string `yaml:"ruling"`
	} `yaml:"deferred"`
}

// TestContentFamilyInventory guards executable fixture shapes, not just names.
// Behavioral runners separately decode every field strictly and reject unknown
// dispatch values. No deferred case is admitted in the current inventory.
func TestContentFamilyInventory(t *testing.T) {
	raw, err := contentFamilyFiles.ReadFile("testdata/content_family_inventory.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var inventory struct {
		RequiredNames []string `yaml:"requiredNames"`
		Families      []struct {
			Name  string `yaml:"name"`
			Owner string `yaml:"owner"`
		} `yaml:"families"`
	}
	if err := yaml.Unmarshal(raw, &inventory); err != nil {
		t.Fatal(err)
	}
	var names []string
	owned := map[string]bool{}
	for _, family := range inventory.Families {
		if strings.TrimSpace(family.Owner) == "" {
			t.Fatalf("family %s has no behavioral runner owner", family.Name)
		}
		names = append(names, family.Name)
		owned[family.Name] = true
		t.Run(family.Name, func(t *testing.T) {
			fixture, err := contentFamilyFiles.ReadFile("testdata/" + family.Name + ".yaml")
			if err != nil {
				t.Fatal(err)
			}
			manifestRaw, err := contentFamilyFiles.ReadFile("testdata/" + family.Name + ".manifest.yaml")
			if err != nil {
				t.Fatal(err)
			}
			var manifest contentFamilyManifest
			if err := yaml.Unmarshal(manifestRaw, &manifest); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Cases []yaml.Node `yaml:"cases"`
			}
			if err := yaml.Unmarshal(fixture, &envelope); err != nil {
				t.Fatal(err)
			}
			deferred := map[string]bool{}
			for _, entry := range manifest.Deferred {
				if strings.TrimSpace(entry.Owner) == "" || strings.TrimSpace(entry.Ruling) == "" || deferred[entry.Name] {
					t.Fatalf("invalid or repeated deferral %+v", entry)
				}
				deferred[entry.Name] = true
			}
			var actual []string
			for _, node := range envelope.Cases {
				if node.Kind != yaml.MappingNode {
					t.Fatal("fixture case is not a typed mapping")
				}
				var name string
				executable := false
				for i := 0; i < len(node.Content); i += 2 {
					key := node.Content[i].Value
					if key == "name" {
						name = node.Content[i+1].Value
					} else if key != "why" && key != "ownedBy" {
						executable = true
					}
				}
				actual = append(actual, name)
				if !executable && !deferred[name] {
					t.Fatalf("case %s has no typed stimulus or expectation; owner %s must implement it", name, family.Owner)
				}
				if executable && deferred[name] {
					t.Fatalf("case %s has typed fields but retains a stale deferral", name)
				}
			}
			if err := validateRecoveryRequiredNames(manifest.RequiredNames, actual, family.Name); err != nil {
				t.Fatal(err)
			}
			for name := range deferred {
				found := false
				for _, present := range actual {
					if present == name {
						found = true
					}
				}
				if !found {
					t.Fatalf("deferral names absent case %s", name)
				}
			}
			if len(deferred) != 0 {
				t.Fatalf("family %s retains deferred cases; the current contract admits none", family.Name)
			}
		})
	}
	if err := validateRecoveryRequiredNames(inventory.RequiredNames, names, "content family inventory"); err != nil {
		t.Fatal(err)
	}
	paths, err := fs.Glob(contentFamilyFiles, "testdata/content_*.manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		family := strings.TrimSuffix(strings.TrimPrefix(path, "testdata/"), ".manifest.yaml")
		if !owned[family] {
			t.Fatalf("content family %s has no inventory owner", family)
		}
	}
}
