//go:build guided_screenshots

package main

import (
	"context"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
)

type completionState string

const (
	completionWarningsTop    completionState = "warnings-top"
	completionWarningsBottom completionState = "warnings-bottom"
	completionNoWarnings     completionState = "no-warnings"
)

func (s completionState) valid() bool {
	return s == completionWarningsTop || s == completionWarningsBottom || s == completionNoWarnings
}

type completionStateFixture struct {
	Key          completionState `yaml:"key"`
	WantContains []string        `yaml:"wantContains"`
	WantAbsent   []string        `yaml:"wantAbsent"`
}

type completionCaptureFixture struct {
	Name   string          `yaml:"name"`
	State  completionState `yaml:"state"`
	Theme  captureTheme    `yaml:"theme"`
	Width  int             `yaml:"width"`
	Height int             `yaml:"height"`
}

type completionFixture struct {
	New         int                        `yaml:"new"`
	Updated     int                        `yaml:"updated"`
	Unchanged   int                        `yaml:"unchanged"`
	Errors      int                        `yaml:"errors"`
	Diagnostics []ingest.DiagnosticEntry   `yaml:"diagnostics"`
	States      []completionStateFixture   `yaml:"states"`
	Captures    []completionCaptureFixture `yaml:"captures"`
}

func validateCompletionMatrix(fixture completionFixture) error {
	states := map[completionState]bool{}
	for _, state := range fixture.States {
		if !state.Key.valid() || states[state.Key] || !nonEmptyStrings(state.WantContains) {
			return fmt.Errorf("invalid completion state %q", state.Key)
		}
		states[state.Key] = true
	}
	for _, required := range []completionState{completionWarningsTop, completionWarningsBottom, completionNoWarnings} {
		if !states[required] {
			return fmt.Errorf("screenshot fixture omits completion state %q", required)
		}
	}
	if len(fixture.Diagnostics) == 0 {
		return fmt.Errorf("completion fixture omits representative warning data")
	}
	for _, diagnostic := range fixture.Diagnostics {
		if diagnostic.Location == "" || diagnostic.Message == "" || diagnostic.Remediation == "" {
			return fmt.Errorf("completion diagnostic omits reason, location, or remedy")
		}
	}
	names := map[string]bool{}
	for _, capture := range fixture.Captures {
		want := fmt.Sprintf("ingest-completion-%s-%s-%dx%d", capture.State, capture.Theme, capture.Width, capture.Height)
		if capture.Name != want || names[capture.Name] || !states[capture.State] || !capture.Theme.valid() || !validCaptureSize(capture.Width, capture.Height) {
			return fmt.Errorf("invalid completion capture %q", capture.Name)
		}
		names[capture.Name] = true
	}
	for state := range states {
		for _, mode := range []captureTheme{captureThemeDark, captureThemeLight} {
			for _, size := range []viewportFixture{{Width: 80, Height: 24}, {Width: 120, Height: 40}} {
				name := fmt.Sprintf("ingest-completion-%s-%s-%dx%d", state, mode, size.Width, size.Height)
				if !names[name] {
					return fmt.Errorf("screenshot fixture omits completion capture %q", name)
				}
			}
		}
	}
	return nil
}

func renderCompletionCapture(directory string, index int, fixture completionFixture, capture completionCaptureFixture) (string, error) {
	draft, err := newCaptureDraft(directory, fmt.Sprintf("ingest-completion-%02d", index), true)
	if err != nil {
		return "", err
	}
	result := &ftue.IngestResult{New: fixture.New, Updated: fixture.Updated, Unchanged: fixture.Unchanged, Errors: fixture.Errors}
	if capture.State != completionNoWarnings {
		result.Diagnostics = fixture.Diagnostics
	}
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme: captureThemeValue(capture.Theme), Draft: draft, AlreadyConnected: true,
		Source: scannerfix.NewFixtureTreeSource("standard"),
		Ingest: func(context.Context) (*ftue.IngestResult, error) { return result, nil },
	})
	program.SetSize(capture.Width, capture.Height)
	program = drainProgram(program, program.Init())
	program, command := advanceProgramToIngest(program)
	if program.Phase() != kickstart.PhaseIngest || command == nil {
		return "", fmt.Errorf("completion capture %q did not start local ingest", capture.Name)
	}
	program = drainProgram(program, command)
	if program.Phase() != kickstart.PhaseDone || program.IngestErr() != nil || program.IngestResult() != result {
		return "", fmt.Errorf("completion capture %q did not retain successful local result", capture.Name)
	}
	if capture.State == completionWarningsBottom {
		program = sendProgramMessage(program, tea.KeyPressMsg{Code: 'G', Text: "G"})
	}
	return program.View(), nil
}

func renderCompletionSheet(directory string, sheet sheetFixture, fixture completionFixture) (string, []sheetRow, error) {
	captures := map[string]terminalCapture{}
	states := map[completionState]completionStateFixture{}
	for _, state := range fixture.States {
		states[state.Key] = state
	}
	for index, capture := range fixture.Captures {
		view, err := renderCompletionCapture(directory, index, fixture, capture)
		if err != nil {
			return "", nil, err
		}
		state := states[capture.State]
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, state.WantContains, state.WantAbsent); err != nil {
			return "", nil, err
		}
		captures[capture.Name] = terminalCapture{name: capture.Name, view: view}
	}
	var rows []string
	var metadata []sheetRow
	for _, state := range fixture.States {
		for _, mode := range []captureTheme{captureThemeDark, captureThemeLight} {
			left := captures[fmt.Sprintf("ingest-completion-%s-%s-80x24", state.Key, mode)]
			right := captures[fmt.Sprintf("ingest-completion-%s-%s-120x40", state.Key, mode)]
			row := joinCapturePair(left, right)
			rows = append(rows, row)
			metadata = append(metadata, sheetRow{label: fmt.Sprintf("completion state %q (%s)", state.Key, mode), lines: strings.Count(row, "\n") + 1})
		}
	}
	return renderContactSheet(sheet.Title, rows), metadata, nil
}
