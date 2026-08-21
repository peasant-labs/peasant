//go:build guided_screenshots

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"gopkg.in/yaml.v3"
)

const (
	requiredSheetCount             = 4
	requiredGuidedSectionCount     = 6
	requiredGuidedCaptureCount     = 24
	requiredSelectionStateCount    = 6
	requiredSelectionCaptureCount  = 20
	requiredSelectionSessionCount  = 6
	requiredSelectionHarnessCount  = 2
	requiredSelectionIngestedCount = 1
	requiredPushStateCount         = 4
	requiredPushCaptureCount       = 16
	requiredPushSessionCount       = 3
)

type sheetName string

const (
	sheetGuidedDark  sheetName = "guided-dark"
	sheetGuidedLight sheetName = "guided-light"
	sheetSelection   sheetName = "selection"
	sheetPush        sheetName = "push"
)

type sheetKind string

const (
	sheetKindGuided    sheetKind = "guided"
	sheetKindSelection sheetKind = "selection"
	sheetKindPush      sheetKind = "push"
)

type captureTheme string

const (
	captureThemeDark  captureTheme = "dark"
	captureThemeLight captureTheme = "light"
)

func (t captureTheme) valid() bool {
	return t == captureThemeDark || t == captureThemeLight
}

type guidedSection string

const (
	guidedSectionAutoIngest  guidedSection = kickstart.SectionAutoIngest
	guidedSectionPublication guidedSection = kickstart.SectionPublication
	guidedSectionPrivacy     guidedSection = kickstart.SectionPrivacy
	guidedSectionLicense     guidedSection = kickstart.SectionLicense
	guidedSectionDestination guidedSection = kickstart.SectionDestination
	guidedSectionRetention   guidedSection = kickstart.SectionRetention
)

func (s guidedSection) valid() bool {
	switch s {
	case guidedSectionAutoIngest, guidedSectionPublication, guidedSectionPrivacy,
		guidedSectionLicense, guidedSectionDestination, guidedSectionRetention:
		return true
	default:
		return false
	}
}

type selectionState string

const (
	selectionStateDefault        selectionState = "default"
	selectionStateSearch         selectionState = "search"
	selectionStateProjectPreview selectionState = "project-preview"
	selectionStateBranchPreview  selectionState = "branch-preview"
	selectionStateSessionPreview selectionState = "session-preview"
	// selectionStateSourcePreview is a session the local store does not hold,
	// previewed from the transcript its harness wrote.
	selectionStateSourcePreview selectionState = "harness-source-preview"
)

func (s selectionState) valid() bool {
	return s == selectionStateDefault || s == selectionStateSearch || s == selectionStateProjectPreview ||
		s == selectionStateBranchPreview || s == selectionStateSessionPreview || s == selectionStateSourcePreview
}

func (s selectionState) requiresBothThemes() bool {
	return s == selectionStateProjectPreview || s == selectionStateBranchPreview ||
		s == selectionStateSessionPreview || s == selectionStateSourcePreview
}

// pushState is the closed set of push-wizard screens the harness captures: the
// consent prompt that opens the wizard, the selection tree with its preview,
// the page that states what leaves the machine, and the receipt.
type pushState string

const (
	pushStateStart     pushState = "start"
	pushStateSelection pushState = "selection"
	pushStateConsent   pushState = "consent"
	pushStateReceipt   pushState = "receipt"
)

func (s pushState) valid() bool {
	switch s {
	case pushStateStart, pushStateSelection, pushStateConsent, pushStateReceipt:
		return true
	default:
		return false
	}
}

// pushRedactionState names the state of one fixture session's stored copy.
type pushRedactionState string

const (
	pushRedactionCurrent pushRedactionState = "current"
	pushRedactionStale   pushRedactionState = "stale"
	pushRedactionRaw     pushRedactionState = "raw"
)

func (s pushRedactionState) valid() bool {
	switch s {
	case pushRedactionCurrent, pushRedactionStale, pushRedactionRaw:
		return true
	default:
		return false
	}
}

// pushStateFixture names what one captured push screen must show.
type pushStateFixture struct {
	Key          pushState `yaml:"key"`
	WantContains []string  `yaml:"wantContains"`
}

// pushCaptureFixture is one push screen at one palette and one region.
type pushCaptureFixture struct {
	Name   string       `yaml:"name"`
	State  pushState    `yaml:"state"`
	Theme  captureTheme `yaml:"theme"`
	Width  int          `yaml:"width"`
	Height int          `yaml:"height"`
}

