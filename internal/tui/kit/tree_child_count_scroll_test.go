package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/child_count_scroll.yaml
var childCountScrollData []byte

// childCountScrollSession is one session row of the case's forest. A non-empty
// childCount makes it a TWO-LINE row.
type childCountScrollSession struct {
	ID         string `yaml:"id"`
	Label      string `yaml:"label"`
	ChildCount string `yaml:"childCount"`
}

// childCountScrollCase is one window: the viewport, the forest, the keys to
// replay, and what the resulting screen must show.
type childCountScrollCase struct {
	Name                  string                    `yaml:"name"`
	Width                 int                       `yaml:"width"`
	Height                int                       `yaml:"height"`
	Sessions              []childCountScrollSession `yaml:"sessions"`
	Keys                  []string                  `yaml:"keys"`
	WantCursor            string                    `yaml:"wantCursor"`
	WantFirstLineContains string                    `yaml:"wantFirstLineContains"`
	WantLastLineContains  string                    `yaml:"wantLastLineContains"`
	// WantLastLineBlank states that the window deliberately UNDERFILLS: the
	// rows left below the offset cannot fill the viewport, and a row is never
	// split across an edge to fill it.
	WantLastLineBlank  bool     `yaml:"wantLastLineBlank"`
	WantPresent        []string `yaml:"wantPresent"`
	WantAbsent         []string `yaml:"wantAbsent"`
	WantTopOverflow    bool     `yaml:"wantTopOverflow"`
	WantBottomOverflow bool     `yaml:"wantBottomOverflow"`
}

type childCountScrollDocument struct {
	RequiredCaseNames []string               `yaml:"requiredCaseNames"`
	Cases             []childCountScrollCase `yaml:"cases"`
}

const childCountScrollProjectLabel = "acme/tool"

func loadChildCountScroll(t *testing.T) childCountScrollDocument {
	t.Helper()
	var doc childCountScrollDocument
	dec := yaml.NewDecoder(bytes.NewReader(childCountScrollData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/child_count_scroll.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("child_count_scroll.yaml must hold exactly one document: %v", err)
	}
	present := make(map[string]bool, len(doc.Cases))
	for _, c := range doc.Cases {
		present[c.Name] = true
		if c.Width <= 0 || c.Height <= 0 {
			t.Fatalf("scroll case %q renders at %dx%d; a non-positive size renders nothing to assert on", c.Name, c.Width, c.Height)
		}
		if c.WantCursor == "" {
			t.Fatalf("scroll case %q names no expected cursor row", c.Name)
		}
		// Exactly one of the two says what the bottom of the window holds; a
		// case that says neither would let the window end anywhere.
		if (c.WantLastLineContains != "") == c.WantLastLineBlank {
			t.Fatalf("scroll case %q must declare either wantLastLineContains or wantLastLineBlank, not both and not neither", c.Name)
		}
		// A case whose forest fits the viewport would assert nothing about
		// scrolling, which is the whole point of this fixture.
		if lines := c.forestLines(); lines <= c.Height {
			t.Fatalf("scroll case %q builds a forest %d lines tall for a viewport of %d: it never scrolls, so it cannot exercise the window arithmetic",
				c.Name, lines, c.Height)
		}
		if !c.holdsTwoLineRow() {
			t.Fatalf("scroll case %q holds no session with a child count, so every row is one line and the case proves nothing this fixture exists for", c.Name)
		}
	}
	if err := testutil.RequireFixtureNames("child count scroll fixture", "case", doc.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return doc
}

// forestLines is how tall the case's forest renders: the project row, each
// session row, and a second line for every session carrying a count.
func (c childCountScrollCase) forestLines() int {
	lines := 1
	for _, s := range c.Sessions {
		lines++
		if s.ChildCount != "" {
			lines++
		}
	}
	return lines
}

func (c childCountScrollCase) holdsTwoLineRow() bool {
	for _, s := range c.Sessions {
		if s.ChildCount != "" {
			return true
		}
	}
	return false
}

func (c childCountScrollCase) forest() []*kit.TreeNode {
	project := &kit.TreeNode{ID: "project", Label: childCountScrollProjectLabel}
	for _, s := range c.Sessions {
		node := &kit.TreeNode{ID: s.ID, Label: s.Label}
		if s.ChildCount != "" {
			node.Meta = map[string]string{kit.MetaChildCount: s.ChildCount}
		}
		project.Children = append(project.Children, node)
	}
	return []*kit.TreeNode{project}
}

// TestTree_ChildCountLineScrolling drives a window that is genuinely too small
// for its forest and holds two-line rows: the combination a real store produces
// on every run, and the one the rest of the tree's coverage never reaches.
func TestTree_ChildCountLineScrolling(t *testing.T) {
	doc := loadChildCountScroll(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			tr := loadStaticTree(t, c.forest(), c.Width, c.Height)
			for _, key := range c.Keys {
				tr, _ = tr.Update(keyPress(t, key))
			}
			node, ok := tr.CurrentNode()
			if !ok {
				t.Fatalf("no node under the cursor after keys %v", c.Keys)
			}
			if node.ID != c.WantCursor {
				t.Fatalf("cursor is on %q, want %q after keys %v", node.ID, c.WantCursor, c.Keys)
			}
			view := stripANSI(tr.View())
			lines := strings.Split(view, "\n")
			if len(lines) != c.Height {
				t.Fatalf("the window rendered %d lines for a viewport of %d", len(lines), c.Height)
			}
			// The cursor's own row line must be on screen. This is the property
			// the line-measured correction exists to hold: a window sized in
			// rows can push it off the bottom once rows differ in height.
			if !strings.Contains(view, node.Label) {
				t.Errorf("the cursor is on %q but its row line is not on screen; window:\n%s", node.Label, view)
			}
			if c.WantFirstLineContains != "" && !strings.Contains(lines[0], c.WantFirstLineContains) {
				t.Errorf("the first visible line is %q, want it to contain %q: the window starts in the wrong place",
					strings.TrimRight(lines[0], " "), c.WantFirstLineContains)
			}
			last := lines[len(lines)-1]
			if c.WantLastLineContains != "" && !strings.Contains(last, c.WantLastLineContains) {
				t.Errorf("the last visible line is %q, want it to contain %q: the window ends in the wrong place",
					strings.TrimRight(last, " "), c.WantLastLineContains)
			}
			if c.WantLastLineBlank && strings.TrimSpace(last) != "" {
				t.Errorf("the last visible line is %q, want it blank: this window underfills by design rather than splitting a row across its top edge",
					strings.TrimRight(last, " "))
			}
			for _, present := range c.WantPresent {
				if !strings.Contains(view, present) {
					t.Errorf("the window does not show %q, which must be inside it; window:\n%s", present, view)
				}
			}
			for _, absent := range c.WantAbsent {
				if strings.Contains(view, absent) {
					t.Errorf("the window still shows %q, which is outside it; window:\n%s", absent, view)
				}
			}
			overflow := tr.Overflow()
			if overflow.Top != c.WantTopOverflow {
				t.Errorf("top overflow = %v, want %v", overflow.Top, c.WantTopOverflow)
			}
			if overflow.Bottom != c.WantBottomOverflow {
				t.Errorf("bottom overflow = %v, want %v", overflow.Bottom, c.WantBottomOverflow)
			}
		})
	}
}
