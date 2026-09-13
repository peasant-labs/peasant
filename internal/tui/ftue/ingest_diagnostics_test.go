package ftue

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/ingest_diagnostics.yaml
var ingestDiagnosticsData []byte

type legacyDiagnosticDocument struct {
	RequiredNames []string `yaml:"requiredNames"`
	Viewports     []struct {
		Width  int `yaml:"width"`
		Height int `yaml:"height"`
	} `yaml:"viewports"`
	Cases []struct {
		Name        string                   `yaml:"name"`
		Diagnostics []ingest.DiagnosticEntry `yaml:"diagnostics"`
		WantText    []string                 `yaml:"wantText"`
		WantAbsent  []string                 `yaml:"wantAbsent"`
	} `yaml:"cases"`
}

func loadLegacyDiagnosticFixtures(t *testing.T) legacyDiagnosticDocument {
	t.Helper()
	var fixture legacyDiagnosticDocument
	decoder := yaml.NewDecoder(bytes.NewReader(ingestDiagnosticsData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("legacy diagnostic fixture must contain exactly one document")
	}
	required := []string{"warning-details", "long-warning-controls", "no-warning-control"}
	if !reflect.DeepEqual(fixture.RequiredNames, required) {
		t.Fatal("legacy diagnostic manifest changed")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid diagnostic case %q", row.Name)
		}
		seen[row.Name] = true
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing diagnostic fixture %q", name)
		}
	}
	sizes := map[string]bool{}
	for _, size := range fixture.Viewports {
		name := fmt.Sprintf("%dx%d", size.Width, size.Height)
		if size.Width <= 0 || size.Height <= 0 || sizes[name] {
			t.Fatalf("invalid diagnostic viewport %q", name)
		}
		sizes[name] = true
	}
	if !sizes["80x24"] || !sizes["120x40"] {
		t.Fatal("diagnostic fixtures must retain 80x24 and 120x40 viewports")
	}
	return fixture
}

func TestLegacyIngestCompletionRetainsWarnings(t *testing.T) {
	fixture := loadLegacyDiagnosticFixtures(t)
	for _, row := range fixture.Cases {
		for _, size := range fixture.Viewports {
			t.Run(fmt.Sprintf("%s/%dx%d", row.Name, size.Width, size.Height), func(t *testing.T) {
				result := &IngestResult{New: 2, Updated: 1, Unchanged: 3, Errors: 1, Diagnostics: row.Diagnostics}
				page := NewIngestPage("Local import")
				command := page.Start(func(context.Context, WizardAnswers) (*IngestResult, error) { return result, nil }, &WizardAnswers{}, nil)
				batch := command().(tea.BatchMsg)
				page.Update(batch[0]())
				if page.IsComplete() {
					t.Fatal("warning auto-completed setup")
				}
				var evidence strings.Builder
				for index := 0; index < 80; index++ {
					view := ansi.Strip(page.View(size.Width, size.Height))
					if len(row.Diagnostics) > 0 && (len(strings.Split(view, "\n")) > size.Height || !strings.Contains(view, "finish setup")) {
						t.Fatalf("legacy warning completion overflow/lost continuation:\n%s", view)
					}
					evidence.WriteString(view)
					evidence.WriteByte('\n')
					page.Update(tea.KeyPressMsg{Code: tea.KeyDown})
				}
				text := strings.Join(strings.Fields(evidence.String()), " ")
				for _, want := range row.WantText {
					if !strings.Contains(text, want) {
						t.Errorf("legacy completion lost %q", want)
					}
				}
				for _, absent := range row.WantAbsent {
					if strings.Contains(text, absent) {
						t.Errorf("legacy completion contains %q", absent)
					}
				}
				page.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if !page.IsComplete() {
					t.Fatal("warning prevented explicit continuation")
				}
			})
		}
	}
}

func TestLegacyJourneyReceiptRetainsWarningHistory(t *testing.T) {
	fixture := loadLegacyDiagnosticFixtures(t)
	for _, row := range fixture.Cases {
		for _, size := range fixture.Viewports {
			t.Run(fmt.Sprintf("%s/%dx%d", row.Name, size.Width, size.Height), func(t *testing.T) {
				result := JourneyResult{Effects: []PersistedEffect{{Stage: StageIngest, Status: StatusSkipped, Detail: "0 new, 0 updated, 1 unchanged, 0 errors"}}, Diagnostics: row.Diagnostics}
				wizard := NewWizard(WithJourneyRunner(JourneyRunnerFunc(func(context.Context, JourneyRequest) (JourneyResult, error) { return result, nil })))
				model, _ := wizard.Update(tea.WindowSizeMsg{Width: size.Width, Height: size.Height})
				wizard = model.(WizardModel)
				model, _ = wizard.Update(wizard.startJourney(nil, nil)())
				wizard = model.(WizardModel)
				var evidence strings.Builder
				for index := 0; index < 80; index++ {
					view := ansi.Strip(wizard.View().Content)
					if len(row.Diagnostics) > 0 && (len(strings.Split(view, "\n")) > size.Height || !strings.Contains(view, "enter: finish")) {
						t.Fatalf("legacy receipt overflow/lost continuation:\n%s", view)
					}
					evidence.WriteString(view)
					evidence.WriteByte('\n')
					model, _ = wizard.Update(tea.KeyPressMsg{Code: tea.KeyDown})
					wizard = model.(WizardModel)
				}
				text := strings.Join(strings.Fields(evidence.String()), " ")
				for _, want := range row.WantText {
					if !strings.Contains(text, want) {
						t.Errorf("legacy receipt lost %q", want)
					}
				}
				for _, absent := range row.WantAbsent {
					if strings.Contains(text, absent) {
						t.Errorf("legacy receipt contains %q", absent)
					}
				}
				if len(row.Diagnostics) > 0 && !strings.Contains(text, "earlier warnings may now be resolved") {
					t.Fatal("receipt represented historical warnings as current failures")
				}
				_, command := wizard.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if command == nil {
					t.Fatal("receipt warnings prevented finish")
				}
			})
		}
	}
}