// pushSessionFixture is one candidate session the captured wizard offers.
type pushSessionFixture struct {
	SessionID string             `yaml:"sessionId"`
	Harness   string             `yaml:"harness"`
	Project   string             `yaml:"project"`
	StartMs   int64              `yaml:"startMs"`
	Redaction pushRedactionState `yaml:"redaction"`
	Withheld  bool               `yaml:"withheld"`
}

// pushFixture is the candidate inventory the captured wizard mounts over.
type pushFixture struct {
	Sessions []pushSessionFixture `yaml:"sessions"`
}

type viewportFixture struct {
	Width  int `yaml:"width"`
	Height int `yaml:"height"`
}

type sheetFixture struct {
	Name     sheetName       `yaml:"name"`
	Kind     sheetKind       `yaml:"kind"`
	Title    string          `yaml:"title"`
	Theme    captureTheme    `yaml:"theme"`
	Viewport viewportFixture `yaml:"viewport"`
}

type guidedSectionFixture struct {
	Key          guidedSection `yaml:"key"`
	WantContains []string      `yaml:"wantContains"`
}

type selectionStateFixture struct {
	Key          selectionState `yaml:"key"`
	Query        string         `yaml:"query"`
	WantContains []string       `yaml:"wantContains"`
}

type guidedCaptureFixture struct {
	Name    string        `yaml:"name"`
	Section guidedSection `yaml:"section"`
	Theme   captureTheme  `yaml:"theme"`
	Width   int           `yaml:"width"`
	Height  int           `yaml:"height"`
}

type selectionCaptureFixture struct {
	Name   string         `yaml:"name"`
	State  selectionState `yaml:"state"`
	Theme  captureTheme   `yaml:"theme"`
	Width  int            `yaml:"width"`
	Height int            `yaml:"height"`
}

type selectionFixture struct {
	Repositories []selectionRepositoryFixture      `yaml:"repositories"`
	Listings     []ftue.SessionListing             `yaml:"listings"`
	Transcripts  map[string][]selectionTurnFixture `yaml:"transcripts"`
	// SourceTranscripts holds the harness transcript lines for sessions the
	// local store does not hold. The renderer writes each one to its isolated
	// workspace and points the listing at it, so the preview reads a real file
	// through the production reader.
	SourceTranscripts map[string][]string `yaml:"sourceTranscripts"`
	Ingested          []string            `yaml:"ingested"`
}

type selectionRepositoryFixture struct {
	ClonePath    string `yaml:"clonePath"`
	CohortKey    string `yaml:"cohortKey"`
	GitDirectory string `yaml:"gitDirectory"`
}

type selectionTurnFixture struct {
	Role      ingest.Role      `yaml:"role"`
	EntryType ingest.EntryType `yaml:"entryType"`
	Content   string           `yaml:"content"`
}

type captureDocument struct {
	ExpectedPushStateCount         int                       `yaml:"expectedPushStateCount"`
	ExpectedPushCaptureCount       int                       `yaml:"expectedPushCaptureCount"`
	ExpectedPushSessionCount       int                       `yaml:"expectedPushSessionCount"`
	PushStates                     []pushStateFixture        `yaml:"pushStates"`
	PushCaptures                   []pushCaptureFixture      `yaml:"pushCaptures"`
	Push                           pushFixture               `yaml:"push"`
	ExpectedSheetCount             int                       `yaml:"expectedSheetCount"`
	ExpectedGuidedSectionCount     int                       `yaml:"expectedGuidedSectionCount"`
	ExpectedGuidedCaptureCount     int                       `yaml:"expectedGuidedCaptureCount"`
	ExpectedSelectionStateCount    int                       `yaml:"expectedSelectionStateCount"`
	ExpectedSelectionCaptureCount  int                       `yaml:"expectedSelectionCaptureCount"`
	ExpectedSelectionSessionCount  int                       `yaml:"expectedSelectionSessionCount"`
	ExpectedSelectionHarnessCount  int                       `yaml:"expectedSelectionHarnessCount"`
	ExpectedSelectionIngestedCount int                       `yaml:"expectedSelectionIngestedCount"`
	Sheets                         []sheetFixture            `yaml:"sheets"`
	GuidedSections                 []guidedSectionFixture    `yaml:"guidedSections"`
	SelectionStates                []selectionStateFixture   `yaml:"selectionStates"`
	Selection                      selectionFixture          `yaml:"selection"`
	GuidedCaptures                 []guidedCaptureFixture    `yaml:"guidedCaptures"`
	SelectionCaptures              []selectionCaptureFixture `yaml:"selectionCaptures"`
}

