//go:build guided_screenshots

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"gopkg.in/yaml.v3"
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
	// selectionStateOriginHidden is the mounted list with an agent-driven root
	// hidden, its user-origin control visible, and a visible parent's child
	// badge reading correctly.
	selectionStateOriginHidden selectionState = "origin-hidden"
)

func (s selectionState) valid() bool {
	switch s {
	case selectionStateDefault, selectionStateSearch, selectionStateProjectPreview,
		selectionStateBranchPreview, selectionStateSessionPreview, selectionStateSourcePreview,
		selectionStateOriginHidden:
		return true
	default:
		return false
	}
}

func (s selectionState) requiresBothThemes() bool {
	return s == selectionStateProjectPreview || s == selectionStateBranchPreview ||
		s == selectionStateSessionPreview || s == selectionStateSourcePreview ||
		s == selectionStateOriginHidden
}

// pushState is the closed set of push-wizard screens the harness captures: the
// consent prompt that opens the wizard, the selection tree over a project row,
// the same tree over a session row (where the pane draws that session's
// transcript as the push will publish it), the page that states what leaves the
// machine, and the receipt.
type pushState string

const (
	pushStateStart     pushState = "start"
	pushStateSelection pushState = "selection"
	// pushStateSessionPreview is the selection page with a SESSION highlighted,
	// which is the only state that draws the published transcript.
	pushStateSessionPreview pushState = "session-preview"
	pushStateConsent        pushState = "consent"
	pushStateReceipt        pushState = "receipt"
)

