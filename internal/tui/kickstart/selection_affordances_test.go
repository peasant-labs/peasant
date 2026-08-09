package kickstart_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/selection_affordances.yaml
var selectionAffordanceData []byte

type selectionAffordanceRow struct {
	Label        string   `yaml:"label"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

type selectionAffordanceCase struct {
	Name          string                   `yaml:"name"`
	Keys          []string                 `yaml:"keys"`
	WantContains  []string                 `yaml:"wantContains"`
	WantMissing   []string                 `yaml:"wantMissing"`
	RowAssertions []selectionAffordanceRow `yaml:"rowAssertions"`
}

type selectionAffordanceDocument struct {
	ExpectedCaseCount int                       `yaml:"expectedCaseCount"`
	SavedSelection    config.SelectionConfig    `yaml:"savedSelection"`
	Cases             []selectionAffordanceCase `yaml:"cases"`
}

func decodeSelectionAffordances(data []byte) (selectionAffordanceDocument, error) {
	var doc selectionAffordanceDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/selection_affordances.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("selection_affordances.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf("selection_affordances.yaml expectedCaseCount=%d but has %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if !doc.SavedSelection.Mode.IsValid() {
		return doc, fmt.Errorf("selection_affordances.yaml saved selection mode %q is invalid", doc.SavedSelection.Mode)
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] || len(c.WantContains)+len(c.WantMissing)+len(c.RowAssertions) == 0 {
			return doc, fmt.Errorf("selection_affordances.yaml contains an invalid, duplicate, or assertion-free case: %#v", c)
		}
		seen[c.Name] = true
		for _, row := range c.RowAssertions {
			if row.Label == "" || len(row.WantContains)+len(row.WantMissing) == 0 {
				return doc, fmt.Errorf("selection_affordances.yaml case %q has an empty row assertion", c.Name)
			}
		}
	}
	return doc, nil
}

func loadSelectionAffordances(t *testing.T) selectionAffordanceDocument {
	t.Helper()
	doc, err := decodeSelectionAffordances(selectionAffordanceData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func selectionAffordanceProgram(t *testing.T, saved config.SelectionConfig) kickstart.Program {
	t.Helper()
	renderDoc := loadSelectionRenderDoc(t)
	th := theme.New(theme.ModeDark)
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Selection = saved
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed selection affordance config: %v", err)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse selection affordance config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open selection affordance draft: %v", err)
	}
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:   th,
		Draft:   draft,
		Source:  kickstart.NewScannerTreeSource(renderDoc.Listings, kickstart.WithIngestedSessionIDs(renderDoc.Ingested)),
		Preview: kickstart.NewListingPreview(th, renderDoc.Listings, turnsFromPrompts(renderDoc.Stored)),
	})
	program.SetSize(132, 30)
	program = declineOAuth(t, program)
	return drainProgram(program, program.Init())
}

func selectionAffordanceKey(t *testing.T, value string) tea.KeyPressMsg {
	t.Helper()
	switch value {
	case "ctrl+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}
	default:
		runes := []rune(value)
		if len(runes) != 1 {
			t.Fatalf("selection affordance fixture names unsupported key %q", value)
		}
		return tea.KeyPressMsg{Code: runes[0], Text: value}
	}
}

func driveSelectionAffordanceProgram(t *testing.T, program kickstart.Program, keys []string) kickstart.Program {
	t.Helper()
	for _, value := range keys {
		next, cmd := program.Update(selectionAffordanceKey(t, value))
		program = drainProgram(next, cmd)
	}
	return program
}

func selectionAffordanceLine(view, label string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	return ""
}

func TestSelectionStep_AffordancesUseMountedProductionPath(t *testing.T) {
	doc := loadSelectionAffordances(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			program := driveSelectionAffordanceProgram(t, selectionAffordanceProgram(t, doc.SavedSelection), c.Keys)
			view := stripRender(program.View())
			for _, want := range c.WantContains {
				if !strings.Contains(view, want) {
					t.Errorf("mounted selection step must contain %q:\n%s", want, view)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(view, missing) {
					t.Errorf("mounted selection step must not contain %q:\n%s", missing, view)
				}
			}
			for _, row := range c.RowAssertions {
				line := selectionAffordanceLine(view, row.Label)
				if line == "" {
					t.Errorf("mounted selection row %q is absent:\n%s", row.Label, view)
					continue
				}
				for _, want := range row.WantContains {
					if !strings.Contains(line, want) {
						t.Errorf("row %q must contain %q: %s", row.Label, want, line)
					}
				}
				for _, missing := range row.WantMissing {
					if strings.Contains(line, missing) {
						t.Errorf("row %q must not contain %q: %s", row.Label, missing, line)
					}
				}
			}
		})
	}
}

func TestSelectionAffordanceFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), selectionAffordanceData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeSelectionAffordances(mutated); err == nil {
		t.Fatal("selection affordance fixture accepted an unknown field")
	}
}

func TestSelectionAffordanceFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), selectionAffordanceData...), []byte("\n---\n{}\n")...)
	if _, err := decodeSelectionAffordances(mutated); err == nil {
		t.Fatal("selection affordance fixture accepted a trailing document")
	}
}

func TestSelectionAffordanceFixtureEnforcesRowCount(t *testing.T) {
	mutated := bytes.Replace(selectionAffordanceData, []byte("expectedCaseCount: 5"), []byte("expectedCaseCount: 6"), 1)
	if _, err := decodeSelectionAffordances(mutated); err == nil {
		t.Fatal("selection affordance fixture accepted a mismatched row-count guard")
	}
}
