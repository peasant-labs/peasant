package kickstart_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// ansiPattern matches the escape sequences a rendered screen carries, so a text
// assertion reads the VISIBLE characters rather than the styling around them.
var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// stripRender removes styling from a rendered screen.
func stripRender(s string) string { return ansiPattern.ReplaceAllString(s, "") }

//go:embed testdata/selection_render.yaml
var selectionRenderData []byte

const (
	expectedSelectionRenderCaseCount             = 23
	expectedSelectionRenderSessionCount          = 5
	expectedSelectionRenderHarnessCount          = 2
	expectedSelectionRenderPreviewAssertionCount = 3
	expectedSelectionRenderTextAssertionCount    = 11
	expectedSelectionRenderBothThemeStateCount   = 6
)

type selectionRenderState string

const (
	selectionRenderDefault         selectionRenderState = "default"
	selectionRenderNarrowed        selectionRenderState = "narrowed"
	selectionRenderGutterHidden    selectionRenderState = "gutter-hidden"
	selectionRenderSessionCursor   selectionRenderState = "session-cursor"
	selectionRenderPreviewFocused  selectionRenderState = "preview-focused"
	selectionRenderPreviewScrolled selectionRenderState = "preview-scrolled"
	selectionRenderSearchEditing   selectionRenderState = "search-editing"
	selectionRenderKeptFilter      selectionRenderState = "kept-filter"
	selectionRenderBranchScope     selectionRenderState = "branch-scope"
	selectionRenderSessionScope    selectionRenderState = "session-scope"
	selectionRenderOverflowMiddle  selectionRenderState = "overflow-middle"
	selectionRenderOverflowBottom  selectionRenderState = "overflow-bottom"
)

func (s selectionRenderState) valid() bool {
	switch s {
	case selectionRenderDefault, selectionRenderNarrowed, selectionRenderGutterHidden,
		selectionRenderSessionCursor, selectionRenderPreviewFocused, selectionRenderPreviewScrolled,
		selectionRenderSearchEditing, selectionRenderKeptFilter, selectionRenderBranchScope,
		selectionRenderSessionScope, selectionRenderOverflowMiddle, selectionRenderOverflowBottom:
		return true
	default:
		return false
	}
}

func (s selectionRenderState) requiresBothThemes() bool {
	switch s {
	case selectionRenderSearchEditing, selectionRenderKeptFilter, selectionRenderBranchScope,
		selectionRenderSessionScope, selectionRenderOverflowMiddle, selectionRenderOverflowBottom:
		return true
	default:
		return false
	}
}

type selectionRenderTheme string

const (
	selectionRenderDark  selectionRenderTheme = "dark"
	selectionRenderLight selectionRenderTheme = "light"
)

func (t selectionRenderTheme) valid() bool {
	return t == selectionRenderDark || t == selectionRenderLight
}

// selectionRenderCase is one captured screen: the state the step is driven
// into, the palette, and the region it renders at.
type selectionRenderCase struct {
	Name   string               `yaml:"name"`
	State  selectionRenderState `yaml:"state"`
	Theme  selectionRenderTheme `yaml:"theme"`
	Width  int                  `yaml:"width"`
	Height int                  `yaml:"height"`
}

