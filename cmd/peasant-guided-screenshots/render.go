//go:build guided_screenshots

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
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
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, guidedSections[capture.Section].WantContains, nil); err != nil {
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
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, state.WantContains, state.WantAbsent); err != nil {
			return nil, err
		}
		selectionCaptures[capture.Name] = terminalCapture{name: capture.Name, view: view}
	}

	pushCaptures := make(map[string]terminalCapture, len(document.PushCaptures))
	pushStates := make(map[pushState]pushStateFixture, len(document.PushStates))
	for _, state := range document.PushStates {
		pushStates[state.Key] = state
	}
	for _, capture := range document.PushCaptures {
		view, renderErr := renderPushCapture(document.Push, capture)
		if renderErr != nil {
			return nil, renderErr
		}
		if err := validateTerminalCapture(capture.Name, view, capture.Width, capture.Height, pushStates[capture.State].WantContains, nil); err != nil {
			return nil, err
		}
		pushCaptures[capture.Name] = terminalCapture{name: capture.Name, view: view}
	}

	sheets := make([]renderedSheet, 0, len(document.Sheets))
	for _, sheet := range document.Sheets {
		var content string
		switch sheet.Kind {
		case sheetKindGuided:
			content, err = composeGuidedSheet(sheet, document.GuidedSections, document.GuidedCaptures, guidedCaptures)
		case sheetKindSelection:
			content, err = composeSelectionSheet(sheet, document.SelectionStates, document.SelectionCaptures, selectionCaptures)
		case sheetKindPush:
			content, err = composePushSheet(sheet, document.PushStates, document.PushCaptures, pushCaptures)
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
	listings, err := listingsWithHarnessTranscripts(workingDirectory, index, selection)
	if err != nil {
		return "", err
	}
	source := kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(capturePathResolver{}),
		kickstart.WithRepositoryIdentityResolver(newCaptureRepositoryResolver(selection.Repositories)),
		kickstart.WithIngestedSessionIDs(selection.Ingested),
	)
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:  th,
		Draft:  draft,
		Source: source,
		Preview: kickstart.NewListingPreview(
			th,
			listings,
			// The store first, then the transcript the harness wrote. This is
			// the order the mounted command wires, so a session with no store
			// row previews from its own source file.
			storedThenHarnessTurns(selection.Transcripts, kickstart.NewSourceTurns(&ingest.OSFileSystem{}, listings)),
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
	if state.Key == selectionStateBranchPreview {
		program = sendProgramMessage(program, tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	if state.Key == selectionStateSessionPreview || state.Key == selectionStateSourcePreview {
		program = advanceToMarkers(program, state.WantContains)
	}
	return program.View(), nil
}

// advanceToMarkers steps the tree cursor one visible row at a time until the
// screen shows every marker the state requires. Deriving the number of steps
// keeps the capture on the intended row when the tree adds or reorders a row.
// The capture validator still fails when no row shows the markers.
func advanceToMarkers(program kickstart.Program, markers []string) kickstart.Program {
	const maxSteps = 12
	for i := 0; i < maxSteps; i++ {
		if captureShowsMarkers(program.View(), markers) {
			return program
		}
		program = sendProgramMessage(program, tea.KeyPressMsg{Code: 'j', Text: "j"})
	}
	return program
}

func captureShowsMarkers(view string, markers []string) bool {
	plain := ansi.Strip(view)
	for _, marker := range markers {
		if !strings.Contains(plain, marker) {
			return false
		}
	}
	return true
}

// listingsWithHarnessTranscripts writes the fixture's harness transcripts into
// the isolated capture workspace and points their listings at them. The files
// exist only for the capture run and hold scrubbed fixture text.
func listingsWithHarnessTranscripts(workingDirectory string, index int, selection selectionFixture) ([]ftue.SessionListing, error) {
	listings := append([]ftue.SessionListing(nil), selection.Listings...)
	if len(selection.SourceTranscripts) == 0 {
		return listings, nil
	}
	dir := filepath.Join(workingDirectory, fmt.Sprintf("harness-%02d", index))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create harness transcript workspace: %w", err)
	}
	for i := range listings {
		lines, ok := selection.SourceTranscripts[listings[i].SessionID]
		if !ok {
			continue
		}
		path := filepath.Join(dir, listings[i].SessionID+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("write harness transcript %q: %w", path, err)
		}
		listings[i].Source.Path = path
	}
	return listings, nil
}

// storedThenHarnessTurns reads the store first and falls back to the harness
// transcript, which is the order the mounted command wires.
func storedThenHarnessTurns(transcripts map[string][]selectionTurnFixture, harness *kickstart.SourceTurns) kickstart.SessionTurnsFunc {
	stored := selectionTurns(transcripts)
	return func(sessionID string) ([]ingest.Turn, error) {
		turns, err := stored(sessionID)
		if err != nil {
			return nil, err
		}
		if len(turns) > 0 {
			return turns, nil
		}
		return harness.Turns(sessionID)
	}
}

// renderPushCapture drives the REAL push wizard - the same model the push
// command mounts - into one screen and returns its rendered view.
func renderPushCapture(fixture pushFixture, capture pushCaptureFixture) (string, error) {
	turns, err := pushPublishedTurns(fixture)
	if err != nil {
		return "", fmt.Errorf("render push capture %q: %w", capture.Name, err)
	}
	model := push.NewPushWizard(captureThemeValue(capture.Theme), pushWizardSessions(fixture), turns)
	current := sendPushMessage(model, tea.WindowSizeMsg{Width: capture.Width, Height: capture.Height})
	switch capture.State {
	case pushStateStart:
	case pushStateSelection:
		current = acceptPushStart(current)
	case pushStateSessionPreview:
		// One row down from the opening project row is its session, which is
		// what makes the pane draw a transcript.
		current = sendPushMessage(acceptPushStart(current), tea.KeyPressMsg{Code: tea.KeyDown})
	case pushStateConsent:
		current = sendPushMessage(acceptPushStart(current), tea.KeyPressMsg{Code: tea.KeyEnter})
	case pushStateReceipt:
		current = sendPushMessage(acceptPushStart(current), tea.KeyPressMsg{Code: tea.KeyEnter})
		current = sendPushMessage(current, tea.KeyPressMsg{Code: tea.KeyEnter})
	default:
		return "", fmt.Errorf("render push capture %q: unknown push state %q", capture.Name, capture.State)
	}
	return current.View().Content, nil
}

// acceptPushStart answers the opening prompt with yes, which opens the
// selection tree. The prompt opens on "no", so the left key moves onto "yes"
// before the answer.
func acceptPushStart(model push.PushWizardModel) push.PushWizardModel {
	model = sendPushMessage(model, tea.KeyPressMsg{Code: tea.KeyLeft})
	return sendPushMessage(model, tea.KeyPressMsg{Code: tea.KeyEnter})
}

// sendPushMessage advances the wizard and settles the work the message started.
//
// It settles FOLLOW-UP work too, which is what gets a preview onto the capture.
// Answering the opening prompt produces a result message, and handling that
// message is what issues the preview load; a drain that stopped at the first
// level dropped the load and every selection capture read "loading preview".
func sendPushMessage(model push.PushWizardModel, message tea.Msg) push.PushWizardModel {
	updated, command := model.Update(message)
	return drainPushWizard(updated.(push.PushWizardModel), command, pushDrainDepth)
}

// pushDrainDepth bounds the follow-up drain so a repeating animation tick
// cannot loop the capture.
const pushDrainDepth = 3

func drainPushWizard(model push.PushWizardModel, command tea.Cmd, depth int) push.PushWizardModel {
	if command == nil || depth <= 0 {
		return model
	}
	for _, message := range collectMessages(command) {
		updated, follow := model.Update(message)
		model = updated.(push.PushWizardModel)
		model = drainPushWizard(model, follow, depth-1)
	}
	return model
}

// pushWizardSessions turns the fixture inventory into the candidate list the
// wizard mounts over.
func pushWizardSessions(fixture pushFixture) []push.PushWizardSession {
	sessions := make([]push.PushWizardSession, 0, len(fixture.Sessions))
	for _, row := range fixture.Sessions {
		candidate := push.PushWizardSession{
			Row: ingest.PushSessionRow{
				SessionID:    row.SessionID,
				ModelHarness: row.Harness,
				ProjectName:  row.Project,
				StartMs:      row.StartMs,
			},
			Meta:   pushCaptureMetadata(row.Redaction),
			Action: push.PushWithRedaction,
		}
		if row.Withheld {
			candidate.Action = push.PushExclude
			candidate.Locked = true
		}
		sessions = append(sessions, candidate)
	}
	return sessions
}

// pushPublishedTurns is the preview read the captured wizard mounts with: the
// fixture's recorded entries, through the REAL push redactor at the level a
// push runs at. The sheet therefore shows the transcript a publish would send,
// including the placeholders redaction leaves behind.
func pushPublishedTurns(fixture pushFixture) (push.PublishedTurnsFunc, error) {
	redactor, err := redact.NewRedactor(config.RecommendedRedactionLevel, nil, redact.XDGPaths{})
	if err != nil {
		return nil, fmt.Errorf("build the push preview redactor: %w", err)
	}
	stored := make(map[string][]schema.SessionEntry, len(fixture.Transcripts))
	for sessionID, rows := range fixture.Transcripts {
		entries := make([]schema.SessionEntry, 0, len(rows))
		for index, row := range rows {
			content := row.Content
			entries = append(entries, schema.SessionEntry{
				SessionID:      schema.SessionID(sessionID),
				EntryIndex:     index,
				EntryType:      schema.EntryType(row.EntryType),
				Role:           schema.Role(row.Role),
				ContentPreview: &content,
			})
		}
		stored[sessionID] = entries
	}
	return push.NewPublishedTurns(func(sessionID string) ([]schema.SessionEntry, error) {
		return stored[sessionID], nil
	}, redactor), nil
}

// pushCaptureMetadata builds the stored-copy record one redaction state
// produces, so the captured rows read as the wizard reads them.
func pushCaptureMetadata(state pushRedactionState) *schema.UnifiedMetadata {
	switch state {
	case pushRedactionCurrent:
		return &schema.UnifiedMetadata{
			ContentHash: "content-hash",
			Redaction:   schema.RedactionInfo{Applied: true, ContentHashAtRedact: "content-hash"},
		}
	case pushRedactionStale:
		return &schema.UnifiedMetadata{
			ContentHash: "content-hash-new",
			Redaction:   schema.RedactionInfo{Applied: true, ContentHashAtRedact: "content-hash-old"},
		}
	default:
		return &schema.UnifiedMetadata{ContentHash: "content-hash"}
	}
}

func selectionTurns(transcripts map[string][]selectionTurnFixture) kickstart.SessionTurnsFunc {
	return func(sessionID string) ([]ingest.Turn, error) {
		rows := transcripts[sessionID]
		turns := make([]ingest.Turn, 0, len(rows))
		for index, row := range rows {
			turns = append(turns, ingest.Turn{
				Index:     index,
				Role:      row.Role,
				EntryType: row.EntryType,
				Content:   row.Content,
			})
		}
		return turns, nil
	}
}

type capturePathResolver struct{}

func (capturePathResolver) Resolve(dir string) (ingest.ClonePath, error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("capture path %q is not a clean absolute path", dir)
	}
	return ingest.ClonePath(dir), nil
}

type captureRepositoryResolver map[ingest.ClonePath]ingest.RepositoryIdentity

func newCaptureRepositoryResolver(fixtures []selectionRepositoryFixture) captureRepositoryResolver {
	resolver := make(captureRepositoryResolver, len(fixtures))
	for _, fixture := range fixtures {
		resolver[ingest.ClonePath(fixture.ClonePath)] = ingest.RepositoryIdentity{
			CohortKey:    ingest.RepositoryCohortKey(fixture.CohortKey),
			GitDirectory: ingest.RepositoryPath(fixture.GitDirectory),
		}
	}
	return resolver
}

func (r captureRepositoryResolver) ResolveRepositoryIdentity(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	identity, ok := r[clonePath]
	if !ok {
		return ingest.RepositoryIdentity{}, fmt.Errorf("capture fixture has no repository identity for %q", clonePath)
	}
	return identity, nil
}

var _ ingest.PathIdentityResolver = capturePathResolver{}
var _ ingest.RepositoryIdentityResolver = captureRepositoryResolver{}

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

func validateTerminalCapture(name, view string, width, height int, wantContains, wantAbsent []string) error {
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
	for _, marker := range wantAbsent {
		if strings.Contains(plain, marker) {
			return fmt.Errorf("render terminal capture %q: mounted view carries forbidden marker %q, which must stay hidden", name, marker)
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
		themes := []captureTheme{captureThemeDark}
		if state.Key.requiresBothThemes() {
			themes = append(themes, captureThemeLight)
		}
		for _, captureTheme := range themes {
			left, err := selectionCaptureFor(cases, captures, state.Key, captureTheme, 80, 24)
			if err != nil {
				return "", err
			}
			right, err := selectionCaptureFor(cases, captures, state.Key, captureTheme, 120, 40)
			if err != nil {
				return "", err
			}
			rows = append(rows, joinCapturePair(left, right))
		}
	}
	return renderContactSheet(sheet.Title, rows), nil
}

func composePushSheet(
	sheet sheetFixture,
	states []pushStateFixture,
	cases []pushCaptureFixture,
	captures map[string]terminalCapture,
) (string, error) {
	rows := make([]string, 0, len(states)*2)
	for _, state := range states {
		for _, name := range []captureTheme{captureThemeDark, captureThemeLight} {
			left, err := pushCaptureFor(cases, captures, state.Key, name, 80, 24)
			if err != nil {
				return "", err
			}
			right, err := pushCaptureFor(cases, captures, state.Key, name, 120, 40)
			if err != nil {
				return "", err
			}
			rows = append(rows, joinCapturePair(left, right))
		}
	}
	return renderContactSheet(sheet.Title, rows), nil
}

func pushCaptureFor(
	cases []pushCaptureFixture,
	captures map[string]terminalCapture,
	state pushState,
	captureTheme captureTheme,
	width, height int,
) (terminalCapture, error) {
	for _, capture := range cases {
		if capture.State == state && capture.Theme == captureTheme && capture.Width == width && capture.Height == height {
			return captures[capture.Name], nil
		}
	}
	return terminalCapture{}, fmt.Errorf("compose push sheet: no capture for %s/%s/%dx%d", state, captureTheme, width, height)
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
	captureTheme captureTheme,
	width, height int,
) (terminalCapture, error) {
	for _, capture := range cases {
		if capture.State == state && capture.Theme == captureTheme && capture.Width == width && capture.Height == height {
			return captures[capture.Name], nil
		}
	}
	return terminalCapture{}, fmt.Errorf("compose selection sheet: no capture for %s/%s/%dx%d", state, captureTheme, width, height)
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
