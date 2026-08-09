package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const expectedRetainedLoginCases = 2

type retainedLoginScenario string

const (
	retainedLoginPreservePresentation retainedLoginScenario = "preserve-presentation"
	retainedLoginPendingTreeResult    retainedLoginScenario = "pending-tree-result"
)

func (s retainedLoginScenario) valid() bool {
	return s == retainedLoginPreservePresentation || s == retainedLoginPendingTreeResult
}

type retainedLoginCase struct {
	Name                string                 `yaml:"name"`
	Scenario            retainedLoginScenario  `yaml:"scenario"`
	Fixture             string                 `yaml:"fixture"`
	Width               int                    `yaml:"width"`
	Height              int                    `yaml:"height"`
	SavedSelection      config.SelectionConfig `yaml:"savedSelection"`
	BeforeLoginKeys     []string               `yaml:"beforeLoginKeys"`
	ExpectedSelection   config.SelectionConfig `yaml:"expectedSelection"`
	ExpectedSourceLoads int                    `yaml:"expectedSourceLoads"`
	ExpectedPreviewID   string                 `yaml:"expectedPreviewID"`
	CommitAfterDelivery bool                   `yaml:"commitAfterDelivery"`
	StateLineLabels     []string               `yaml:"stateLineLabels"`
	WantViewContains    []string               `yaml:"wantViewContains"`
	WantViewMissing     []string               `yaml:"wantViewMissing"`
}

type retainedLoginDocument struct {
	ExpectedCaseCount int                 `yaml:"expectedCaseCount"`
	Cases             []retainedLoginCase `yaml:"cases"`
}

//go:embed testdata/guided/retained_login.yaml
var retainedLoginData []byte

func decodeRetainedLoginDocument(data []byte) (retainedLoginDocument, error) {
	var document retainedLoginDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode testdata/guided/retained_login.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("retained_login.yaml must hold exactly one document: %w", err)
	}
	if document.ExpectedCaseCount != expectedRetainedLoginCases || len(document.Cases) != expectedRetainedLoginCases {
		return document, fmt.Errorf("retained login rows: declared=%d actual=%d required=%d",
			document.ExpectedCaseCount, len(document.Cases), expectedRetainedLoginCases)
	}
	return document, nil
}

func loadRetainedLoginDocument(t *testing.T) retainedLoginDocument {
	t.Helper()
	document, err := decodeRetainedLoginDocument(retainedLoginData)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, row := range document.Cases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] || !row.Scenario.valid() ||
			strings.TrimSpace(row.Fixture) == "" || row.Width <= 0 || row.Height <= 0 ||
			!row.SavedSelection.Mode.IsValid() || !row.ExpectedSelection.Mode.IsValid() ||
			row.ExpectedSourceLoads != 1 || len(row.WantViewContains) == 0 {
			t.Fatalf("retained login row is incomplete or duplicated: %#v", row)
		}
		names[row.Name] = true
		switch row.Scenario {
		case retainedLoginPreservePresentation:
			if len(row.BeforeLoginKeys) == 0 || row.ExpectedPreviewID == "" || len(row.StateLineLabels) == 0 || row.CommitAfterDelivery {
				t.Fatalf("retained presentation row %q does not pin interactive state", row.Name)
			}
		case retainedLoginPendingTreeResult:
			if len(row.BeforeLoginKeys) != 0 || row.ExpectedPreviewID != "" || len(row.StateLineLabels) != 0 || !row.CommitAfterDelivery {
				t.Fatalf("pending tree row %q mixes incompatible presentation expectations", row.Name)
			}
		}
	}
	return document
}

func TestRetainedLoginFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), retainedLoginData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeRetainedLoginDocument(mutated); err == nil {
		t.Fatal("retained login fixture accepted an unknown field")
	}
}

func TestRetainedLoginFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), retainedLoginData...), []byte("\n---\n{}\n")...)
	if _, err := decodeRetainedLoginDocument(mutated); err == nil {
		t.Fatal("retained login fixture accepted a trailing document")
	}
}

func TestRetainedLoginFixtureEnforcesExactRowCount(t *testing.T) {
	mutated := bytes.Replace(retainedLoginData, []byte("expectedCaseCount: 2"), []byte("expectedCaseCount: 3"), 1)
	if _, err := decodeRetainedLoginDocument(mutated); err == nil {
		t.Fatal("retained login fixture accepted a mismatched row-count guard")
	}
}

