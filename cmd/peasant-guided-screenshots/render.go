//go:build guided_screenshots

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

type terminalCapture struct {
	name string
	view string
}

type renderedSheet struct {
	fixture sheetFixture
	ansi    string
}

func renderSheets(document captureDocument) ([]renderedSheet, error) {
	workingDirectory, err := os.MkdirTemp("", "peasant-guided-screenshots-")
	if err != nil {
		return nil, fmt.Errorf("create isolated screenshot workspace: %w", err)
	}
	defer os.RemoveAll(workingDirectory)

	guidedCaptures := make(map[string]terminalCapture, len(document.GuidedCaptures))
	guidedSections := make(map[guidedSection]guidedSectionFixture, len(document.GuidedSections))
	for _, section := range document.GuidedSections {
		guidedSections[section.Key] = section
	}
	for index, capture := range document.GuidedCaptures {
		view, renderErr := renderGuidedCapture(workingDirectory, index, capture)
		if renderErr != nil {
			return nil, renderErr
		}
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, guidedSections[capture.Section].WantContains); err != nil {
			return nil, err
		}
		guidedCaptures[capture.Name] = terminalCapture{name: capture.Name, view: view}
	}

	selectionCaptures := make(map[string]terminalCapture, len(document.SelectionCaptures))
	selectionStates := make(map[selectionState]selectionStateFixture, len(document.SelectionStates))
	for _, state := range document.SelectionStates {
		selectionStates[state.Key] = state
	}
	for index, capture := range document.SelectionCaptures {
		state := selectionStates[capture.State]
		view, renderErr := renderSelectionCapture(workingDirectory, index, document.Selection, capture, state)
		if renderErr != nil {
			return nil, renderErr
		}
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, state.WantContains); err != nil {
			return nil, err
		}
		selectionCaptures[capture.Name] = terminalCapture{name: capture.Name, view: view}
	}

	sheets := make([]renderedSheet, 0, len(document.Sheets))
	for _, sheet := range document.Sheets {
		var content string
		switch sheet.Kind {
		case sheetKindGuided:
			content, err = composeGuidedSheet(sheet, document.GuidedSections, document.GuidedCaptures, guidedCaptures)
		case sheetKindSelection:
			content, err = composeSelectionSheet(sheet, document.SelectionStates, document.SelectionCaptures, selectionCaptures)
		default:
			err = fmt.Errorf("compose unknown screenshot sheet kind %q", sheet.Kind)
		}
		if err != nil {
			return nil, err
		}
		sheets = append(sheets, renderedSheet{fixture: sheet, ansi: content})
	}
	return sheets, nil
}

func renderGuidedCapture(workingDirectory string, index int, capture guidedCaptureFixture) (string, error) {
	draft, err := newCaptureDraft(workingDirectory, fmt.Sprintf("guided-%02d", index), true)
	if err != nil {
		return "", err
	}
	registry := kickstart.BuildRegistry(kickstart.Options{
		Source:                scannerfix.NewFixtureTreeSource("standard"),
		VillageConnected:      true,
		ClaudeSessionsPresent: true,
	})
	var selected settings.Section
	for _, section := range registry.Sections {
		if section.Key == string(capture.Section) {
			selected = section
			break
		}
	}
	if selected.Key == "" {
		return "", fmt.Errorf("render guided capture %q: canonical registry has no section %q", capture.Name, capture.Section)
	}
	flow := settings.NewFlow(captureThemeValue(capture.Theme), settings.Registry{Sections: []settings.Section{selected}}, draft)
	flow.SetSize(capture.Width, capture.Height)
	return flow.View(), nil
}