func (s pushState) valid() bool {
	switch s {
	case pushStateStart, pushStateSelection, pushStateSessionPreview, pushStateConsent, pushStateReceipt:
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
	// Transcripts holds the RECORDED entries of each session, keyed by session
	// id. The capture feeds them through the real push redactor, so the preview
	// pane in the sheet shows what a publish would send.
	Transcripts map[string][]selectionTurnFixture `yaml:"transcripts"`
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
	// WantAbsent names markers that must NOT appear in the rendered view. It
	// is optional; only the origin-hiding state uses it today, to prove a
	// row is actually gone rather than merely not asserted present.
	WantAbsent []string `yaml:"wantAbsent"`
	// WideWantContains and NarrowWantAbsent carry the markers whose presence
	// depends on the terminal width, so a width-specific truth is a GATE
	// rather than a note asking a human to look. The child-session badge is
	// the case that exists today: the wider capture must show it, and the
	// narrower one drops it entirely rather than truncating it.
	WideWantContains []string `yaml:"wideWantContains"`
	NarrowWantAbsent []string `yaml:"narrowWantAbsent"`
}

// selectionWideColumns is the width at and above which a selection capture is
// the WIDE one: it has room for the markers the narrow capture drops.
const selectionWideColumns = 120

// selectionStateExpectations resolves one state's markers for one capture
// width, so the real capture run and the mounted-render test cannot disagree
// about what a width is expected to show.
func selectionStateExpectations(state selectionStateFixture, width int) (wantContains, wantAbsent []string) {
	wantContains = append([]string(nil), state.WantContains...)
	wantAbsent = append([]string(nil), state.WantAbsent...)
	if width >= selectionWideColumns {
		wantContains = append(wantContains, state.WideWantContains...)
		return wantContains, wantAbsent
	}
	return wantContains, append(wantAbsent, state.NarrowWantAbsent...)
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
	Repositories []selectionRepositoryFixture `yaml:"repositories"`
	// Listings is the ROOTS-ONLY cohort the picker lists, exactly what
	// production discovery hands it. A subagent session is never here.
	Listings []ftue.SessionListing `yaml:"listings"`
	// SubagentDiscovery is the parent-to-subagent relation over the whole
	// discovered set, the second thing production discovery returns. It
	// resolves a parent row's child count and nothing else: a session named
	// only here is discovered, counted, and never listed.
	SubagentDiscovery []selectionSubagentFixture        `yaml:"subagentDiscovery"`
	Transcripts       map[string][]selectionTurnFixture `yaml:"transcripts"`
	// SourceTranscripts holds the harness transcript lines for sessions the
	// local store does not hold. The renderer writes each one to its isolated
	// workspace and points the listing at it, so the preview reads a real file
	// through the production reader.
	SourceTranscripts map[string][]string `yaml:"sourceTranscripts"`
	Ingested          []string            `yaml:"ingested"`
	// RequiredSessionNames, RequiredHarnessNames, and RequiredIngestedNames
	// are deletion-protection manifests: every listed name must be present
	// among Listings' session ids, Listings' harnesses, and Ingested
	// respectively. None bounds how many rows or distinct values exist.
	RequiredSessionNames  []string `yaml:"requiredSessionNames"`
	RequiredHarnessNames  []string `yaml:"requiredHarnessNames"`
	RequiredIngestedNames []string `yaml:"requiredIngestedNames"`
	// RequiredSubagentDiscoveryNames protects the sessions that exist ONLY in
	// SubagentDiscovery. They are what makes the capture production-shaped, so
	// deleting one must fail the fixture rather than quietly shrink a count.
	RequiredSubagentDiscoveryNames []string `yaml:"requiredSubagentDiscoveryNames"`
}

// selectionSubagentFixture is one entry of the discovered subagent relation:
// a session discovery surfaced, and the subagent sessions it spawned.
type selectionSubagentFixture struct {
	SessionID   string   `yaml:"sessionId"`
	SubagentIDs []string `yaml:"subagentIds"`
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
	// RequiredPushSessionNames is a deletion-protection manifest: every
	// listed session id must be present in Push.Sessions. It does not bound
	// how many push sessions exist.
	RequiredPushSessionNames []string                  `yaml:"requiredPushSessionNames"`
	PushStates               []pushStateFixture        `yaml:"pushStates"`
	PushCaptures             []pushCaptureFixture      `yaml:"pushCaptures"`
	Push                     pushFixture               `yaml:"push"`
	Sheets                   []sheetFixture            `yaml:"sheets"`
	GuidedSections           []guidedSectionFixture    `yaml:"guidedSections"`
	SelectionStates          []selectionStateFixture   `yaml:"selectionStates"`
	Selection                selectionFixture          `yaml:"selection"`
	GuidedCaptures           []guidedCaptureFixture    `yaml:"guidedCaptures"`
	SelectionCaptures        []selectionCaptureFixture `yaml:"selectionCaptures"`
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
	if err := validatePushData(document.Push, document.RequiredPushSessionNames); err != nil {
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
	for _, key := range []pushState{
		pushStateStart, pushStateSelection, pushStateSessionPreview, pushStateConsent, pushStateReceipt,
	} {
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
func validatePushData(fixture pushFixture, requiredSessionNames []string) error {
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
	if err := requireNames("push session", requiredSessionNames, ids); err != nil {
		return err
	}
	if len(states) != 3 {
		return fmt.Errorf("screenshot fixture push sessions cover %d redaction states, want all 3", len(states))
	}
	if withheld != 1 {
		return fmt.Errorf("screenshot fixture push sessions hold %d withheld rows, want exactly 1", withheld)
	}
	// The preview capture is only evidence if a session actually has a stored
	// transcript to draw. Without this the sheet would show the empty-transcript
	// note in every theme and read as a passing capture of the wrong screen.
	previewable := 0
	for sessionID, entries := range fixture.Transcripts {
		if !ids[sessionID] {
			return fmt.Errorf("screenshot fixture holds a transcript for unknown push session %q", sessionID)
		}
		for _, entry := range entries {
			if strings.TrimSpace(string(entry.Role)) == "" || strings.TrimSpace(string(entry.EntryType)) == "" ||
				strings.TrimSpace(entry.Content) == "" {
				return fmt.Errorf("screenshot fixture push transcript %q holds an incomplete entry: %#v", sessionID, entry)
			}
		}
		if len(entries) > 0 {
			previewable++
		}
	}
	if previewable == 0 {
		return fmt.Errorf("screenshot fixture holds no push transcript, so the preview capture would show no transcript")
	}
	return nil
}

// requireNames asserts every name in required is present in present, and
// that required itself declares no blank or duplicate name. kind identifies
// the axis (e.g. "selection session", "push session") in failure messages.
// It is a deletion-protection manifest, not a row count: it never bounds how
// many rows exist, only that the named ones remain. It delegates to the one
// shared checker in internal/testutil so every fixture family in this repo
// reports the same shape of error for the same mistake.
func requireNames(kind string, required []string, present map[string]bool) error {
	return testutil.RequireFixtureNames("screenshot fixture", kind, required, present)
}

func validateSheets(sheets []sheetFixture) error {
	required := map[sheetName]struct {
		kind          sheetKind
		theme         captureTheme
		width, height int
	}{
		sheetGuidedDark:  {kind: sheetKindGuided, theme: captureThemeDark, width: 1800, height: 3420},
		sheetGuidedLight: {kind: sheetKindGuided, theme: captureThemeLight, width: 1800, height: 3420},
		sheetSelection:   {kind: sheetKindSelection, theme: captureThemeDark, width: 1800, height: 6750},
		sheetPush:        {kind: sheetKindPush, theme: captureThemeDark, width: 1800, height: 6000},
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
		guidedSectionAutoIngest, guidedSectionPublication, guidedSectionPrivacy, guidedSectionLicense,
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
			(len(state.WantAbsent) > 0 && !nonEmptyStrings(state.WantAbsent)) ||
			(state.Key == selectionStateDefault && state.Query != "") ||
			(state.Key == selectionStateSearch && strings.TrimSpace(state.Query) == "") {
			return fmt.Errorf("screenshot fixture has an invalid or duplicate selection state: %#v", state)
		}
		stateRows[state.Key] = state
	}
	for _, state := range []selectionState{
		selectionStateDefault, selectionStateSearch, selectionStateProjectPreview,
		selectionStateBranchPreview, selectionStateSessionPreview, selectionStateSourcePreview,
		selectionStateOriginHidden,
	} {
		if stateRows[state].Key == "" {
			return fmt.Errorf("screenshot fixture omits selection state %q", state)
		}
	}
	if len(stateRows[selectionStateOriginHidden].WantAbsent) == 0 {
		return fmt.Errorf("screenshot fixture selection state %q declares no wantAbsent marker, so a broken origin filter would pass unnoticed", selectionStateOriginHidden)
	}
	if len(stateRows[selectionStateOriginHidden].WideWantContains) == 0 {
		return fmt.Errorf(
			"screenshot fixture selection state %q declares no wideWantContains marker.\n"+
				"what: the wider capture asserts nothing that only the wider capture can show.\n"+
				"why: the child-session badge fits at %d columns and is dropped below it, so without\n"+
				"     a width-specific marker a badge that stopped rendering would pass unnoticed.\n"+
				"where: the origin-hidden selection state in testdata/captures.yaml.\n"+
				"when: while validating the fixture, before any capture was rendered.\n"+
				"means: no screenshot was written.\n"+
				"fix: restore the badge marker under wideWantContains.",
			selectionStateOriginHidden, selectionWideColumns)
	}
	if len(stateRows[selectionStateOriginHidden].NarrowWantAbsent) == 0 {
		return fmt.Errorf(
			"screenshot fixture selection state %q declares no narrowWantAbsent marker.\n"+
				"what: nothing pins that the narrower capture DROPS the child-session badge.\n"+
				"why: the drop is deliberate width behaviour; unpinned, a badge leaking into a\n"+
				"     capture too narrow to hold it would go unnoticed.\n"+
				"where: the origin-hidden selection state in testdata/captures.yaml.\n"+
				"when: while validating the fixture, before any capture was rendered.\n"+
				"means: no screenshot was written.\n"+
				"fix: restore the badge marker under narrowWantAbsent.",
			selectionStateOriginHidden)
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
	if err := requireNames("selection session", selection.RequiredSessionNames, sessionIDs); err != nil {
		return err
	}
	if err := requireNames("selection harness", selection.RequiredHarnessNames, harnesses); err != nil {
		return err
	}
	// The discovered set is the listed cohort PLUS the sessions that reach the
	// picker only through the relation. Every child reference must resolve
	// inside it, exactly as the production count guard requires.
	discovered := make(map[string]bool, len(sessionIDs)+len(selection.SubagentDiscovery))
	for sessionID := range sessionIDs {
		discovered[sessionID] = true
	}
	relationNames := make(map[string]bool, len(selection.SubagentDiscovery))
	for _, entry := range selection.SubagentDiscovery {
		if strings.TrimSpace(entry.SessionID) == "" || relationNames[entry.SessionID] {
			return fmt.Errorf("screenshot fixture has an empty or duplicate discovered subagent entry: %#v", entry)
		}
		relationNames[entry.SessionID] = true
		discovered[entry.SessionID] = true
	}
	// Production records the relation for EVERY discovered session, so a
	// fixture that covers only some of them is not the production shape.
	for _, listing := range selection.Listings {
		if !relationNames[listing.SessionID] {
			return fmt.Errorf(
				"screenshot fixture lists session %q but the discovered subagent relation omits it.\n"+
					"what: the capture fixture is not in the shape production discovery produces.\n"+
					"why: production records a relation entry for every session it discovers, listed or not.\n"+
					"where: the selection fixture in testdata/captures.yaml.\n"+
					"when: while validating the fixture, before any capture was rendered.\n"+
					"means: no screenshot was written.\n"+
					"fix: add a subagentDiscovery entry for %q, with no subagentIds when it spawned none.",
				listing.SessionID, listing.SessionID)
		}
	}
	for _, listing := range selection.Listings {
		for _, childID := range listing.SubagentIDs {
			if !discovered[childID] {
				return fmt.Errorf("screenshot fixture session %q references unknown child %q", listing.SessionID, childID)
			}
		}
	}
	for _, entry := range selection.SubagentDiscovery {
		for _, childID := range entry.SubagentIDs {
			if !discovered[childID] {
				return fmt.Errorf("screenshot fixture discovered session %q references unknown child %q", entry.SessionID, childID)
			}
		}
		// A discovered session that is neither listed nor anybody's subagent
		// would be a root production WOULD have listed, so the fixture would
		// no longer describe a reachable state.
		if sessionIDs[entry.SessionID] {
			continue
		}
		claimed := false
		for _, other := range selection.SubagentDiscovery {
			for _, childID := range other.SubagentIDs {
				if childID == entry.SessionID {
					claimed = true
				}
			}
		}
		if !claimed {
			return fmt.Errorf(
				"screenshot fixture discovered session %q is neither listed nor named as anybody's subagent.\n"+
					"what: the fixture describes a session production discovery could not have hidden.\n"+
					"why: a discovered session with no parent is a ROOT, and roots are listed.\n"+
					"where: subagentDiscovery in testdata/captures.yaml.\n"+
					"when: while validating the fixture, before any capture was rendered.\n"+
					"means: no screenshot was written.\n"+
					"fix: list %q among the selection listings, or name it as a subagent of a discovered session.",
				entry.SessionID, entry.SessionID)
		}
	}
	if err := requireNames("selection discovered subagent", selection.RequiredSubagentDiscoveryNames, relationNames); err != nil {
		return err
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
	if err := requireNames("selection ingested session", selection.RequiredIngestedNames, seenIngested); err != nil {
		return err
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
