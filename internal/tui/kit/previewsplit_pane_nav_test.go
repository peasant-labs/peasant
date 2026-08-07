package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/previewsplit_pane_nav.yaml
var previewPaneNavData []byte

// previewPaneNavCase is one keystroke sequence and what the split must look
// like after it: which pane takes input, which item the list still highlights,
// and which content lines the viewport shows.
type previewPaneNavCase struct {
	Name          string   `yaml:"name"`
	Presses       []string `yaml:"presses"`
	WantPane      string   `yaml:"wantPane"`
	WantHighlight string   `yaml:"wantHighlight"`
	WantVisible   []string `yaml:"wantVisible"`
	WantMissing   []string `yaml:"wantMissing"`
}

// previewPaneNavDoc is the whole fixture: the region and body the split is
// mounted over, the cases, and the row-count guard.
type previewPaneNavDoc struct {
	ExpectedCaseCount int                  `yaml:"expectedCaseCount"`
	Width             int                  `yaml:"width"`
	Height            int                  `yaml:"height"`
	ContentLines      int                  `yaml:"contentLines"`
	Items             []string             `yaml:"items"`
	Cases             []previewPaneNavCase `yaml:"cases"`
}

func loadPreviewPaneNav(t *testing.T) previewPaneNavDoc {
	t.Helper()
	var doc previewPaneNavDoc
	dec := yaml.NewDecoder(bytes.NewReader(previewPaneNavData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/previewsplit_pane_nav.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("previewsplit_pane_nav.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if doc.Width <= 0 || doc.Height <= 0 || doc.ContentLines <= 0 {
		t.Fatalf("fixture declares a %dx%d region over %d content lines; a non-positive value renders nothing to assert on",
			doc.Width, doc.Height, doc.ContentLines)
	}
	if len(doc.Items) == 0 {
		t.Fatal("fixture declares no list items, so there is no highlight to assert")
	}
	for _, c := range doc.Cases {
		if c.Name == "" {
			t.Fatal("a pane-nav case declares no name")
		}
		if c.WantPane != kit.PaneLeft.String() && c.WantPane != kit.PaneRight.String() {
			t.Fatalf("pane-nav case %q wants pane %q, which is neither %q nor %q",
				c.Name, c.WantPane, kit.PaneLeft, kit.PaneRight)
		}
		if len(c.WantVisible)+len(c.WantMissing) == 0 {
			t.Fatalf("pane-nav case %q asserts nothing about the viewport; an empty want list is a guaranteed pass", c.Name)
		}
	}
	return doc
}

// numberedContentSource returns a fixed body of distinctly-numbered lines, so a
// scroll assertion can name the exact lines a viewport window holds.
type numberedContentSource struct{ body string }

func newNumberedContentSource(lines int) numberedContentSource {
	var b strings.Builder
	for i := range lines {
		fmt.Fprintf(&b, "line-%02d\n", i)
	}
	return numberedContentSource{body: b.String()}
}

func (s numberedContentSource) Content(_ string, _ int) (string, error) { return s.body, nil }

// TestPreviewSplit_PaneNavigation drives the REAL split through each fixture's
// keystroke sequence and asserts where input goes and what the viewport shows.
// It covers the two halves of the same contract: ctrl+h / ctrl+l move focus
// ACROSS the divider, and once the preview holds focus the movement keys scroll
// it instead of moving the list cursor underneath.
func TestPreviewSplit_PaneNavigation(t *testing.T) {
	t.Parallel()
	doc := loadPreviewPaneNav(t)

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			ps := mountPaneNavSplit(t, doc)
			for _, press := range c.Presses {
				ps, _ = ps.Update(keyPress(t, press))
			}

			if got := ps.ActivePane().String(); got != c.WantPane {
				t.Errorf("active pane = %s, want %s", got, c.WantPane)
			}
			if id, ok := ps.HighlightedID(); !ok || id != c.WantHighlight {
				t.Errorf("highlighted item = %q (present=%t), want %q", id, ok, c.WantHighlight)
			}
			view := stripANSI(ps.View())
			for _, want := range c.WantVisible {
				if !strings.Contains(view, want) {
					t.Errorf("viewport must show %q; view:\n%s", want, view)
				}
			}
			for _, missing := range c.WantMissing {
				if strings.Contains(view, missing) {
					t.Errorf("viewport must not show %q; view:\n%s", missing, view)
				}
			}
		})
	}
}

