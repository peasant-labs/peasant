package ftue

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/schema"
)

//go:embed testdata/provider_rows.yaml
var providerRowsYAML []byte

//go:embed testdata/provider_rows.manifest.yaml
var providerRowsManifestYAML []byte

// providerRowsPage names the first-run page a case mounts.
type providerRowsPage string

const (
	providerRowsProviderSelect providerRowsPage = "provider-select"
	providerRowsProjectScope   providerRowsPage = "project-scope"
)

// providerRowState is how a page shows one harness.
type providerRowState string

const (
	providerRowListedChecked   providerRowState = "listed-checked"
	providerRowListedUnchecked providerRowState = "listed-unchecked"
	providerRowOmitted         providerRowState = "omitted"
)

type providerRowsInventoryEntry struct {
	Harness  string `yaml:"harness"`
	Sessions int    `yaml:"sessions"`
}

type providerRowsExpectation struct {
	Harness string           `yaml:"harness"`
	Want    providerRowState `yaml:"want"`
}

type providerRowsRendered struct {
	Contains []string `yaml:"contains"`
	Omits    []string `yaml:"omits"`
}

type providerRowsCase struct {
	Name      string                       `yaml:"name"`
	Page      providerRowsPage             `yaml:"page"`
	Inventory []providerRowsInventoryEntry `yaml:"inventory"`
	Rows      []providerRowsExpectation    `yaml:"rows"`
	Rendered  providerRowsRendered         `yaml:"rendered"`
	Mutation  string                       `yaml:"mutation"`
}

type providerRowsFixture struct {
	Cases []providerRowsCase `yaml:"cases"`
}

func decodeProviderRowsYAML(t *testing.T, label string, data []byte, into any) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("%s must contain exactly one YAML document: %v", label, err)
	}
}

func loadProviderRowsFixture(t *testing.T) providerRowsFixture {
	t.Helper()
	var fixture providerRowsFixture
	decodeProviderRowsYAML(t, "provider rows fixture", providerRowsYAML, &fixture)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decodeProviderRowsYAML(t, "provider rows manifest", providerRowsManifestYAML, &manifest)
	present := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if present[fixtureCase.Name] {
			t.Fatalf("provider rows case %q is declared twice", fixtureCase.Name)
		}
		if fixtureCase.Mutation == "" || len(fixtureCase.Rows) == 0 {
			t.Fatalf("provider rows case %q needs rows and a mutation", fixtureCase.Name)
		}
		present[fixtureCase.Name] = true
	}
	for _, required := range manifest.RequiredNames {
		if !present[required] {
			t.Errorf("required provider rows case %q is missing from the fixture", required)
		}
	}
	return fixture
}

// parseFixtureHarness reads a fixture harness value through the harness parser
// and requires a known value, so a typo cannot pass as an omitted harness.
func parseFixtureHarness(t *testing.T, value string) schema.Harness {
	t.Helper()
	var harness schema.Harness
	if err := harness.UnmarshalText([]byte(value)); err != nil || !harness.IsKnown() {
		t.Fatalf("fixture harness %q is not a known harness value (%v)", value, err)
	}
	return harness
}

// mountProviderRowsPage builds the case's page through its production
// constructor and returns its harness rows and its plain rendered view.
func mountProviderRowsPage(t *testing.T, fixtureCase providerRowsCase) ([]providerEntry, string) {
	t.Helper()
	inventory := make(ProviderInventory, len(fixtureCase.Inventory))
	for _, entry := range fixtureCase.Inventory {
		inventory[parseFixtureHarness(t, entry.Harness)] = ProviderDiscovery{SessionCount: entry.Sessions, Enabled: true}
	}
	switch fixtureCase.Page {
	case providerRowsProviderSelect:
		page := NewProviderSelectPage("Select Providers", "desc", inventory)
		return page.providers, ansi.Strip(page.View(100, 40))
	case providerRowsProjectScope:
		page := NewProjectScopePage(nil, inventory, nil, false)
		return page.harnesses, ansi.Strip(page.View(120, 40))
	default:
		t.Fatalf("case %q: unknown page %q; use %q or %q", fixtureCase.Name, fixtureCase.Page, providerRowsProviderSelect, providerRowsProjectScope)
		return nil, ""
	}
}

func TestFirstRunPagesListOnlyHarvestableHarnesses(t *testing.T) {
	t.Parallel()
	for _, fixtureCase := range loadProviderRowsFixture(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			rows, view := mountProviderRowsPage(t, fixtureCase)
			for _, expected := range fixtureCase.Rows {
				harness := parseFixtureHarness(t, expected.Harness)
				var found *providerEntry
				for i := range rows {
					if rows[i].provider == harness {
						found = &rows[i]
						break
					}
				}
				switch expected.Want {
				case providerRowOmitted:
					if found != nil {
						t.Errorf("%s lists %q (red under: %s)", fixtureCase.Page, harness, fixtureCase.Mutation)
					}
				case providerRowListedChecked, providerRowListedUnchecked:
					wantChecked := expected.Want == providerRowListedChecked
					if found == nil {
						t.Errorf("%s omits %q (red under: %s)", fixtureCase.Page, harness, fixtureCase.Mutation)
					} else if found.checked != wantChecked {
						t.Errorf("%s shows %q checked=%v, want %v (red under: %s)", fixtureCase.Page, harness, found.checked, wantChecked, fixtureCase.Mutation)
					}
				default:
					t.Fatalf("case %q: unknown row state %q", fixtureCase.Name, expected.Want)
				}
			}
			for _, text := range fixtureCase.Rendered.Contains {
				if !strings.Contains(view, text) {
					t.Errorf("%s view does not contain %q (red under: %s):\n%s", fixtureCase.Page, text, fixtureCase.Mutation, view)
				}
			}
			for _, text := range fixtureCase.Rendered.Omits {
				if strings.Contains(view, text) {
					t.Errorf("%s view contains %q (red under: %s):\n%s", fixtureCase.Page, text, fixtureCase.Mutation, view)
				}
			}
		})
	}
}
