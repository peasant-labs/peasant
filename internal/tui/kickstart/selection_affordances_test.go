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

const expectedSelectionAffordanceCaseCount = 7

type selectionAffordanceSurface string

const (
	selectionAffordanceSurfaceField selectionAffordanceSurface = "selection"
	selectionAffordanceSurfaceHelp  selectionAffordanceSurface = "help"
)

func (s selectionAffordanceSurface) valid() bool {
	return s == selectionAffordanceSurfaceField || s == selectionAffordanceSurfaceHelp
}

type selectionAffordanceRow struct {
	Label        string   `yaml:"label"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

type selectionAffordanceCase struct {
	Name                    string                     `yaml:"name"`
	Surface                 selectionAffordanceSurface `yaml:"surface"`
	ExpectedSearchCursors   *int                       `yaml:"expectedSearchCursors"`
	Keys                    []string                   `yaml:"keys"`
	BeforeLoad              bool                       `yaml:"beforeLoad"`
	DrainAfterKeys          bool                       `yaml:"drainAfterKeys"`
	WantBeforeDrainContains []string                   `yaml:"wantBeforeDrainContains"`
	WantBeforeDrainMissing  []string                   `yaml:"wantBeforeDrainMissing"`
	WantContains            []string                   `yaml:"wantContains"`
	WantMissing             []string                   `yaml:"wantMissing"`
	RowAssertions           []selectionAffordanceRow   `yaml:"rowAssertions"`
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
	if doc.ExpectedCaseCount != expectedSelectionAffordanceCaseCount || len(doc.Cases) != expectedSelectionAffordanceCaseCount {
		return doc, fmt.Errorf("selection_affordances.yaml cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedSelectionAffordanceCaseCount)
	}
	if !doc.SavedSelection.Mode.IsValid() {
		return doc, fmt.Errorf("selection_affordances.yaml saved selection mode %q is invalid", doc.SavedSelection.Mode)
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] || !c.Surface.valid() || c.ExpectedSearchCursors == nil ||
			*c.ExpectedSearchCursors < 0 || *c.ExpectedSearchCursors > 1 ||
			len(c.WantBeforeDrainContains)+len(c.WantBeforeDrainMissing)+len(c.WantContains)+len(c.WantMissing)+len(c.RowAssertions) == 0 ||
			!selectionRenderValuesPresent(c.WantBeforeDrainContains, c.WantBeforeDrainMissing, c.WantContains, c.WantMissing) {
			return doc, fmt.Errorf("selection_affordances.yaml contains an invalid, duplicate, or assertion-free case: %#v", c)
		}
		seen[c.Name] = true
		if c.DrainAfterKeys && !c.BeforeLoad {
			return doc, fmt.Errorf("selection_affordances.yaml case %q drains after keys without starting before load", c.Name)
		}
		for _, row := range c.RowAssertions {
			if strings.TrimSpace(row.Label) == "" || len(row.WantContains)+len(row.WantMissing) == 0 ||
				!selectionRenderValuesPresent(row.WantContains, row.WantMissing) {
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

func selectionAffordanceProgram(t *testing.T, saved config.SelectionConfig, drainInitial bool) kickstart.Program {
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
	if drainInitial {
		program = declineOAuth(t, program)
	} else {
		// Keep the transition's initialization command pending so fixtures can
		// assert which controls are available before the first scan completes.
		program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if program.Phase() != kickstart.PhaseFlow {
			t.Fatalf("after declining OAuth, phase = %s, want flow", program.Phase())
		}
	}
	return program
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

func assertSingleSelectionSearchBar(t *testing.T, view string) {
	t.Helper()
	if got := strings.Count(view, "search:"); got != 1 {
		t.Errorf("mounted selection step renders %d search bars, want exactly one:\n%s", got, view)
	}
}

func TestSelectionStep_AffordancesUseMountedProductionPath(t *testing.T) {
	doc := loadSelectionAffordances(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			program := driveSelectionAffordanceProgram(t, selectionAffordanceProgram(t, doc.SavedSelection, !c.BeforeLoad), c.Keys)
			if c.BeforeLoad {
				view := stripRender(program.View())
				assertSingleSelectionSearchBar(t, view)
				for _, want := range c.WantBeforeDrainContains {
					if !strings.Contains(view, want) {
						t.Errorf("pre-load selection step must contain %q:\n%s", want, view)
					}
				}
				for _, missing := range c.WantBeforeDrainMissing {
					if strings.Contains(view, missing) {
						t.Errorf("pre-load selection step must not contain %q:\n%s", missing, view)
					}
				}
			}
			if c.DrainAfterKeys {
				program = drainProgram(program, program.Init())
			}
			view := stripRender(program.View())
			if c.Surface == selectionAffordanceSurfaceField {
				assertSingleSelectionSearchBar(t, view)
			} else if strings.Contains(view, "search:") {
				t.Errorf("help modal leaked the underlying selection search bar:\n%s", view)
			}
			if got := strings.Count(view, "▏"); got != *c.ExpectedSearchCursors {
				t.Errorf("mounted %s surface renders %d search cursors, want %d:\n%s",
					c.Surface, got, *c.ExpectedSearchCursors, view)
			}
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
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedSelectionAffordanceCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedSelectionAffordanceCaseCount+1))
	mutated := bytes.Replace(selectionAffordanceData, declared, changed, 1)
	if bytes.Equal(mutated, selectionAffordanceData) {
		t.Fatal("selection affordance count mutation did not alter the fixture")
	}
	if _, err := decodeSelectionAffordances(mutated); err == nil {
		t.Fatal("selection affordance fixture accepted a mismatched row-count guard")
	}
}
