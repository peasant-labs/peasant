package kickstart_test

import (
	"bytes"
	"context"
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
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedVisibilityOverlayCases     = 2
	expectedVisibilityConfirmCases     = 1
	expectedVisibilityHelpOverlayCases = 1
)

type visibilityOverlayKind string

const (
	visibilityOverlayConfirm visibilityOverlayKind = "confirm"
	visibilityOverlayHelp    visibilityOverlayKind = "help"
)

func (k visibilityOverlayKind) valid() bool {
	return k == visibilityOverlayConfirm || k == visibilityOverlayHelp
}

type visibilityOverlayCase struct {
	Name        string                `yaml:"name"`
	Overlay     visibilityOverlayKind `yaml:"overlay"`
	Width       int                   `yaml:"width"`
	Height      int                   `yaml:"height"`
	WantSection string                `yaml:"wantSection"`
	WantOverlay string                `yaml:"wantOverlay"`
}

type visibilityOverlayDocument struct {
	ExpectedCaseCount    int                     `yaml:"expectedCaseCount"`
	ExpectedConfirmCount int                     `yaml:"expectedConfirmCount"`
	ExpectedHelpCount    int                     `yaml:"expectedHelpCount"`
	Cases                []visibilityOverlayCase `yaml:"cases"`
}

//go:embed testdata/guided/visibility_overlay.yaml
var visibilityOverlayData []byte

func decodeVisibilityOverlayDocument(data []byte) (visibilityOverlayDocument, error) {
	var document visibilityOverlayDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/guided/visibility_overlay.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("visibility_overlay.yaml must hold exactly one document: %w", err)
	}
	if document.ExpectedCaseCount != expectedVisibilityOverlayCases || len(document.Cases) != expectedVisibilityOverlayCases {
		return document, fmt.Errorf("visibility overlay rows: declared=%d actual=%d required=%d",
			document.ExpectedCaseCount, len(document.Cases), expectedVisibilityOverlayCases)
	}
	return document, nil
}

func loadVisibilityOverlayDocument(t *testing.T) visibilityOverlayDocument {
	t.Helper()
	document, err := decodeVisibilityOverlayDocument(visibilityOverlayData)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	counts := map[visibilityOverlayKind]int{}
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] || !row.Overlay.valid() ||
			row.Width <= 0 || row.Height <= 0 || strings.TrimSpace(row.WantSection) == "" ||
			strings.TrimSpace(row.WantOverlay) == "" {
			t.Fatalf("visibility overlay row is incomplete or duplicated: %#v", row)
		}
		names[row.Name] = true
		counts[row.Overlay]++
	}
	if document.ExpectedConfirmCount != expectedVisibilityConfirmCases ||
		counts[visibilityOverlayConfirm] != expectedVisibilityConfirmCases ||
		document.ExpectedHelpCount != expectedVisibilityHelpOverlayCases ||
		counts[visibilityOverlayHelp] != expectedVisibilityHelpOverlayCases {
		t.Fatalf("visibility overlay kinds are not pinned: confirm=%d help=%d",
			counts[visibilityOverlayConfirm], counts[visibilityOverlayHelp])
	}
	return document
}

func TestVisibilityOverlayFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), visibilityOverlayData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeVisibilityOverlayDocument(mutated); err == nil {
		t.Fatal("visibility overlay fixture accepted an unknown field")
	}
}

func TestVisibilityOverlayFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), visibilityOverlayData...), []byte("\n---\n{}\n")...)
	if _, err := decodeVisibilityOverlayDocument(mutated); err == nil {
		t.Fatal("visibility overlay fixture accepted a trailing document")
	}
}

func TestVisibilityOverlayFixturePinsCounts(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedVisibilityOverlayCases))
	mutated := bytes.Replace(visibilityOverlayData, declared,
		[]byte(fmt.Sprintf("expectedCaseCount: %d", expectedVisibilityOverlayCases-1)), 1)
	if bytes.Equal(mutated, visibilityOverlayData) {
		t.Fatal("visibility overlay exact-count mutation did not alter the fixture")
	}
	if _, err := decodeVisibilityOverlayDocument(mutated); err == nil {
		t.Fatal("visibility overlay fixture accepted a coordinated count mutation")
	}
}

