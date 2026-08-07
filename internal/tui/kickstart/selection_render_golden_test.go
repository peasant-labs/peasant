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
	"github.com/peasant-labs/peasant/internal/testutil"
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

// selectionRenderCase is one captured screen: the state the step is driven
// into, the palette, and the region it renders at.
type selectionRenderCase struct {
	Name   string `yaml:"name"`
	State  string `yaml:"state"`
	Theme  string `yaml:"theme"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
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
	ExpectedCaseCount    int                   `yaml:"expectedCaseCount"`
	ExpectedHarnessCount int                   `yaml:"expectedHarnessCount"`
	PreviewAssertions    previewAssertions     `yaml:"previewAssertions"`
	Stored               map[string]string     `yaml:"stored"`
	Listings             []ftue.SessionListing `yaml:"listings"`
	Ingested             []string              `yaml:"ingested"`
	Cases                []selectionRenderCase `yaml:"cases"`
}

func loadSelectionRenderDoc(t *testing.T) selectionRenderDoc {
	t.Helper()
	var doc selectionRenderDoc
	dec := yaml.NewDecoder(bytes.NewReader(selectionRenderData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/selection_render.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("selection_render.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	harnesses := map[string]bool{}
	for _, sess := range doc.Listings {
		harnesses[sess.Harness] = true
	}
	pa := doc.PreviewAssertions
	if pa.ExpectedRowCount != len(pa.Rows) || len(pa.Rows) == 0 {
		t.Fatalf("previewAssertions.expectedRowCount=%d but %d rows present", pa.ExpectedRowCount, len(pa.Rows))
	}
	for _, row := range pa.Rows {
		if len(row.WantVisible)+len(row.WantMissing)+len(row.WantColored) == 0 {
			t.Fatalf("preview assertion row %q declares no expected values; an empty want list is a guaranteed pass", row.Case)
		}
		testutil.RequireFixtureFields(t, "preview assertion", row.Case, []testutil.FixtureField{
			{Key: "wantFocusMarker", Value: row.WantFocusMarker},
		})
	}
	if len(harnesses) != doc.ExpectedHarnessCount {
		t.Fatalf("expectedHarnessCount=%d but the listing carries %d", doc.ExpectedHarnessCount, len(harnesses))
	}
	for _, c := range doc.Cases {
		testutil.RequireFixtureFields(t, "selection render", c.Name, []testutil.FixtureField{
			{Key: "state", Value: c.State},
			{Key: "theme", Value: c.Theme},
		})
		if c.Width <= 0 || c.Height <= 0 {
			t.Fatalf("selection render fixture case %q declares a %dx%d region; a non-positive size captures nothing",
				c.Name, c.Width, c.Height)
		}
	}
	return doc
}

func renderThemeFor(t *testing.T, name string) theme.Mode {
	t.Helper()
	switch name {
	case "dark":
		return theme.ModeDark
	case "light":
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
	case "default":
	case "narrowed":
		p = pressAndDrain(p, 'f')
	case "gutter-hidden":
		// The cycle is every-value, then one state per harness, then hidden.
		for i := 0; i < doc.ExpectedHarnessCount+1; i++ {
			p = pressAndDrain(p, 'f')
		}
	case "session-cursor":
		p = cursorOntoImportedSession(p)
	case "preview-focused":
		p = drainProgram(cursorOntoImportedSession(p).Update(focusPreviewKey()))
	case "preview-scrolled":
		p = drainProgram(cursorOntoImportedSession(p).Update(focusPreviewKey()))
		// Far enough to carry the pane past the identity lines AND past the
		// prose, so the capture proves the viewport moved rather than that a
		// blank line appeared at the top.
		for i := 0; i < previewScrollRows; i++ {
			p = pressAndDrain(p, 'j')
		}
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
func requireEveryContentShapeAsserted(t *testing.T, doc selectionRenderDoc, want map[string]struct{ contains, missing []string }) {
	t.Helper()
	// A shape counts as asserted from EITHER source: this test's per-state text
	// expectations, or the fixture's previewAssertions rows, which name what the
	// preview pane must show for the states that exist to capture it.
	named := map[string]bool{}
	for name := range want {
		named[name] = true
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
	want := map[string]struct{ contains, missing []string }{
		"default-dark": {
			contains: []string{
				"+ 2 child sessions", // the parent summarises its subagent chain
				"already imported",   // the stored session is marked
				"harness",            // the facet gutter is shown by default
				"claude code",
				"cursor",
			},
			missing: []string{"child subagent", "grandchild subagent"},
		},
		"narrowed-dark": {
			contains: []string{"+ 2 child sessions", "claude code"},
			missing:  []string{"cursor session"}, // narrowed away
		},
		"gutter-hidden-dark": {
			contains: []string{"+ 2 child sessions", "cursor session"},
			missing:  []string{"claude code 3"}, // the gutter rows are gone
		},
		// The preview shows the highlighted session's whole recorded
		// conversation, tagged by voice. It deliberately does NOT repeat the
		// session title: a title is derived from the first user message, and
		// that message is the first thing the transcript below renders.
		"session-cursor-dark": {
			contains: []string{
				"harness: claude code",                // the pane's own header chrome
				"you",                                 // the person's turn is tagged
				"assistant",                           // and so is the agent's
				"please refactor the ingest pipeline", // the first turn
				recordedExchange,                      // and the reply after it
			},
			missing: []string{"imported session"},
		},
		// A narrower region is NOT a colour variant of session-cursor-dark: the
		// tree pane no longer has the budget to carry a row annotation, so both
		// are dropped while the preview keeps describing the highlighted row.
		"session-cursor-narrow": {
			contains: []string{
				"harness: claude code",                // the preview names the row
				"please refactor the ingest pipeline", // and the recorded transcript
			},
			missing: []string{"child sessions", "already imported", "imported session"},
		},
	}
	requireEveryContentShapeAsserted(t, doc, want)
	for name, expect := range want {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("fixture has no case %q", name)
		}
		t.Run(name, func(t *testing.T) {
			view := stripRender(buildSelectionStep(t, doc, c).View())
			for _, s := range expect.contains {
				if !strings.Contains(view, s) {
					t.Errorf("state %q must show %q; view:\n%s", c.State, s, view)
				}
			}
			for _, s := range expect.missing {
				if strings.Contains(view, s) {
					t.Errorf("state %q must not show %q; view:\n%s", c.State, s, view)
				}
			}
		})
	}
}