//go:embed testdata/captures.yaml
var captureFixtureData []byte

func decodeCaptureDocument(data []byte) (captureDocument, error) {
	var document captureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("decode screenshot fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return document, fmt.Errorf("screenshot fixture must contain exactly one YAML document: %w", err)
	}
	if err := validateDeclaredCounts(document); err != nil {
		return document, err
	}
	if err := validateSheets(document.Sheets); err != nil {
		return document, err
	}
	if err := validateGuidedMatrix(document.GuidedSections, document.GuidedCaptures); err != nil {
		return document, err
	}
	if err := validateSelectionMatrix(document.SelectionStates, document.SelectionCaptures); err != nil {
		return document, err
	}
	if err := validateSelectionData(document.Selection); err != nil {
		return document, err
	}
	if err := validatePushMatrix(document.PushStates, document.PushCaptures); err != nil {
		return document, err
	}
	if err := validatePushData(document.Push); err != nil {
		return document, err
	}
	return document, nil
}

// validatePushMatrix requires one capture per push screen, per palette, per
// region: the push wizard is reviewed in both themes at both sizes.
func validatePushMatrix(states []pushStateFixture, captures []pushCaptureFixture) error {
	stateRows := make(map[pushState]pushStateFixture, len(states))
	for _, state := range states {
		if !state.Key.valid() || stateRows[state.Key].Key != "" || !nonEmptyStrings(state.WantContains) {
			return fmt.Errorf("screenshot fixture has an invalid or duplicate push state: %#v", state)
		}
		stateRows[state.Key] = state
	}
	for _, key := range []pushState{pushStateStart, pushStateSelection, pushStateConsent, pushStateReceipt} {
		if stateRows[key].Key == "" {
			return fmt.Errorf("screenshot fixture omits push state %q", key)
		}
	}
	seenNames := make(map[string]bool, len(captures))
	pairs := make(map[string]int, len(captures))
	for _, capture := range captures {
		wantName := fmt.Sprintf("push-%s-%s-%dx%d", capture.State, capture.Theme, capture.Width, capture.Height)
		if capture.Name != wantName || seenNames[capture.Name] || stateRows[capture.State].Key == "" ||
			!capture.Theme.valid() || !validCaptureSize(capture.Width, capture.Height) {
			return fmt.Errorf("screenshot fixture has an invalid or duplicate push capture: %#v", capture)
		}
		seenNames[capture.Name] = true
		pairs[fmt.Sprintf("%s/%s/%dx%d", capture.State, capture.Theme, capture.Width, capture.Height)]++
	}
	for state := range stateRows {
		for _, name := range []captureTheme{captureThemeDark, captureThemeLight} {
			for _, viewport := range []viewportFixture{{Width: 80, Height: 24}, {Width: 120, Height: 40}} {
				pair := fmt.Sprintf("%s/%s/%dx%d", state, name, viewport.Width, viewport.Height)
				if pairs[pair] != 1 {
					return fmt.Errorf("screenshot fixture push pair %q has %d captures, want exactly one", pair, pairs[pair])
				}
			}
		}
	}
	return nil
}

// validatePushData requires a complete candidate inventory: every redaction
// state a session can carry, plus one session the branch-aware selection
// withheld, so the captured screens show every row form the wizard renders.
func validatePushData(fixture pushFixture) error {
	if len(fixture.Sessions) != requiredPushSessionCount {
		return fmt.Errorf("screenshot fixture push sessions: actual=%d required=%d",
			len(fixture.Sessions), requiredPushSessionCount)
	}
	ids := make(map[string]bool, len(fixture.Sessions))
	states := make(map[pushRedactionState]bool, len(fixture.Sessions))
	withheld := 0
	for _, session := range fixture.Sessions {
		if strings.TrimSpace(session.SessionID) == "" || strings.TrimSpace(session.Harness) == "" ||
			strings.TrimSpace(session.Project) == "" || session.StartMs <= 0 ||
			!session.Redaction.valid() || ids[session.SessionID] {
			return fmt.Errorf("screenshot fixture has an incomplete or duplicate push session: %#v", session)
		}
		ids[session.SessionID] = true
		states[session.Redaction] = true
		if session.Withheld {
			withheld++
		}
	}
	if len(states) != 3 {
		return fmt.Errorf("screenshot fixture push sessions cover %d redaction states, want all 3", len(states))
	}
	if withheld != 1 {
		return fmt.Errorf("screenshot fixture push sessions hold %d withheld rows, want exactly 1", withheld)
	}
	return nil
}