func visibilityOverlayOpenKey(kind visibilityOverlayKind) tea.KeyPressMsg {
	if kind == visibilityOverlayConfirm {
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{Code: '?', Text: "?"}
}

func visibilityOverlayCloseKey(kind visibilityOverlayKind) tea.KeyPressMsg {
	if kind == visibilityOverlayConfirm {
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	return tea.KeyPressMsg{Code: tea.KeyEscape}
}

func advanceRetainedProgramToLicense(t *testing.T, program kickstart.Program, wantSection string) kickstart.Program {
	t.Helper()
	for step := 0; step < 16; step++ {
		if program.Phase() != kickstart.PhaseFlow {
			t.Fatalf("guided Program left Flow before license; phase=%s view:\n%s", program.Phase(), stripRender(program.View()))
		}
		if strings.Contains(stripRender(program.View()), wantSection) {
			return program
		}
		var command tea.Cmd
		program, command = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
		program = drainProgram(program, command)
	}
	t.Fatalf("guided Program never reached %q; view:\n%s", wantSection, stripRender(program.View()))
	return program
}

func TestVisibilityDetourYieldsToFlowOwnedOverlays(t *testing.T) {
	for _, row := range loadVisibilityOverlayDocument(t).Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			loaded := config.BaseConfig()
			if err := config.SaveAtomic(path, loaded); err != nil {
				t.Fatalf("seed visibility overlay config: %v", err)
			}
			draft, err := settings.NewDraft(path, loaded)
			if err != nil {
				t.Fatalf("open visibility overlay draft: %v", err)
			}
			loginCalls := 0
			program := kickstart.NewProgram(kickstart.ProgramDeps{
				Theme:  theme.New(theme.ModeDark),
				Draft:  draft,
				Source: scannerfix.NewFixtureTreeSource("standard"),
				Login: func(context.Context, func(string)) (string, error) {
					loginCalls++
					return "fixture-user", nil
				},
			})
			program.SetSize(row.Width, row.Height)
			program, startup := program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			program = runFlowInitOnce(t, program, startup)
			program = advanceRetainedProgramToLicense(t, program, row.WantSection)

			sectionView := stripRender(program.View())
			beforeWorking := retainedWorkingBytes(t, draft)
			beforeConfig := mustReadFile(t, path)

			program, _ = program.Update(visibilityOverlayOpenKey(row.Overlay))
			if program.Phase() != kickstart.PhaseFlow || !strings.Contains(stripRender(program.View()), row.WantOverlay) {
				t.Fatalf("Flow-owned overlay did not open; phase=%s view:\n%s", program.Phase(), stripRender(program.View()))
			}
			program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			if program.Phase() != kickstart.PhaseFlow || !strings.Contains(stripRender(program.View()), row.WantOverlay) ||
				strings.Contains(stripRender(program.View()), "log in now to choose a default sharing visibility?") {
				t.Fatalf("Tab escaped the active %s overlay; phase=%s view:\n%s", row.Overlay, program.Phase(), stripRender(program.View()))
			}
			if loginCalls != 0 {
				t.Fatalf("Tab through the active %s overlay invoked login %d times", row.Overlay, loginCalls)
			}
			if afterWorking := retainedWorkingBytes(t, draft); !bytes.Equal(beforeWorking, afterWorking) {
				t.Fatalf("active %s overlay changed Draft bytes\nbefore=%s\nafter=%s", row.Overlay, beforeWorking, afterWorking)
			}
			if afterConfig := mustReadFile(t, path); !bytes.Equal(beforeConfig, afterConfig) {
				t.Fatalf("active %s overlay changed config bytes", row.Overlay)
			}

			program, _ = program.Update(visibilityOverlayCloseKey(row.Overlay))
			if program.Phase() != kickstart.PhaseFlow || stripRender(program.View()) != sectionView {
				t.Fatalf("closing %s did not restore the exact license section; phase=%s\nbefore:\n%s\nafter:\n%s",
					row.Overlay, program.Phase(), sectionView, stripRender(program.View()))
			}
			if afterWorking := retainedWorkingBytes(t, draft); !bytes.Equal(beforeWorking, afterWorking) {
				t.Fatalf("closing %s changed Draft bytes", row.Overlay)
			}

			program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			visibilityView := stripRender(program.View())
			if program.Phase() != kickstart.PhaseVisibility || !strings.Contains(visibilityView, "log in now to choose a default sharing visibility?") {
				t.Fatalf("one Tab after closing %s did not enter visibility; phase=%s view:\n%s",
					row.Overlay, program.Phase(), visibilityView)
			}
			program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			if program.Phase() != kickstart.PhaseVisibility || loginCalls != 0 {
				t.Fatalf("visibility prompt was replayed or invoked login after %s; phase=%s loginCalls=%d",
					row.Overlay, program.Phase(), loginCalls)
			}
		})
	}
}
