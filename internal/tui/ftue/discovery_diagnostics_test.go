package ftue

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/discovery_diagnostics.yaml
var discoveryDiagnosticsYAML []byte

type discoveryDiagnosticsDocument struct {
	DeclaredRows int                           `yaml:"declaredRows"`
	Cases        []discoveryDiagnosticsFixture `yaml:"cases"`
}

type discoveryDiagnosticsFixture struct {
	Name     string         `yaml:"name"`
	State    DiscoveryState `yaml:"state"`
	Detail   string         `yaml:"detail"`
	Expected string         `yaml:"expected"`
}

func loadDiscoveryDiagnosticsFixtures(raw []byte) ([]discoveryDiagnosticsFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var document discoveryDiagnosticsDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode discovery diagnostics fixture: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("discovery diagnostics fixture must contain exactly one YAML document")
	}
	if document.DeclaredRows != len(document.Cases) || document.DeclaredRows < 2 {
		return nil, fmt.Errorf("discovery diagnostics fixture row count = declared %d actual %d; need at least 2", document.DeclaredRows, len(document.Cases))
	}
	seen := make(map[string]bool, len(document.Cases))
	for _, fixture := range document.Cases {
		if fixture.Name == "" || seen[fixture.Name] || fixture.State.IsOperational() || fixture.Detail == "" || fixture.Expected == "" {
			return nil, fmt.Errorf("discovery diagnostics fixture has blank, duplicate, or operational row %q", fixture.Name)
		}
		seen[fixture.Name] = true
	}
	return document.Cases, nil
}

func TestProjectScopeShowsNonOperationalHarnessDiagnostics(t *testing.T) {
	fixtures, err := loadDiscoveryDiagnosticsFixtures(discoveryDiagnosticsYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			page := NewProjectScopePage(nil, ProviderInventory{
				defaults.HarnessClaudeCode: {State: fixture.State, Detail: fixture.Detail},
			}, nil, false)
			if got := page.View(80, 24); !strings.Contains(got, fixture.Expected) {
				t.Fatalf("mounted scope omitted discovery diagnostic %q:\n%s", fixture.Expected, got)
			}
			if len(page.ProviderSelections()) != 0 {
				t.Fatalf("non-operational harness was selected: %+v", page.ProviderSelections())
			}
			updated, _ := page.Update(tea.KeyMsg{Type: tea.KeyTab})
			page = updated.(*ProjectScopePage)
			updated, _ = page.Update(tea.KeyMsg{Type: tea.KeySpace})
			page = updated.(*ProjectScopePage)
			if len(page.ProviderSelections()) != 0 {
				t.Fatalf("non-operational harness became selectable: %+v", page.ProviderSelections())
			}
		})
	}
}

func TestDiscoveryDiagnosticsFixtureIsStrict(t *testing.T) {
	if _, err := loadDiscoveryDiagnosticsFixtures(append(discoveryDiagnosticsYAML, []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("loader accepted a second YAML document")
	}
}