func validateDeclaredCounts(document captureDocument) error {
	if document.ExpectedSelectionHarnessCount != requiredSelectionHarnessCount {
		return fmt.Errorf("screenshot fixture selection harnesses: declared=%d required=%d",
			document.ExpectedSelectionHarnessCount, requiredSelectionHarnessCount)
	}
	checks := []struct {
		name     string
		declared int
		actual   int
		required int
	}{
		{name: "sheets", declared: document.ExpectedSheetCount, actual: len(document.Sheets), required: requiredSheetCount},
		{name: "guided sections", declared: document.ExpectedGuidedSectionCount, actual: len(document.GuidedSections), required: requiredGuidedSectionCount},
		{name: "guided captures", declared: document.ExpectedGuidedCaptureCount, actual: len(document.GuidedCaptures), required: requiredGuidedCaptureCount},
		{name: "selection states", declared: document.ExpectedSelectionStateCount, actual: len(document.SelectionStates), required: requiredSelectionStateCount},
		{name: "selection captures", declared: document.ExpectedSelectionCaptureCount, actual: len(document.SelectionCaptures), required: requiredSelectionCaptureCount},
		{name: "selection sessions", declared: document.ExpectedSelectionSessionCount, actual: len(document.Selection.Listings), required: requiredSelectionSessionCount},
		{name: "selection ingested sessions", declared: document.ExpectedSelectionIngestedCount, actual: len(document.Selection.Ingested), required: requiredSelectionIngestedCount},
		{name: "push states", declared: document.ExpectedPushStateCount, actual: len(document.PushStates), required: requiredPushStateCount},
		{name: "push captures", declared: document.ExpectedPushCaptureCount, actual: len(document.PushCaptures), required: requiredPushCaptureCount},
		{name: "push sessions", declared: document.ExpectedPushSessionCount, actual: len(document.Push.Sessions), required: requiredPushSessionCount},
	}
	for _, check := range checks {
		if check.declared != check.required || check.actual != check.required {
			return fmt.Errorf("screenshot fixture %s: declared=%d actual=%d required=%d",
				check.name, check.declared, check.actual, check.required)
		}
	}
	return nil
}

func validateSheets(sheets []sheetFixture) error {
	required := map[sheetName]struct {
		kind          sheetKind
		theme         captureTheme
		width, height int
	}{
		sheetGuidedDark:  {kind: sheetKindGuided, theme: captureThemeDark, width: 1800, height: 3300},
		sheetGuidedLight: {kind: sheetKindGuided, theme: captureThemeLight, width: 1800, height: 3300},
		sheetSelection:   {kind: sheetKindSelection, theme: captureThemeDark, width: 1800, height: 5700},
		sheetPush:        {kind: sheetKindPush, theme: captureThemeDark, width: 1800, height: 4800},
	}
	seen := make(map[sheetName]bool, len(sheets))
	for _, sheet := range sheets {
		want, ok := required[sheet.Name]
		if !ok || seen[sheet.Name] || strings.TrimSpace(sheet.Title) == "" ||
			sheet.Kind != want.kind || sheet.Theme != want.theme ||
			sheet.Viewport.Width != want.width || sheet.Viewport.Height != want.height {
			return fmt.Errorf("screenshot fixture has an unknown, duplicate, or invalid sheet: %#v", sheet)
		}
		seen[sheet.Name] = true
	}
	for name := range required {
		if !seen[name] {
			return fmt.Errorf("screenshot fixture omits required sheet %q", name)
		}
	}
	return nil
}