// mountPaneNavSplit builds the split the fixture describes and drives its real
// load command to completion, so every case starts from a loaded, focused
// component rather than a hand-set internal state.
func mountPaneNavSplit(t *testing.T, doc previewPaneNavDoc) kit.PreviewSplit {
	t.Helper()
	items := make([]kit.ListItem, 0, len(doc.Items))
	for _, label := range doc.Items {
		items = append(items, kit.StringItem(label))
	}
	ps := kit.NewPreviewSplit(darkTheme(), kit.NewListLeftPane(kit.NewList(darkTheme(), items)), newNumberedContentSource(doc.ContentLines))
	ps.SetSize(doc.Width, doc.Height)
	ps.Focus()
	for _, msg := range collectMsgs(ps.Load()) {
		ps, _ = ps.Update(msg)
	}
	if ps.Loading() {
		t.Fatal("split is still loading after its own load command ran")
	}
	return ps
}

// TestPreviewSplit_AdvertisesTheActivePanesActions proves the footer and help
// overlay follow focus: a split reports the left pane's own actions while the
// list has focus and the viewport's scroll set once the preview does, with the
// two pane-focus keys always available. Advertising a fixed list would let the
// hint bar promise keys the active pane cannot dispatch - the exact drift the
// keymap package exists to prevent.
func TestPreviewSplit_AdvertisesTheActivePanesActions(t *testing.T) {
	t.Parallel()
	doc := loadPreviewPaneNav(t)
	ps := mountPaneNavSplit(t, doc)

	onList := ps.PaneActions()
	assertHasActions(t, "list-focused", onList, keymap.ActionFocusPaneLeft, keymap.ActionFocusPaneRight, keymap.ActionConfirm)
	assertLacksActions(t, "list-focused", onList, keymap.ActionTop, keymap.ActionBottom)

	ps, _ = ps.Update(keyPress(t, "ctrl+l"))
	onPreview := ps.PaneActions()
	assertHasActions(t, "preview-focused", onPreview,
		keymap.ActionFocusPaneLeft, keymap.ActionFocusPaneRight,
		keymap.ActionUp, keymap.ActionDown, keymap.ActionPageUp, keymap.ActionPageDown,
		keymap.ActionTop, keymap.ActionBottom)
	assertLacksActions(t, "preview-focused", onPreview, keymap.ActionConfirm)

	// The tab/shift+tab toggle is NOT part of the pane set - a host that steps
	// between form fields with those keys must be able to keep them.
	assertLacksActions(t, "pane-actions", onPreview, keymap.ActionNextField, keymap.ActionPrevField)
	assertHasActions(t, "full-availability", ps.AvailableActions(), keymap.ActionNextField, keymap.ActionPrevField)
}

func assertHasActions(t *testing.T, state string, got []keymap.ActionID, want ...keymap.ActionID) {
	t.Helper()
	present := map[keymap.ActionID]bool{}
	for _, a := range got {
		present[a] = true
	}
	for _, a := range want {
		if !present[a] {
			t.Errorf("%s: split does not advertise %s; advertised: %v", state, a, actionNames(got))
		}
	}
}

func assertLacksActions(t *testing.T, state string, got []keymap.ActionID, unwanted ...keymap.ActionID) {
	t.Helper()
	present := map[keymap.ActionID]bool{}
	for _, a := range got {
		present[a] = true
	}
	for _, a := range unwanted {
		if present[a] {
			t.Errorf("%s: split advertises %s, which the active pane cannot dispatch; advertised: %v", state, a, actionNames(got))
		}
	}
}

func actionNames(actions []keymap.ActionID) []string {
	names := make([]string, 0, len(actions))
	for _, a := range actions {
		names = append(names, a.String())
	}
	return names
}