type retainedPreviewSource struct {
	mu    sync.Mutex
	calls []string
}

func (s *retainedPreviewSource) Body(id string) (kit.PreviewBody, error) {
	s.mu.Lock()
	s.calls = append(s.calls, id)
	s.mu.Unlock()
	lines := make([]string, 8)
	for index := range lines {
		lines[index] = fmt.Sprintf("retained preview %s line %d", id, index)
	}
	return retainedPreviewBody(strings.Join(lines, "\n")), nil
}

func (s *retainedPreviewSource) lastID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.calls) == 0 {
		return ""
	}
	return s.calls[len(s.calls)-1]
}

type retainedPreviewBody string

func (b retainedPreviewBody) Render(int) string { return string(b) }

var _ kit.BodySource = (*retainedPreviewSource)(nil)

func retainedLoginKey(t *testing.T, value string) tea.KeyPressMsg {
	t.Helper()
	switch value {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "ctrl+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}
	default:
		runes := []rune(value)
		if len(runes) != 1 {
			t.Fatalf("retained login fixture names unsupported key %q", value)
		}
		return tea.KeyPressMsg{Code: runes[0], Text: value}
	}
}

func driveRetainedLoginKeys(t *testing.T, program kickstart.Program, keys []string) kickstart.Program {
	t.Helper()
	for _, value := range keys {
		next, command := program.Update(retainedLoginKey(t, value))
		program = drainProgram(next, command)
	}
	return program
}

func enterRetainedVisibility(t *testing.T, program kickstart.Program) kickstart.Program {
	t.Helper()
	for step := 0; step < 16 && program.Phase() != kickstart.PhaseVisibility; step++ {
		program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	if program.Phase() != kickstart.PhaseVisibility {
		t.Fatalf("guided flow never offered visibility login; phase=%s view:\n%s", program.Phase(), stripRender(program.View()))
	}
	return program
}

func completeRetainedVisibilityLogin(t *testing.T, program kickstart.Program) kickstart.Program {
	t.Helper()
	program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	program, command := program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if command == nil {
		t.Fatal("accepting retained visibility login produced no login command")
	}
	transitioned := false
	for _, message := range collectMsgs(command) {
		before := program.Phase()
		var next tea.Cmd
		program, next = program.Update(message)
		if before == kickstart.PhaseVisibility && program.Phase() == kickstart.PhaseFlow {
			transitioned = true
			if next != nil {
				t.Fatal("successful visibility login restarted the mounted settings flow")
			}
		}
	}
	if !transitioned || !program.Connected() {
		t.Fatalf("visibility login did not enter the connected flow; connected=%t phase=%s", program.Connected(), program.Phase())
	}
	return program
}

func returnToRetainedSelection(t *testing.T, program kickstart.Program) kickstart.Program {
	t.Helper()
	for step := 0; step < 16; step++ {
		view := stripRender(program.View())
		if strings.Contains(view, "choose which recorded sessions become part of your local peasant history") {
			return program
		}
		program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	}
	t.Fatalf("connected flow did not return to its retained selection field; view:\n%s", stripRender(program.View()))
	return program
}

func retainedStateLines(t *testing.T, view string, labels []string) []string {
	t.Helper()
	lines := strings.Split(stripRender(view), "\n")
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		found := ""
		for _, line := range lines {
			if strings.Contains(line, label) {
				found = line
				break
			}
		}
		if found == "" {
			t.Fatalf("selection presentation has no state line containing %q:\n%s", label, stripRender(view))
		}
		out = append(out, found)
	}
	return out
}

func assertRetainedView(t *testing.T, row retainedLoginCase, view string) {
	t.Helper()
	plain := stripRender(view)
	for _, want := range row.WantViewContains {
		if !strings.Contains(plain, want) {
			t.Errorf("retained selection view does not contain %q:\n%s", want, plain)
		}
	}
	for _, missing := range row.WantViewMissing {
		if strings.Contains(plain, missing) {
			t.Errorf("retained selection view unexpectedly contains %q:\n%s", missing, plain)
		}
	}
}

