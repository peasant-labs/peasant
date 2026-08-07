package settings

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/tree_facet.yaml
var treeFacetData []byte

// facetStateCase is one step of the facet cycle: how many times the filter key
// has been pressed, whether the gutter is drawn, which of its rows is active,
// and the session rows the narrowed tree must and must not show.
type facetStateCase struct {
	Name            string   `yaml:"name"`
	Presses         int      `yaml:"presses"`
	GutterVisible   bool     `yaml:"gutterVisible"`
	ActiveRow       string   `yaml:"activeRow"`
	WantRows        []string `yaml:"wantRows"`
	WantMissingRows []string `yaml:"wantMissingRows"`
}

// facetDocument is the whole fixture plus its row-count guards.
type facetDocument struct {
	Fixture            string           `yaml:"fixture"`
	GutterLabel        string           `yaml:"gutterLabel"`
	ExpectedStateCount int              `yaml:"expectedStateCount"`
	ExpectedValueCount int              `yaml:"expectedValueCount"`
	ExpectedCaseCount  int              `yaml:"expectedCaseCount"`
	NarrowWidth        int              `yaml:"narrowWidth"`
	GutterRows         []string         `yaml:"gutterRows"`
	States             []facetStateCase `yaml:"states"`
}

func loadFacetFixture(t *testing.T) facetDocument {
	t.Helper()
	var doc facetDocument
	dec := yaml.NewDecoder(bytes.NewReader(treeFacetData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/tree_facet.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("tree_facet.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.States) || len(doc.States) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d states present", doc.ExpectedCaseCount, len(doc.States))
	}
	// The cycle is the distinct values plus the every-value row and the hidden
	// state, and the gutter draws one row per value plus the every-value row.
	if doc.ExpectedStateCount != doc.ExpectedValueCount+2 {
		t.Fatalf("expectedStateCount=%d but expectedValueCount=%d", doc.ExpectedStateCount, doc.ExpectedValueCount)
	}
	if len(doc.GutterRows) != doc.ExpectedValueCount+1 {
		t.Fatalf("%d gutter rows but expectedValueCount=%d", len(doc.GutterRows), doc.ExpectedValueCount)
	}
	for _, state := range doc.States {
		// A state that expects no rows at all asserts nothing about what the
		// narrowed tree shows, so an edit that empties one must fail loudly.
		if len(state.WantRows)+len(state.WantMissingRows) == 0 {
			t.Fatalf("facet fixture state %q declares no expected rows; an empty want list turns the state into a guaranteed pass", state.Name)
		}
		if state.GutterVisible && state.ActiveRow == "" {
			t.Fatalf("facet fixture state %q draws the gutter but names no active row; the marked row is the state's whole point", state.Name)
		}
	}
	return doc
}

// facetDisplayName renders a harness slug the way the gutter shows it, standing
// in for the mounted registry's schema.HarnessDisplayName (which this package
// cannot reach without depending on the kickstart slice that composes it).
func facetDisplayName(value string) string { return strings.ReplaceAll(value, "-", " ") }

// facetRegistry is the tree step with a harness facet over src.
func facetRegistry(src kit.TreeSource, label string) Registry {
	return Registry{Sections: []Section{
		{
			Key:   "transcripts",
			Title: "select transcripts",
			Fields: []Field{Tree("selection", "transcripts", selectionAccessor(), src,
				WithFacet(MetaHarness, label), WithFacetDisplay(facetDisplayName))},
		},
	}}
}

// facetFlow mounts the faceted fixture in a real Flow and completes its load.
func facetFlow(t *testing.T, doc facetDocument) (Flow, *Draft) {
	t.Helper()
	path, loaded := writeConfigFile(t)
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	f := NewFlow(theme.New(theme.ModeDark), facetRegistry(scannerfix.NewFixtureTreeSource(doc.Fixture), doc.GutterLabel), d)
	f.SetSize(90, 24)
	return drainInit(f), d
}

// TestFlow_TreeFacetCyclesAndNarrows drives the REAL flow: the gutter is shown
// by default with a count per harness, each press of the filter key narrows the
// rendered rows to the next harness, the last state hides the gutter and its
// filter, and the cycle wraps.
func TestFlow_TreeFacetCyclesAndNarrows(t *testing.T) {
	doc := loadFacetFixture(t)
	for _, state := range doc.States {
		t.Run(state.Name, func(t *testing.T) {
			f, _ := facetFlow(t, doc)
			for i := 0; i < state.Presses; i++ {
				f = send(f, "f")
			}
			view := f.View()

			if state.GutterVisible {
				if !strings.Contains(view, doc.GutterLabel) {
					t.Errorf("gutter label %q not drawn; view:\n%s", doc.GutterLabel, view)
				}
				for _, row := range doc.GutterRows {
					if !strings.Contains(view, row) {
						t.Errorf("gutter row %q not drawn; view:\n%s", row, view)
					}
				}
				if state.ActiveRow != "" && !strings.Contains(view, "> "+state.ActiveRow) {
					t.Errorf("gutter row %q not marked active; view:\n%s", state.ActiveRow, view)
				}
			} else {
				for _, row := range doc.GutterRows {
					if strings.Contains(view, row) {
						t.Errorf("hidden gutter still drew row %q; view:\n%s", row, view)
					}
				}
			}

			for _, want := range state.WantRows {
				if !strings.Contains(view, want) {
					t.Errorf("session row %q missing; view:\n%s", want, view)
				}
			}
			for _, missing := range state.WantMissingRows {
				if strings.Contains(view, missing) {
					t.Errorf("session row %q must be filtered out; view:\n%s", missing, view)
				}
			}
		})
	}
}

// TestFlow_TreeFacetKeepsFilteredOutSelections proves the facet is a VIEW, not a
// scope: a session selected while one harness is shown is still in the committed
// selection after the facet narrows to the OTHER harness, so filtering the rows
// can never silently drop what the user already chose.
func TestFlow_TreeFacetKeepsFilteredOutSelections(t *testing.T) {
	doc := loadFacetFixture(t)
	f, d := facetFlow(t, doc)

	// With every harness visible the rows are project, branch, then its two
	// sessions: step down onto the opencode session and select just that one.
	f = send(f, "down", "down", "down", "space")
	before := d.Working().Selection
	if before.Mode != config.SelectionModeSelected {
		t.Fatalf("selecting one session did not produce mode selected, got %q", before.Mode)
	}
	if !selectsSession(before, facetOtherHarness, facetBranch, facetOtherSession) {
		t.Fatalf("the chosen session is not in the selection: %v", before.Harnesses)
	}

	// Narrow to the OTHER harness: the chosen session is no longer rendered, but
	// it must still be in what a commit would persist.
	f = send(f, "f")
	if view := f.View(); strings.Contains(view, "commit detector fix") {
		t.Fatalf("the facet did not narrow away the other harness; view:\n%s", view)
	}
	after := d.Working().Selection
	if !selectsSession(after, facetOtherHarness, facetBranch, facetOtherSession) {
		t.Fatalf("narrowing the facet dropped a selection it had filtered out: %v", after.Harnesses)
	}

	// Editing under the narrowed view must not drop it either: select a session
	// of the visible harness and confirm both are now in the selection.
	f = send(f, "down", "down", "space")
	edited := d.Working().Selection
	if !selectsSession(edited, facetOtherHarness, facetBranch, facetOtherSession) {
		t.Fatalf("editing under a narrowed facet dropped the filtered-out selection: %v", edited.Harnesses)
	}
	if !selectsSession(edited, facetVisibleHarness, facetBranch, facetVisibleSession) {
		t.Fatalf("the session selected under the narrowed facet was not recorded: %v", edited.Harnesses)
	}
}

// facetOtherHarness / facetOtherSession name the session the filtered-out test
// selects: an opencode session, which the first facet value (claude-code)
// narrows away.
// facetVisibleHarness / facetVisibleSession name the session it then selects
// while narrowed: a claude-code session, which the same facet value shows.
// facetRemote / facetBranch locate both in the fixture's forest.
const (
	facetOtherSession   = "oc-1"
	facetVisibleSession = "cc-1"
	facetSecondSession  = "cc-2"
	facetRemote         = "git@github.com:peasant-labs/peasant.git"
	facetBranch         = "develop"
	facetSecondBranch   = "feature-x"
)

// The harnesses are named by their typed constants, never a bare slug, so a
// renamed harness is a compile error here rather than a silently-passing test.
var (
	facetOtherHarness   = string(defaults.HarnessOpenCode)
	facetVisibleHarness = string(defaults.HarnessClaudeCode)
)

// selectsSession reports whether a derived selection ADMITS one session, using
// the canonical ingest.SelectionMatcher rather than reading the persisted shape
// by hand - a selection may name a session directly or cover it through its
// branch or project, and both must count as selected.
func selectsSession(sel config.SelectionConfig, harness, branch, sessionID string) bool {
	matcher := config.CompileSelectionMatcher(sel)
	match := matcher.MatchDiscovery(
		ingest.Harness(harness), facetRemote, "", branch,
		ingest.SessionID(sessionID), sel.AutoIngestNewBranches)
	return match == ingest.BranchMatchYes
}

// TestFlow_TreeFacetDefaultsToTheRawValue proves the display seam is optional:
// with no WithFacetDisplay the gutter names each value exactly as the source
// wrote it, so a facet over any Meta key is readable without extra wiring.
func TestFlow_TreeFacetDefaultsToTheRawValue(t *testing.T) {
	doc := loadFacetFixture(t)
	path, loaded := writeConfigFile(t)
	d, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	reg := Registry{Sections: []Section{{
		Key:    "transcripts",
		Title:  "select transcripts",
		Fields: []Field{Tree("selection", "transcripts", selectionAccessor(), scannerfix.NewFixtureTreeSource(doc.Fixture), WithFacet(MetaHarness, doc.GutterLabel))},
	}}}
	f := NewFlow(theme.New(theme.ModeDark), reg, d)
	f.SetSize(90, 24)
	f = drainInit(f)

	if view := f.View(); !strings.Contains(view, "claude-code 2") {
		t.Fatalf("gutter did not name the raw facet value; view:\n%s", view)
	}
}

// TestFlow_TreeFacetSelectsThroughANarrowedInteriorNode proves a selection made
// on an INTERIOR row of a narrowed view lands on the real forest: selecting the
// project while the tree shows only one harness records that harness's sessions
// across every branch, and leaves the hidden harness's sessions alone. The
// interior rows of a narrowed view are the field's own copies, so the whole
// forest must be rolled up again before the selection is derived - without that
// the project still reads as unselected and everything just chosen is dropped.
func TestFlow_TreeFacetSelectsThroughANarrowedInteriorNode(t *testing.T) {
	doc := loadFacetFixture(t)
	f, d := facetFlow(t, doc)

	f = send(f, "f")     // narrow to the first harness (claude-code)
	f = send(f, "space") // select the project row the narrowed view opens on

	sel := d.Working().Selection
	if sel.Mode != config.SelectionModeSelected {
		t.Fatalf("selection mode = %q, want selected", sel.Mode)
	}
	for _, want := range []struct{ branch, session string }{
		{facetBranch, facetVisibleSession},
		{facetSecondBranch, facetSecondSession},
	} {
		if !selectsSession(sel, facetVisibleHarness, want.branch, want.session) {
			t.Errorf("session %q of the visible harness was not selected: %v", want.session, sel.Harnesses)
		}
	}
	if selectsSession(sel, facetOtherHarness, facetBranch, facetOtherSession) {
		t.Errorf("selecting through a narrowed view also selected the hidden harness: %v", sel.Harnesses)
	}
}

// TestFlow_TreeFacetCollapsesWhenTooNarrow proves the gutter yields rather than
// squeezing the rows it exists to explain: in a region too narrow to carry both,
// the gutter is not drawn and the tree keeps the whole width.
func TestFlow_TreeFacetCollapsesWhenTooNarrow(t *testing.T) {
	doc := loadFacetFixture(t)
	if doc.NarrowWidth <= 0 {
		t.Fatalf("fixture declares no narrowWidth")
	}
	f, _ := facetFlow(t, doc)
	f.SetSize(doc.NarrowWidth, 24)
	view := f.View()

	for _, row := range doc.GutterRows {
		if strings.Contains(view, row) {
			t.Errorf("gutter row %q drawn in a region too narrow for it; view:\n%s", row, view)
		}
	}
	if strings.TrimSpace(view) == "" {
		t.Fatalf("the tree rendered nothing at width %d", doc.NarrowWidth)
	}
}