func validateGuidedMatrix(sections []guidedSectionFixture, captures []guidedCaptureFixture) error {
	sectionRows := make(map[guidedSection]guidedSectionFixture, len(sections))
	for _, section := range sections {
		if !section.Key.valid() || sectionRows[section.Key].Key != "" || !nonEmptyStrings(section.WantContains) {
			return fmt.Errorf("screenshot fixture has an invalid or duplicate guided section: %#v", section)
		}
		sectionRows[section.Key] = section
	}
	for _, key := range []guidedSection{
		guidedSectionAutoIngest, guidedSectionPrivacy, guidedSectionLicense,
		guidedSectionDestination, guidedSectionRetention,
	} {
		if sectionRows[key].Key == "" {
			return fmt.Errorf("screenshot fixture omits guided section %q", key)
		}
	}

	seenNames := make(map[string]bool, len(captures))
	pairs := make(map[string]int, len(captures))
	for _, capture := range captures {
		wantName := fmt.Sprintf("%s-%s-%dx%d", capture.Section, capture.Theme, capture.Width, capture.Height)
		if capture.Name != wantName || seenNames[capture.Name] || sectionRows[capture.Section].Key == "" ||
			!capture.Theme.valid() || !validCaptureSize(capture.Width, capture.Height) {
			return fmt.Errorf("screenshot fixture has an invalid or duplicate guided capture: %#v", capture)
		}
		seenNames[capture.Name] = true
		pairs[guidedPair(capture.Section, capture.Theme, capture.Width, capture.Height)]++
	}
	for section := range sectionRows {
		for _, captureTheme := range []captureTheme{captureThemeDark, captureThemeLight} {
			for _, viewport := range []viewportFixture{{Width: 80, Height: 24}, {Width: 120, Height: 40}} {
				pair := guidedPair(section, captureTheme, viewport.Width, viewport.Height)
				if pairs[pair] != 1 {
					return fmt.Errorf("screenshot fixture guided pair %q has %d captures, want exactly one", pair, pairs[pair])
				}
			}
		}
	}
	return nil
}

func validateSelectionMatrix(states []selectionStateFixture, captures []selectionCaptureFixture) error {
	stateRows := make(map[selectionState]selectionStateFixture, len(states))
	for _, state := range states {
		if !state.Key.valid() || stateRows[state.Key].Key != "" || !nonEmptyStrings(state.WantContains) ||
			(state.Key == selectionStateDefault && state.Query != "") ||
			(state.Key == selectionStateSearch && strings.TrimSpace(state.Query) == "") {
			return fmt.Errorf("screenshot fixture has an invalid or duplicate selection state: %#v", state)
		}
		stateRows[state.Key] = state
	}
	for _, state := range []selectionState{
		selectionStateDefault, selectionStateSearch, selectionStateProjectPreview,
		selectionStateBranchPreview, selectionStateSessionPreview, selectionStateSourcePreview,
	} {
		if stateRows[state].Key == "" {
			return fmt.Errorf("screenshot fixture omits selection state %q", state)
		}
	}

	seenNames := make(map[string]bool, len(captures))
	pairs := make(map[string]int, len(captures))
	for _, capture := range captures {
		wantName := fmt.Sprintf("selection-%s-%s-%dx%d", capture.State, capture.Theme, capture.Width, capture.Height)
		if capture.Name != wantName || seenNames[capture.Name] || stateRows[capture.State].Key == "" ||
			!capture.Theme.valid() || (!capture.State.requiresBothThemes() && capture.Theme != captureThemeDark) ||
			!validCaptureSize(capture.Width, capture.Height) {
			return fmt.Errorf("screenshot fixture has an invalid or duplicate selection capture: %#v", capture)
		}
		seenNames[capture.Name] = true
		pair := fmt.Sprintf("%s/%s/%dx%d", capture.State, capture.Theme, capture.Width, capture.Height)
		pairs[pair]++
	}
	for state := range stateRows {
		themes := []captureTheme{captureThemeDark}
		if state.requiresBothThemes() {
			themes = append(themes, captureThemeLight)
		}
		for _, captureTheme := range themes {
			for _, viewport := range []viewportFixture{{Width: 80, Height: 24}, {Width: 120, Height: 40}} {
				pair := fmt.Sprintf("%s/%s/%dx%d", state, captureTheme, viewport.Width, viewport.Height)
				if pairs[pair] != 1 {
					return fmt.Errorf("screenshot fixture selection pair %q has %d captures, want exactly one", pair, pairs[pair])
				}
			}
		}
	}
	return nil
}