func TestVisibilityLoginRetainsMountedSelectionStateAndAsyncDelivery(t *testing.T) {
	for _, row := range loadRetainedLoginDocument(t).Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			loaded := config.BaseConfig()
			loaded.Selection = row.SavedSelection
			if err := config.SaveAtomic(path, loaded); err != nil {
				t.Fatalf("seed retained login config: %v", err)
			}
			draft, err := settings.NewDraft(path, loaded)
			if err != nil {
				t.Fatalf("open retained login draft: %v", err)
			}
			beforeConfig := mustReadFile(t, path)
			source := &countingTreeSource{inner: scannerfix.NewFixtureTreeSource(row.Fixture)}
			preview := &retainedPreviewSource{}
			loginCalls := 0
			program := kickstart.NewProgram(kickstart.ProgramDeps{
				Theme:   theme.New(theme.ModeDark),
				Draft:   draft,
				Source:  source,
				Preview: preview,
				Login: func(context.Context) (string, error) {
					loginCalls++
					return "fixture-user", nil
				},
			})
			program.SetSize(row.Width, row.Height)

			program, initialFlowCommand := program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if program.Phase() != kickstart.PhaseFlow || initialFlowCommand == nil {
				t.Fatalf("declining initial login did not enter and initialize flow; phase=%s commandNil=%t",
					program.Phase(), initialFlowCommand == nil)
			}

			switch row.Scenario {
			case retainedLoginPreservePresentation:
				program = runFlowInitOnce(t, program, initialFlowCommand)
				program = driveRetainedLoginKeys(t, program, row.BeforeLoginKeys)
				if !reflect.DeepEqual(draft.Working().Selection, row.ExpectedSelection) {
					t.Fatalf("interactive selection before login = %#v, want %#v", draft.Working().Selection, row.ExpectedSelection)
				}
				beforeState := retainedStateLines(t, program.View(), row.StateLineLabels)
				assertRetainedView(t, row, program.View())

				program = enterRetainedVisibility(t, program)
				program = completeRetainedVisibilityLogin(t, program)
				program = returnToRetainedSelection(t, program)
				afterState := retainedStateLines(t, program.View(), row.StateLineLabels)
				if !reflect.DeepEqual(afterState, beforeState) {
					t.Errorf("selection presentation changed across login:\n before=%q\n after=%q", beforeState, afterState)
				}
				assertRetainedView(t, row, program.View())
				if got := preview.lastID(); got != row.ExpectedPreviewID {
					t.Errorf("retained preview id=%q, want %q", got, row.ExpectedPreviewID)
				}

			case retainedLoginPendingTreeResult:
				if source.Loads() != 0 {
					t.Fatalf("held startup command loaded source %d times before delivery, want 0", source.Loads())
				}
				program = enterRetainedVisibility(t, program)
				program = completeRetainedVisibilityLogin(t, program)
				// Deliver the original Flow.Init result while sharing is current.
				// The retained selection field, not the focused sharing radio, owns it.
				program = runFlowInitOnce(t, program, initialFlowCommand)
				program = returnToRetainedSelection(t, program)
				assertRetainedView(t, row, program.View())
				if !reflect.DeepEqual(draft.Working().Selection, row.ExpectedSelection) {
					t.Fatalf("selection after delayed tree delivery = %#v, want %#v", draft.Working().Selection, row.ExpectedSelection)
				}
				program = advanceToConsent(t, program)
				program, _ = program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if row.CommitAfterDelivery && !program.Committed() {
					t.Fatalf("delayed tree result did not make retained selection committable; phase=%s view:\n%s",
						program.Phase(), stripRender(program.View()))
				}
				committed, err := config.Parse(mustReadFile(t, path))
				if err != nil {
					t.Fatalf("parse config committed after delayed tree result: %v", err)
				}
				if !reflect.DeepEqual(committed.Selection, row.ExpectedSelection) {
					t.Errorf("committed delayed selection = %#v, want %#v", committed.Selection, row.ExpectedSelection)
				}
			}

			if loginCalls != 1 {
				t.Errorf("visibility login calls=%d, want 1", loginCalls)
			}
			if source.Loads() != row.ExpectedSourceLoads {
				t.Errorf("selection source loads=%d, want one mounted-flow load", source.Loads())
			}
			if row.Scenario == retainedLoginPreservePresentation {
				if afterConfig := mustReadFile(t, path); !bytes.Equal(beforeConfig, afterConfig) {
					t.Error("visibility login changed config bytes before final consent")
				}
				if !reflect.DeepEqual(draft.Working().Selection, row.ExpectedSelection) {
					t.Errorf("buffered selection after login = %#v, want %#v", draft.Working().Selection, row.ExpectedSelection)
				}
			}
		})
	}
}