type selectionRenderAssertionRow struct {
	Case         string   `yaml:"case"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

type selectionRenderAssertions struct {
	ExpectedRowCount int                           `yaml:"expectedRowCount"`
	Rows             []selectionRenderAssertionRow `yaml:"rows"`
}

// previewColoredRun is one span of the preview body that must carry a NAMED
// palette token's color - the evidence that the pane rendered highlighted
// markdown rather than plain characters.
type previewColoredRun struct {
	Text  string `yaml:"text"`
	Token string `yaml:"token"`
}

// previewAssertionRow is what one captured state's preview pane must show.
type previewAssertionRow struct {
	Case            string              `yaml:"case"`
	WantVisible     []string            `yaml:"wantVisible"`
	WantMissing     []string            `yaml:"wantMissing"`
	WantColored     []previewColoredRun `yaml:"wantColored"`
	WantFocusMarker string              `yaml:"wantFocusMarker"`
}

// previewAssertions is the preview-pane expectation block plus its row-count
// guard.
type previewAssertions struct {
	ExpectedRowCount int                   `yaml:"expectedRowCount"`
	Rows             []previewAssertionRow `yaml:"rows"`
}

// selectionRenderDoc is the whole fixture: the discovery listing the step folds,
// which sessions the store holds, and the cases - plus the row-count guards.
type selectionRenderDoc struct {
	ExpectedCaseCount           int                       `yaml:"expectedCaseCount"`
	ExpectedSessionCount        int                       `yaml:"expectedSessionCount"`
	ExpectedHarnessCount        int                       `yaml:"expectedHarnessCount"`
	ExpectedBothThemeStateCount int                       `yaml:"expectedBothThemeStateCount"`
	BothThemeStates             []selectionRenderState    `yaml:"bothThemeStates"`
	RenderAssertions            selectionRenderAssertions `yaml:"renderAssertions"`
	PreviewAssertions           previewAssertions         `yaml:"previewAssertions"`
	Stored                      map[string]string         `yaml:"stored"`
	Listings                    []ftue.SessionListing     `yaml:"listings"`
	Ingested                    []string                  `yaml:"ingested"`
	Cases                       []selectionRenderCase     `yaml:"cases"`
}

func selectionRenderValuesPresent(values ...[]string) bool {
	for _, group := range values {
		for _, value := range group {
			if strings.TrimSpace(value) == "" {
				return false
			}
		}
	}
	return true
}

func decodeSelectionRender(data []byte) (selectionRenderDoc, error) {
	var doc selectionRenderDoc
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/selection_render.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("selection_render.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != expectedSelectionRenderCaseCount || len(doc.Cases) != expectedSelectionRenderCaseCount {
		return doc, fmt.Errorf("selection render cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedSelectionRenderCaseCount)
	}
	if doc.ExpectedSessionCount != expectedSelectionRenderSessionCount || len(doc.Listings) != expectedSelectionRenderSessionCount {
		return doc, fmt.Errorf("selection render sessions: declared=%d actual=%d required=%d",
			doc.ExpectedSessionCount, len(doc.Listings), expectedSelectionRenderSessionCount)
	}
	harnesses := map[string]bool{}
	sessionIDs := map[string]bool{}
	for _, sess := range doc.Listings {
		if sess.SessionID == "" || sess.Harness == "" || sessionIDs[sess.SessionID] {
			return doc, fmt.Errorf("selection render fixture contains an invalid or duplicate session %q", sess.SessionID)
		}
		sessionIDs[sess.SessionID] = true
		harnesses[sess.Harness] = true
	}
	pa := doc.PreviewAssertions
	if pa.ExpectedRowCount != expectedSelectionRenderPreviewAssertionCount || len(pa.Rows) != expectedSelectionRenderPreviewAssertionCount {
		return doc, fmt.Errorf("selection render preview assertions: declared=%d actual=%d required=%d",
			pa.ExpectedRowCount, len(pa.Rows), expectedSelectionRenderPreviewAssertionCount)
	}
	ra := doc.RenderAssertions
	if ra.ExpectedRowCount != expectedSelectionRenderTextAssertionCount || len(ra.Rows) != expectedSelectionRenderTextAssertionCount {
		return doc, fmt.Errorf("selection render text assertions: declared=%d actual=%d required=%d",
			ra.ExpectedRowCount, len(ra.Rows), expectedSelectionRenderTextAssertionCount)
	}
	if doc.ExpectedHarnessCount != expectedSelectionRenderHarnessCount || len(harnesses) != expectedSelectionRenderHarnessCount {
		return doc, fmt.Errorf("selection render harnesses: declared=%d actual=%d required=%d",
			doc.ExpectedHarnessCount, len(harnesses), expectedSelectionRenderHarnessCount)
	}
	caseNames := map[string]bool{}
	casesByState := map[selectionRenderState]map[selectionRenderTheme][]selectionRenderCase{}
	for _, c := range doc.Cases {
		if c.Name == "" || caseNames[c.Name] || !c.State.valid() || !c.Theme.valid() {
			return doc, fmt.Errorf("selection render fixture case %q is empty, duplicated, or has an invalid state/theme", c.Name)
		}
		caseNames[c.Name] = true
		if casesByState[c.State] == nil {
			casesByState[c.State] = map[selectionRenderTheme][]selectionRenderCase{}
		}
		casesByState[c.State][c.Theme] = append(casesByState[c.State][c.Theme], c)
		if c.Width <= 0 || c.Height <= 0 {
			return doc, fmt.Errorf("selection render fixture case %q declares a %dx%d region; a non-positive size captures nothing",
				c.Name, c.Width, c.Height)
		}
	}
	if doc.ExpectedBothThemeStateCount != expectedSelectionRenderBothThemeStateCount || len(doc.BothThemeStates) != expectedSelectionRenderBothThemeStateCount {
		return doc, fmt.Errorf("selection render both-theme states: declared=%d actual=%d required=%d",
			doc.ExpectedBothThemeStateCount, len(doc.BothThemeStates), expectedSelectionRenderBothThemeStateCount)
	}
	pairedStates := map[selectionRenderState]bool{}
	for _, state := range doc.BothThemeStates {
		if !state.requiresBothThemes() || pairedStates[state] {
			return doc, fmt.Errorf("selection render both-theme state %q is not required or is duplicated", state)
		}
		pairedStates[state] = true
		dark := casesByState[state][selectionRenderDark]
		light := casesByState[state][selectionRenderLight]
		if len(dark) != 1 || len(light) != 1 || dark[0].Width != light[0].Width || dark[0].Height != light[0].Height {
			return doc, fmt.Errorf("selection render state %q must have one dark and one light case at the same size", state)
		}
	}
	for id := range doc.Stored {
		if !sessionIDs[id] {
			return doc, fmt.Errorf("selection render fixture stores preview text for unknown session %q", id)
		}
	}
	ingestedIDs := map[string]bool{}
	for _, id := range doc.Ingested {
		if !sessionIDs[id] || ingestedIDs[id] {
			return doc, fmt.Errorf("selection render fixture ingested session %q is unknown or duplicated", id)
		}
		ingestedIDs[id] = true
	}
	assertionNames := map[string]bool{}
	for _, row := range ra.Rows {
		if row.Case == "" || assertionNames[row.Case] || !caseNames[row.Case] || len(row.WantContains)+len(row.WantMissing) == 0 ||
			!selectionRenderValuesPresent(row.WantContains, row.WantMissing) {
			return doc, fmt.Errorf("selection render text assertion %q is empty, duplicated, assertion-free, or references no case", row.Case)
		}
		assertionNames[row.Case] = true
	}
	previewNames := map[string]bool{}
	for _, row := range pa.Rows {
		if row.Case == "" || strings.TrimSpace(row.WantFocusMarker) == "" || previewNames[row.Case] || !caseNames[row.Case] ||
			len(row.WantVisible)+len(row.WantMissing)+len(row.WantColored) == 0 ||
			!selectionRenderValuesPresent(row.WantVisible, row.WantMissing) {
			return doc, fmt.Errorf("preview assertion row %q is empty, duplicated, assertion-free, or references no case", row.Case)
		}
		previewNames[row.Case] = true
		for _, colored := range row.WantColored {
			if strings.TrimSpace(colored.Text) == "" || strings.TrimSpace(colored.Token) == "" {
				return doc, fmt.Errorf("preview assertion row %q has an empty colored-run field", row.Case)
			}
		}
	}
	return doc, nil
}

func loadSelectionRenderDoc(t *testing.T) selectionRenderDoc {
	t.Helper()
	doc, err := decodeSelectionRender(selectionRenderData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func renderThemeFor(t *testing.T, name selectionRenderTheme) theme.Mode {
	t.Helper()
	switch name {
	case selectionRenderDark:
		return theme.ModeDark
	case selectionRenderLight:
		return theme.ModeLight
	default:
		t.Fatalf("unknown theme %q", name)
		return theme.ModeDark
	}
}

// buildSelectionStep drives the REAL mounted program - the same scanner source,
// registry, facet, and preview the command wires - into one state and returns it
// ready to render.
func buildSelectionStep(t *testing.T, doc selectionRenderDoc, c selectionRenderCase) kickstart.Program {
	t.Helper()
	th := theme.New(renderThemeFor(t, c.Theme))
	preview := kickstart.NewListingPreview(th, doc.Listings, turnsFromPrompts(doc.Stored))

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, config.BaseConfig()); err != nil {
		t.Fatalf("seed base config: %v", err)
	}
	loaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	p := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:   th,
		Draft:   draft,
		Source:  kickstart.NewScannerTreeSource(doc.Listings, kickstart.WithIngestedSessionIDs(doc.Ingested)),
		Preview: preview,
	})
	p.SetSize(c.Width, c.Height)
	p = declineOAuth(t, p)
	p = drainProgram(p, p.Init())

	switch c.State {
	case selectionRenderDefault:
	case selectionRenderNarrowed:
		p = pressAndDrain(p, 'f')
	case selectionRenderGutterHidden:
		// The cycle is every-value, then one state per harness, then hidden.
		for i := 0; i < doc.ExpectedHarnessCount+1; i++ {
			p = pressAndDrain(p, 'f')
		}
	case selectionRenderSessionCursor:
		p = cursorOntoImportedSession(p)
	case selectionRenderPreviewFocused:
		p = drainProgram(cursorOntoImportedSession(p).Update(focusPreviewKey()))
	case selectionRenderPreviewScrolled:
		p = drainProgram(cursorOntoImportedSession(p).Update(focusPreviewKey()))
		// Far enough to carry the pane past the identity lines AND past the
		// prose, so the capture proves the viewport moved rather than that a
		// blank line appeared at the top.
		for i := 0; i < previewScrollRows; i++ {
			p = pressAndDrain(p, 'j')
		}
	case selectionRenderSearchEditing:
		p = pressAndDrain(p, '/')
		p = typeAndDrain(p, 'a')
	case selectionRenderKeptFilter:
		p = moveToSessionScope(p)
		p = pressAndDrain(p, '/')
		for _, r := range "cursor" {
			p = typeAndDrain(p, r)
		}
		p = pressAndDrain(p, tea.KeyEnter)
	case selectionRenderBranchScope:
		p = pressAndDrain(p, ']')
	case selectionRenderSessionScope:
		p = moveToSessionScope(p)
	case selectionRenderOverflowMiddle:
		p = drainProgram(p.Update(tea.KeyPressMsg{Code: tea.KeyPgDown}))
		p = pressAndDrain(p, 'j')
	case selectionRenderOverflowBottom:
		p = drainProgram(p.Update(tea.KeyPressMsg{Code: 'G', Text: "G"}))
	default:
		t.Fatalf("unknown state %q", c.State)
	}
	return p
}

// cursorOntoImportedSession steps the tree cursor onto the already-imported
// session row. Rows are project, branch, then the sessions: the third one,
// which the import-state grouping puts after the not-yet-imported ones.
func cursorOntoImportedSession(p kickstart.Program) kickstart.Program {
	for i := 0; i < 5; i++ {
		p = pressAndDrain(p, 'j')
	}
	return p
}

// previewScrollRows is how far the scrolled capture moves the preview.
const previewScrollRows = 6

// focusPreviewKey is the keystroke that moves input across the divider to the
// preview pane.
func focusPreviewKey() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}
}

// drainProgram runs a command and feeds every message it produced (and their
// follow-ups) back into the program, as the runtime would.
func drainProgram(p kickstart.Program, cmd tea.Cmd) kickstart.Program {
	for _, msg := range collectMsgs(cmd) {
		var follow tea.Cmd
		p, follow = p.Update(msg)
		for _, m := range collectMsgs(follow) {
			p, _ = p.Update(m)
		}
	}
	return p
}

// pressAndDrain sends one key and settles the asynchronous work it started (the
// preview load a cursor move or a facet change issues).
func pressAndDrain(p kickstart.Program, code rune) kickstart.Program {
	next, cmd := p.Update(press(code))
	return drainProgram(next, cmd)
}

func typeAndDrain(p kickstart.Program, value rune) kickstart.Program {
	next, cmd := p.Update(tea.KeyPressMsg{Code: value, Text: string(value)})
	return drainProgram(next, cmd)
}

func moveToSessionScope(p kickstart.Program) kickstart.Program {
	return pressAndDrain(pressAndDrain(p, ']'), ']')
}

// TestSelectionStep_RenderGolden captures the whole rendered step for every
// state, so the child-session counts, the imported/not-yet split, the facet
// gutter, and the preview pane are all visible in the test artifact.
func TestSelectionStep_RenderGolden(t *testing.T) {
	doc := loadSelectionRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			p := buildSelectionStep(t, doc, c)
			golden.RequireEqual(t, []byte(p.View()))
		})
	}
}

// TestSelectionStep_RenderSizeInvariant proves the goldens cannot bake in an
// overflow: every captured screen is exactly the height it was sized to, and no
// line exceeds its width. This is what keeps the gutter, the tree, and the
// preview from silently stealing rows or columns from each other.
func TestSelectionStep_RenderSizeInvariant(t *testing.T) {
	doc := loadSelectionRenderDoc(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			view := buildSelectionStep(t, doc, c).View()
			lines := strings.Split(view, "\n")
			if len(lines) != c.Height {
				t.Errorf("rendered %d lines, want exactly %d", len(lines), c.Height)
			}
			for i, line := range lines {
				if got := lipgloss.Width(line); got > c.Width {
					t.Errorf("line %d is %d cells, over width %d: %q", i, got, c.Width, stripRender(line))
				}
			}
		})
	}
}

// requireEveryContentShapeAsserted fails when a case whose rendered CONTENT can
// differ from every other case has no text assertion of its own, leaving it
// verified by the golden byte-diff alone. Two cases share a content shape only
// when they render the same state at the same size - a second palette re-colours
// the same screen, but a second SIZE can drop or add content (a narrower tree
// pane drops its row annotations), so each size is its own shape.
func requireEveryContentShapeAsserted(t *testing.T, doc selectionRenderDoc) {
	t.Helper()
	// A shape counts as asserted from EITHER source: this test's per-state text
	// expectations, or the fixture's previewAssertions rows, which name what the
	// preview pane must show for the states that exist to capture it.
	named := map[string]bool{}
	for _, row := range doc.RenderAssertions.Rows {
		named[row.Case] = true
	}
	for _, row := range doc.PreviewAssertions.Rows {
		named[row.Case] = true
	}
	asserted := map[string]bool{}
	shapes := map[string][]string{}
	for _, c := range doc.Cases {
		shape := fmt.Sprintf("%s@%dx%d", c.State, c.Width, c.Height)
		shapes[shape] = append(shapes[shape], c.Name)
		if named[c.Name] {
			asserted[shape] = true
		}
	}
	for shape, names := range shapes {
		if !asserted[shape] {
			t.Fatalf("no case of content shape %s (%v) asserts its text; that screen is verified by the golden byte-diff alone - add an entry naming what it must and must not show",
				shape, names)
		}
	}
}

// TestSelectionStep_RenderCarriesEachAnswer pins, per state, the specific text
// the golden must contain - so a snapshot that drifts is a caught regression
// rather than an accepted new baseline.
func TestSelectionStep_RenderCarriesEachAnswer(t *testing.T) {
	doc := loadSelectionRenderDoc(t)
	byName := map[string]selectionRenderCase{}
	for _, c := range doc.Cases {
		byName[c.Name] = c
	}
	requireEveryContentShapeAsserted(t, doc)
	for _, expect := range doc.RenderAssertions.Rows {
		c, ok := byName[expect.Case]
		if !ok {
			t.Fatalf("fixture has no case %q", expect.Case)
		}
		t.Run(expect.Case, func(t *testing.T) {
			view := stripRender(buildSelectionStep(t, doc, c).View())
			for _, s := range expect.WantContains {
				if !strings.Contains(view, s) {
					t.Errorf("state %q must show %q; view:\n%s", c.State, s, view)
				}
			}
			for _, s := range expect.WantMissing {
				if strings.Contains(view, s) {
					t.Errorf("state %q must not show %q; view:\n%s", c.State, s, view)
				}
			}
		})
	}
}

func mutateSelectionRenderCount(t *testing.T, field string, expected int) []byte {
	t.Helper()
	declared := []byte(fmt.Sprintf("%s: %d", field, expected))
	changed := []byte(fmt.Sprintf("%s: %d", field, expected+1))
	mutated := bytes.Replace(selectionRenderData, declared, changed, 1)
	if bytes.Equal(mutated, selectionRenderData) {
		t.Fatalf("selection render %s mutation did not alter the fixture", field)
	}
	return mutated
}

func TestSelectionRenderFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), selectionRenderData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted an unknown field")
	}
}

func TestSelectionRenderFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), selectionRenderData...), []byte("\n---\n{}\n")...)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted a trailing document")
	}
}

func TestSelectionRenderFixturePinsCaseCount(t *testing.T) {
	mutated := mutateSelectionRenderCount(t, "expectedCaseCount", expectedSelectionRenderCaseCount)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted a changed case-count declaration")
	}
}

func TestSelectionRenderFixturePinsSessionCount(t *testing.T) {
	mutated := mutateSelectionRenderCount(t, "expectedSessionCount", expectedSelectionRenderSessionCount)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted a changed session-count declaration")
	}
}

func TestSelectionRenderFixturePinsHarnessCount(t *testing.T) {
	mutated := mutateSelectionRenderCount(t, "expectedHarnessCount", expectedSelectionRenderHarnessCount)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted a changed harness-count declaration")
	}
}

func TestSelectionRenderFixturePinsPreviewAssertionCount(t *testing.T) {
	mutated := mutateSelectionRenderCount(t, "expectedRowCount", expectedSelectionRenderPreviewAssertionCount)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted a changed preview-assertion count")
	}
}

func TestSelectionRenderFixturePinsTextAssertionCount(t *testing.T) {
	// The first expectedRowCount belongs to renderAssertions; mutate it without
	// changing the independent preview assertion declaration.
	mutated := mutateSelectionRenderCount(t, "expectedRowCount", expectedSelectionRenderTextAssertionCount)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted a changed text-assertion count")
	}
}

func TestSelectionRenderFixturePinsBothThemeStateCount(t *testing.T) {
	mutated := mutateSelectionRenderCount(t, "expectedBothThemeStateCount", expectedSelectionRenderBothThemeStateCount)
	if _, err := decodeSelectionRender(mutated); err == nil {
		t.Fatal("selection render fixture accepted a changed both-theme state count")
	}
}