func renderSelectionCapture(
	workingDirectory string,
	index int,
	selection selectionFixture,
	capture selectionCaptureFixture,
	state selectionStateFixture,
) (string, error) {
	draft, err := newCaptureDraft(workingDirectory, fmt.Sprintf("selection-%02d", index), false)
	if err != nil {
		return "", err
	}
	th := captureThemeValue(capture.Theme)
	source := kickstart.NewScannerTreeSource(
		selection.Listings,
		kickstart.WithIngestedSessionIDs(selection.Ingested),
	)
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:  th,
		Draft:  draft,
		Source: source,
		Preview: kickstart.NewListingPreview(
			th,
			selection.Listings,
			nil,
			kickstart.WithListingPreviewContextSource(source),
		),
	})
	program.SetSize(capture.Width, capture.Height)
	program, command := program.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	program = drainProgram(program, command)
	if program.Phase() != kickstart.PhaseFlow {
		return "", fmt.Errorf("render selection capture %q: declining the optional login left phase=%s, want flow", capture.Name, program.Phase())
	}
	if state.Query != "" {
		program = sendProgramMessage(program, tea.KeyPressMsg{Code: '/', Text: "/"})
		for _, value := range state.Query {
			program = sendProgramMessage(program, tea.KeyPressMsg{Code: value, Text: string(value)})
		}
		program = sendProgramMessage(program, tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	return program.View(), nil
}

func newCaptureDraft(workingDirectory, name string, selected bool) (*settings.Draft, error) {
	configPath := filepath.Join(workingDirectory, name, "config.yaml")
	loaded := config.BaseConfig()
	if selected {
		loaded.Selection.Mode = config.SelectionModeSelected
	}
	if err := config.SaveAtomic(configPath, loaded); err != nil {
		return nil, fmt.Errorf("seed screenshot config %q: %w", configPath, err)
	}
	draft, err := settings.NewDraft(configPath, loaded)
	if err != nil {
		return nil, fmt.Errorf("open screenshot draft %q: %w", configPath, err)
	}
	return draft, nil
}

func captureThemeValue(name captureTheme) theme.Theme {
	if name == captureThemeLight {
		return theme.New(theme.ModeLight)
	}
	return theme.New(theme.ModeDark)
}

func validateTerminalCapture(name, view string, width, height int, wantContains []string) error {
	lines := strings.Split(view, "\n")
	if len(lines) != height {
		return fmt.Errorf("render terminal capture %q: height=%d, want exactly %d rows", name, len(lines), height)
	}
	for index, line := range lines {
		if lineWidth := lipgloss.Width(line); lineWidth != width {
			return fmt.Errorf("render terminal capture %q: row %d width=%d, want exactly %d cells", name, index, lineWidth, width)
		}
	}
	plain := ansi.Strip(view)
	for _, marker := range wantContains {
		if !strings.Contains(plain, marker) {
			return fmt.Errorf("render terminal capture %q: mounted view omits required marker %q", name, marker)
		}
	}
	return nil
}

func composeGuidedSheet(
	sheet sheetFixture,
	sections []guidedSectionFixture,
	cases []guidedCaptureFixture,
	captures map[string]terminalCapture,
) (string, error) {
	rows := make([]string, 0, len(sections))
	for _, section := range sections {
		left, err := guidedCaptureFor(cases, captures, section.Key, sheet.Theme, 80, 24)
		if err != nil {
			return "", err
		}
		right, err := guidedCaptureFor(cases, captures, section.Key, sheet.Theme, 120, 40)
		if err != nil {
			return "", err
		}
		rows = append(rows, joinCapturePair(left, right))
	}
	return renderContactSheet(sheet.Title, rows), nil
}

func composeSelectionSheet(
	sheet sheetFixture,
	states []selectionStateFixture,
	cases []selectionCaptureFixture,
	captures map[string]terminalCapture,
) (string, error) {
	rows := make([]string, 0, len(states))
	for _, state := range states {
		left, err := selectionCaptureFor(cases, captures, state.Key, 80, 24)
		if err != nil {
			return "", err
		}
		right, err := selectionCaptureFor(cases, captures, state.Key, 120, 40)
		if err != nil {
			return "", err
		}
		rows = append(rows, joinCapturePair(left, right))
	}
	return renderContactSheet(sheet.Title, rows), nil
}

func guidedCaptureFor(
	cases []guidedCaptureFixture,
	captures map[string]terminalCapture,
	section guidedSection,
	captureTheme captureTheme,
	width, height int,
) (terminalCapture, error) {
	for _, capture := range cases {
		if capture.Section == section && capture.Theme == captureTheme && capture.Width == width && capture.Height == height {
			return captures[capture.Name], nil
		}
	}
	return terminalCapture{}, fmt.Errorf("compose guided sheet: no capture for %s/%s/%dx%d", section, captureTheme, width, height)
}

func selectionCaptureFor(
	cases []selectionCaptureFixture,
	captures map[string]terminalCapture,
	state selectionState,
	width, height int,
) (terminalCapture, error) {
	for _, capture := range cases {
		if capture.State == state && capture.Width == width && capture.Height == height {
			return captures[capture.Name], nil
		}
	}
	return terminalCapture{}, fmt.Errorf("compose selection sheet: no capture for %s/%dx%d", state, width, height)
}

func joinCapturePair(left, right terminalCapture) string {
	labelStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#bdb7aa")).Bold(true)
	leftBlock := labelStyle.Render(left.name) + "\n" + left.view
	rightBlock := labelStyle.Render(right.name) + "\n" + right.view
	return lipgloss.JoinHorizontal(lipgloss.Top, leftBlock, strings.Repeat(" ", 4), rightBlock)
}

func renderContactSheet(title string, rows []string) string {
	titleStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#f8f5ed")).Bold(true)
	return titleStyle.Render(title) + "\n\n" + strings.Join(rows, "\n\n")
}

func sendProgramMessage(program kickstart.Program, message tea.Msg) kickstart.Program {
	next, command := program.Update(message)
	return drainProgram(next, command)
}

func drainProgram(program kickstart.Program, command tea.Cmd) kickstart.Program {
	commands := []tea.Cmd{command}
	for len(commands) > 0 {
		command = commands[0]
		commands = commands[1:]
		for _, message := range collectMessages(command) {
			var follow tea.Cmd
			program, follow = program.Update(message)
			if follow != nil {
				commands = append(commands, follow)
			}
		}
	}
	return program
}

func collectMessages(command tea.Cmd) []tea.Msg {
	if command == nil {
		return nil
	}
	message := command()
	if batch, ok := message.(tea.BatchMsg); ok {
		var messages []tea.Msg
		for _, child := range batch {
			messages = append(messages, collectMessages(child)...)
		}
		return messages
	}
	if message == nil {
		return nil
	}
	return []tea.Msg{message}
}
