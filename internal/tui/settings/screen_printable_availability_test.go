package settings

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

func decodeScreenPrintableAvailability(data []byte) (printableAvailabilityDocument, error) {
	var document printableAvailabilityDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/flow_printable_availability.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("printable availability fixture must hold exactly one document: %w", err)
	}
	if document.ExpectedRowCount != expectedPrintableAvailabilityRows || len(document.Rows) != expectedPrintableAvailabilityRows ||
		document.ExpectedRequiredHintCount != expectedPrintableRequiredHints || len(document.RequiredHints) != expectedPrintableRequiredHints {
		return document, fmt.Errorf("printable availability counts are not pinned: rows=%d/%d hints=%d/%d",
			document.ExpectedRowCount, len(document.Rows), document.ExpectedRequiredHintCount, len(document.RequiredHints))
	}
	seenNames := map[string]bool{}
	seenInputs := map[string]bool{}
	seenActions := map[printableShadowedAction]bool{}
	for _, row := range document.Rows {
		_, validAction := row.ShadowedAction.actionID()
		if strings.TrimSpace(row.Name) == "" || seenNames[row.Name] || len([]rune(row.Input)) != 1 || seenInputs[row.Input] ||
			!validAction || seenActions[row.ShadowedAction] || row.WantQuery == "" || row.ForbiddenHint == "" {
			return document, fmt.Errorf("printable availability fixture contains an invalid or duplicate row: %#v", row)
		}
		seenNames[row.Name] = true
		seenInputs[row.Input] = true
		seenActions[row.ShadowedAction] = true
	}
	for _, hint := range document.RequiredHints {
		if strings.TrimSpace(hint) == "" {
			return document, fmt.Errorf("printable availability fixture contains an empty required hint")
		}
	}
	return document, nil
}

func newPrintableAvailabilityScreen(t *testing.T) (Screen, *treeField) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	loaded := config.BaseConfig()
	if err := config.SaveAtomic(path, loaded); err != nil {
		t.Fatalf("seed printable Screen config: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open printable Screen draft: %v", err)
	}
	field := Tree("selection", "transcripts", selectionAccessor(), scannerfix.NewFixtureTreeSource("standard"),
		WithFacet(MetaHarness, "harness")).(*treeField)
	screen := NewScreen(theme.New(theme.ModeDark), Registry{Sections: []Section{{
		Key: "transcripts", Title: "select transcripts", Fields: []Field{field},
	}}}, draft)
	screen.SetSize(120, 24)
	result := ownedTreeResult(t, screen.Init(), field)
	screen, _ = screen.Update(result)
	if !field.forestReady {
		t.Fatal("mounted printable Screen tree did not become ready")
	}
	screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if screen.navFocused || screen.focusField < 0 {
		t.Fatal("mounted printable Screen did not focus its selection field")
	}
	return screen, field
}

func TestScreenPrintableInputUsesOneEffectiveAvailability(t *testing.T) {
	document, err := decodeScreenPrintableAvailability(printableAvailabilityData)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Rows {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			screen, field := newPrintableAvailabilityScreen(t)
			screen, _ = screen.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
			r := []rune(row.Input)[0]
			screen, _ = screen.Update(tea.KeyPressMsg{Code: r, Text: row.Input})

			plain := stripANSIForSettings(screen.View())
			if !strings.Contains(plain, row.WantQuery) {
				t.Errorf("mounted Screen search status does not contain %q:\n%s", row.WantQuery, plain)
			}
			if strings.Contains(plain, row.ForbiddenHint) {
				t.Errorf("mounted Screen footer advertises shadowed hint %q:\n%s", row.ForbiddenHint, plain)
			}
			for _, hint := range document.RequiredHints {
				if !strings.Contains(plain, hint) {
					t.Errorf("mounted Screen footer omits live search hint %q:\n%s", hint, plain)
				}
			}
			if screen.confirming || screen.helping {
				t.Fatal("printable query input leaked into a Screen lifecycle action")
			}

			shadowed, _ := row.ShadowedAction.actionID()
			entries := keymap.HelpEntries(keymap.Default(), screen.availability())
			available := map[keymap.ActionID]bool{}
			for _, entry := range entries {
				available[entry.Action] = true
			}
			if available[shadowed] {
				t.Errorf("Screen help entries retain shadowed action %s: %#v", shadowed, entries)
			}
			for _, required := range []keymap.ActionID{
				keymap.ActionDeleteFilter,
				keymap.ActionKeepFilter,
				keymap.ActionClearFilter,
				keymap.ActionNextField,
			} {
				if !available[required] {
					t.Errorf("Screen effective help omits live search action %s: %#v", required, entries)
				}
			}

			screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
			if got := field.tree.FilterState(); got.Query != "" || got.Mode != kit.TreeFilterEditing {
				t.Fatalf("mounted delete filter result=%+v want empty editing query", got)
			}
			screen, _ = screen.Update(tea.KeyPressMsg{Code: r, Text: row.Input})
			screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if got := field.tree.FilterState(); got.Query != row.Input || got.Mode != kit.TreeFilterKept {
				t.Fatalf("mounted keep filter result=%+v want kept query %q", got, row.Input)
			}
			screen, _ = screen.Update(tea.KeyPressMsg{Code: '/', Text: "/"})
			screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
			if got := field.tree.FilterState(); got.Query != "" || got.Mode != kit.TreeFilterInactive {
				t.Fatalf("mounted clear filter result=%+v want inactive empty query", got)
			}
		})
	}
}

func TestScreenPrintableAvailabilityFixtureGuards(t *testing.T) {
	if _, err := decodeScreenPrintableAvailability(append(append([]byte(nil), printableAvailabilityData...), []byte("\nunknownField: true\n")...)); err == nil {
		t.Fatal("printable availability fixture accepted an unknown field")
	}
	if _, err := decodeScreenPrintableAvailability(append(append([]byte(nil), printableAvailabilityData...), []byte("\n---\n{}\n")...)); err == nil {
		t.Fatal("printable availability fixture accepted a trailing document")
	}
	declared := []byte(fmt.Sprintf("expectedRowCount: %d", expectedPrintableAvailabilityRows))
	changed := []byte(fmt.Sprintf("expectedRowCount: %d", expectedPrintableAvailabilityRows+1))
	mutated := bytes.Replace(printableAvailabilityData, declared, changed, 1)
	if bytes.Equal(mutated, printableAvailabilityData) {
		t.Fatal("printable availability exact-count mutation did not alter the fixture")
	}
	if _, err := decodeScreenPrintableAvailability(mutated); err == nil {
		t.Fatal("printable availability fixture accepted a changed exact count")
	}
}
