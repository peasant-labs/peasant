package e2e

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/schema_parity/cases.yaml
var schemaParityFixtureBytes []byte

type schemaParityFixture struct {
	Cases []schemaParityCase `yaml:"cases"`
}

type schemaParityCase struct {
	Name              string `yaml:"name"`
	PeasantGoMod      string `yaml:"peasant_go_mod"`
	VillageGoMod      string `yaml:"village_go_mod"`
	WantVersion       string `yaml:"want_version"`
	WantErrorContains string `yaml:"want_error_contains"`
}

func TestSchemaModuleParity(t *testing.T) {
	fixture, err := decodeSchemaParityFixture(schemaParityFixtureBytes)
	if err != nil {
		t.Fatalf("schema parity: parse fixture: %v", err)
	}
	if len(fixture.Cases) != 2 {
		t.Fatalf("schema parity: fixture case count = %d, want 2", len(fixture.Cases))
	}

	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			dir := t.TempDir()
			peasantPath := filepath.Join(dir, "peasant.mod")
			villagePath := filepath.Join(dir, "village.mod")
			if err := os.WriteFile(peasantPath, []byte(testCase.PeasantGoMod), 0o600); err != nil {
				t.Fatalf("schema parity: write peasant fixture: %v", err)
			}
			if err := os.WriteFile(villagePath, []byte(testCase.VillageGoMod), 0o600); err != nil {
				t.Fatalf("schema parity: write village fixture: %v", err)
			}

			gotVersion, err := schemaModuleParity(peasantPath, villagePath)
			if testCase.WantErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.WantErrorContains) {
					t.Fatalf("schema parity: error = %v, want substring %q", err, testCase.WantErrorContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("schema parity: unexpected error: %v", err)
			}
			if gotVersion != testCase.WantVersion {
				t.Fatalf("schema parity: version = %q, want %q", gotVersion, testCase.WantVersion)
			}
		})
	}
}

func decodeSchemaParityFixture(data []byte) (schemaParityFixture, error) {
	var fixture schemaParityFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return schemaParityFixture{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return schemaParityFixture{}, fmt.Errorf("schema parity fixture must contain exactly one YAML document")
	}
	return fixture, nil
}

func TestSchemaModuleParityFixtureStrictDecoding(t *testing.T) {
	mutated := append([]byte("unexpected_fixture_field: true\n"), schemaParityFixtureBytes...)
	if _, err := decodeSchemaParityFixture(mutated); err == nil || !strings.Contains(err.Error(), "field unexpected_fixture_field not found") {
		t.Fatalf("unknown field error = %v, want strict field rejection", err)
	}

	trailingDocument := append(append([]byte{}, schemaParityFixtureBytes...), []byte("\n---\nunexpected: document\n")...)
	if _, err := decodeSchemaParityFixture(trailingDocument); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error = %v, want single-document rejection", err)
	}
}
