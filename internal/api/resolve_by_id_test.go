package api_test

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/resolve_by_id.yaml
var resolveByIDFixtureYAML []byte

//go:embed testdata/resolve_by_id.manifest.yaml
var resolveByIDManifestYAML []byte

type resolveByIDCase struct {
	Name      string   `yaml:"name"`
	Stored    []string `yaml:"stored"`
	Requested []string `yaml:"requested"`
	Expected  []string `yaml:"expected"`
}

// resolveByIDElement stands in for a stored row. Its payload records the
// storage position so a case can tell the returned element is the stored one,
// not a value rebuilt from the requested identifier.
type resolveByIDElement struct {
	ID       string
	Position int
}

func loadResolveByIDFixture(t *testing.T) []resolveByIDCase {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(resolveByIDFixtureYAML))
	decoder.KnownFields(true)
	var fixture struct {
		Cases []resolveByIDCase `yaml:"cases"`
	}
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode resolve-by-id fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("resolve-by-id fixture must contain exactly one YAML document: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(resolveByIDManifestYAML, &manifest); err != nil {
		t.Fatalf("decode resolve-by-id manifest: %v", err)
	}
	names := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Name == "" || names[fixtureCase.Name] {
			t.Fatalf("resolve-by-id case %q is missing or duplicated", fixtureCase.Name)
		}
		names[fixtureCase.Name] = true
	}
	if err := testutil.RequireFixtureNames("resolve-by-id", "case", manifest.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func TestResolveByIDFixture(t *testing.T) {
	t.Parallel()
	for _, fixtureCase := range loadResolveByIDFixture(t) {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			stored := make([]resolveByIDElement, len(fixtureCase.Stored))
			positionOf := make(map[string]int, len(fixtureCase.Stored))
			for i, id := range fixtureCase.Stored {
				stored[i] = resolveByIDElement{ID: id, Position: i}
				positionOf[id] = i
			}

			resolved := api.ResolveByID(stored, fixtureCase.Requested, func(element resolveByIDElement) string {
				return element.ID
			})

			if resolved == nil {
				t.Fatal("ResolveByID returned nil; want a non-nil slice so the JSON payload is [] not null")
			}
			gotIDs := make([]string, len(resolved))
			for i, element := range resolved {
				gotIDs[i] = element.ID
				if element.Position != positionOf[element.ID] {
					t.Errorf("element %q carries storage position %d, want %d", element.ID, element.Position, positionOf[element.ID])
				}
			}
			if !slices.Equal(gotIDs, fixtureCase.Expected) {
				t.Fatalf("ResolveByID(%v, %v) = %v, want %v", fixtureCase.Stored, fixtureCase.Requested, gotIDs, fixtureCase.Expected)
			}
		})
	}
}
