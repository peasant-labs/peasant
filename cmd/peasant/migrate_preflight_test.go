package main

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/cli/migrate_preflight.yaml
var migratePreflightYAML []byte

//go:embed testdata/cli/migrate_preflight.manifest.yaml
var migratePreflightManifestYAML []byte

type migratePreflightCase struct {
	Name          string   `yaml:"name"`
	StoreBytes    int64    `yaml:"storeBytes"`
	FreeBytes     int64    `yaml:"freeBytes"`
	RequiredBytes int64    `yaml:"requiredBytes"`
	DiskOK        bool     `yaml:"diskOK"`
	Refuses       bool     `yaml:"refuses"`
	TextContains  []string `yaml:"textContains"`
}

func TestMigratePreflightFixture(t *testing.T) {
	var fixture struct {
		Cases []migratePreflightCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(migratePreflightYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(migratePreflightManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, c := range fixture.Cases {
		if present[c.Name] || c.Name == "" {
			t.Fatal("duplicate or blank name")
		}
		present[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("migrate_preflight", "case", manifest.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			plan := store.MigratePlan{StoreBytes: c.StoreBytes, DiskFreeBytes: c.FreeBytes, DiskRequiredBytes: c.RequiredBytes, DiskFreeOK: c.DiskOK, Advisory: "no other writer running is recommended"}
			err := plan.CheckDisk()
			if (err != nil) != c.Refuses {
				t.Fatalf("CheckDisk = %v, refuses=%v", err, c.Refuses)
			}
			if err != nil && !strings.Contains(err.Error(), "nothing was migrated") {
				t.Fatal(err)
			}
			var out bytes.Buffer
			writeMigratePreflight(&out, plan)
			for _, text := range c.TextContains {
				if !strings.Contains(out.String(), text) {
					t.Errorf("missing %q: %s", text, out.String())
				}
			}
		})
	}
}
