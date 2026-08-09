package settings

import (
	"bytes"
	_ "embed"
	"io"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedPrintableAvailabilityRows = 4
	expectedPrintableRequiredHints    = 4
)

type printableShadowedAction string

const (
	printableShadowQuit   printableShadowedAction = "quit"
	printableShadowBack   printableShadowedAction = "back"
	printableShadowHelp   printableShadowedAction = "help"
	printableShadowFilter printableShadowedAction = "filter"
)

func (a printableShadowedAction) actionID() (keymap.ActionID, bool) {
	switch a {
	case printableShadowQuit:
		return keymap.ActionQuit, true
	case printableShadowBack:
		return keymap.ActionBack, true
	case printableShadowHelp:
		return keymap.ActionHelp, true
	case printableShadowFilter:
		return keymap.ActionFilter, true
	default:
		return keymap.ActionUnknown, false
	}
}

type printableAvailabilityFixture struct {
	Name           string                  `yaml:"name"`
	Input          string                  `yaml:"input"`
	ShadowedAction printableShadowedAction `yaml:"shadowedAction"`
	WantQuery      string                  `yaml:"wantQuery"`
	ForbiddenHint  string                  `yaml:"forbiddenHint"`
}

type printableAvailabilityDocument struct {
	ExpectedRowCount          int                            `yaml:"expectedRowCount"`
	ExpectedRequiredHintCount int                            `yaml:"expectedRequiredHintCount"`
	RequiredHints             []string                       `yaml:"requiredHints"`
	Rows                      []printableAvailabilityFixture `yaml:"rows"`
}

//go:embed testdata/flow_printable_availability.yaml
var printableAvailabilityData []byte

func loadPrintableAvailabilityDocument(t *testing.T) printableAvailabilityDocument {
	t.Helper()
	var document printableAvailabilityDocument
	decoder := yaml.NewDecoder(bytes.NewReader(printableAvailabilityData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode printable availability fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("printable availability fixture must contain exactly one YAML document")
	}
	if document.ExpectedRowCount != expectedPrintableAvailabilityRows || len(document.Rows) != expectedPrintableAvailabilityRows {
		t.Fatalf("printable availability rows: declared=%d actual=%d required=%d",
			document.ExpectedRowCount, len(document.Rows), expectedPrintableAvailabilityRows)
	}
	if document.ExpectedRequiredHintCount != expectedPrintableRequiredHints || len(document.RequiredHints) != expectedPrintableRequiredHints {
		t.Fatalf("printable availability required hints: declared=%d actual=%d required=%d",
			document.ExpectedRequiredHintCount, len(document.RequiredHints), expectedPrintableRequiredHints)
	}
	seen := map[string]bool{}
	for _, row := range document.Rows {
		_, validAction := row.ShadowedAction.actionID()
		if strings.TrimSpace(row.Name) == "" || seen[row.Name] || len([]rune(row.Input)) != 1 ||
			!validAction || row.WantQuery == "" || row.ForbiddenHint == "" {
			t.Fatalf("printable availability row is incomplete or duplicated: %#v", row)
		}
		seen[row.Name] = true
	}
	for _, hint := range document.RequiredHints {
		if strings.TrimSpace(hint) == "" {
			t.Fatal("printable availability fixture contains an empty required hint")
		}
	}
	return document
}

func newPrintableAvailabilityFlow(t *testing.T) Flow {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	loaded := config.BaseConfig()
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("seed printable availability config: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open printable availability draft: %v", err)
	}
	field := Tree("selection", "transcripts", selectionAccessor(), scannerfix.NewFixtureTreeSource("standard"),
		WithFacet(MetaHarness, "harness"))
	flow := NewFlow(theme.New(theme.ModeDark), Registry{Sections: []Section{{
		Key: "transcripts", Title: "select transcripts", Fields: []Field{field},
	}}}, draft)
	flow.SetSize(120, 24)
	return drainInit(flow)
}

func TestFlowPrintableInputUsesOneEffectiveAvailability(t *testing.T) {
	document := loadPrintableAvailabilityDocument(t)
	for _, row := range document.Rows {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			flow := newPrintableAvailabilityFlow(t)
			flow, _ = flow.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
			r := []rune(row.Input)[0]
			flow, _ = flow.Update(tea.KeyPressMsg{Code: r, Text: row.Input})

			plain := stripANSIForSettings(flow.View())
			if !strings.Contains(plain, row.WantQuery) {
				t.Errorf("mounted search status does not contain %q:\n%s", row.WantQuery, plain)
			}
			if strings.Contains(plain, row.ForbiddenHint) {
				t.Errorf("mounted footer advertises shadowed hint %q:\n%s", row.ForbiddenHint, plain)
			}
			for _, hint := range document.RequiredHints {
				if !strings.Contains(plain, hint) {
					t.Errorf("mounted footer omits live search hint %q:\n%s", hint, plain)
				}
			}
			if flow.Confirming() || flow.Helping() {
				t.Fatal("printable query input leaked into a Flow lifecycle action")
			}

			shadowed, _ := row.ShadowedAction.actionID()
			entries := keymap.HelpEntries(keymap.Default(), flow.availability())
			available := map[keymap.ActionID]bool{}
			for _, entry := range entries {
				available[entry.Action] = true
			}
			if available[shadowed] {
				t.Errorf("help entries retain shadowed action %s: %#v", shadowed, entries)
			}
			for _, required := range []keymap.ActionID{
				keymap.ActionDeleteFilter,
				keymap.ActionKeepFilter,
				keymap.ActionClearFilter,
				keymap.ActionNextField,
			} {
				if !available[required] {
					t.Errorf("effective help omits live search action %s: %#v", required, entries)
				}
			}
		})
	}
}