func validateSelectionData(selection selectionFixture) error {
	if len(selection.Repositories) == 0 {
		return fmt.Errorf("screenshot fixture selection repositories are empty")
	}
	if len(selection.Transcripts) == 0 {
		return fmt.Errorf("screenshot fixture selection transcripts are empty")
	}
	repositories := make(map[string]selectionRepositoryFixture, len(selection.Repositories))
	for _, repository := range selection.Repositories {
		if strings.TrimSpace(repository.ClonePath) == "" || strings.TrimSpace(repository.CohortKey) == "" || strings.TrimSpace(repository.GitDirectory) == "" || repositories[repository.ClonePath].ClonePath != "" {
			return fmt.Errorf("screenshot fixture has an incomplete or duplicate selection repository: %#v", repository)
		}
		repositories[repository.ClonePath] = repository
	}
	sessionIDs := make(map[string]bool, len(selection.Listings))
	harnesses := make(map[string]bool)
	for _, listing := range selection.Listings {
		if strings.TrimSpace(listing.SessionID) == "" || strings.TrimSpace(listing.Harness) == "" ||
			strings.TrimSpace(listing.Title) == "" || sessionIDs[listing.SessionID] || repositories[listing.WorkingDir].ClonePath == "" {
			return fmt.Errorf("screenshot fixture has an incomplete or duplicate selection session: %#v", listing)
		}
		sessionIDs[listing.SessionID] = true
		harnesses[listing.Harness] = true
	}
	for sessionID, turns := range selection.Transcripts {
		if !sessionIDs[sessionID] || len(turns) == 0 {
			return fmt.Errorf("screenshot fixture transcript %q is unknown or empty", sessionID)
		}
		for index, turn := range turns {
			if !turn.Role.IsValid() || !turn.EntryType.IsValid() || strings.TrimSpace(turn.Content) == "" {
				return fmt.Errorf("screenshot fixture transcript %q turn %d is invalid or empty", sessionID, index)
			}
		}
	}
	if len(harnesses) != requiredSelectionHarnessCount {
		return fmt.Errorf("screenshot fixture selection harnesses: actual=%d required=%d",
			len(harnesses), requiredSelectionHarnessCount)
	}
	for _, listing := range selection.Listings {
		for _, childID := range listing.SubagentIDs {
			if !sessionIDs[childID] {
				return fmt.Errorf("screenshot fixture session %q references unknown child %q", listing.SessionID, childID)
			}
		}
	}
	if len(selection.SourceTranscripts) == 0 {
		return fmt.Errorf("screenshot fixture records no harness transcript; the not-yet-imported preview would show nothing")
	}
	for sessionID, lines := range selection.SourceTranscripts {
		if !sessionIDs[sessionID] || len(lines) == 0 {
			return fmt.Errorf("screenshot fixture harness transcript %q is unknown or empty", sessionID)
		}
		if selection.Transcripts[sessionID] != nil {
			return fmt.Errorf("screenshot fixture session %q has both a stored and a harness transcript; the capture could not show which one the pane read", sessionID)
		}
		for index, line := range lines {
			if strings.TrimSpace(line) == "" {
				return fmt.Errorf("screenshot fixture harness transcript %q line %d is empty", sessionID, index)
			}
		}
		var listed ftue.SessionListing
		for _, listing := range selection.Listings {
			if listing.SessionID == sessionID {
				listed = listing
			}
		}
		for _, ingestedID := range selection.Ingested {
			if ingestedID == sessionID {
				return fmt.Errorf("screenshot fixture session %q is imported, so its capture would not show the not-yet-imported preview", sessionID)
			}
		}
		if listed.Source.Origin != ftue.SessionSourceOriginFile {
			return fmt.Errorf("screenshot fixture session %q carries a harness transcript but declares origin %q", sessionID, listed.Source.Origin)
		}
	}
	seenIngested := make(map[string]bool, len(selection.Ingested))
	for _, sessionID := range selection.Ingested {
		if !sessionIDs[sessionID] || seenIngested[sessionID] {
			return fmt.Errorf("screenshot fixture ingested session %q is unknown or duplicated", sessionID)
		}
		seenIngested[sessionID] = true
	}
	return nil
}

func validCaptureSize(width, height int) bool {
	return (width == 80 && height == 24) || (width == 120 && height == 40)
}

func guidedPair(section guidedSection, captureTheme captureTheme, width, height int) string {
	return fmt.Sprintf("%s/%s/%dx%d", section, captureTheme, width, height)
}

func nonEmptyStrings(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}
