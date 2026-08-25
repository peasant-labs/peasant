package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/x/exp/golden"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/child_count_line.yaml
var childCountLineData []byte

// childCountLineCase is one whole rendered screen: the terminal size and theme
// the shared forest renders at.
type childCountLineCase struct {
	Name   string `yaml:"name"`
	Theme  string `yaml:"theme"`
	Width  int    `yaml:"width"`
	Height int    `yaml:"height"`
}

// childCountLineDocument is the whole fixture plus its deletion-protection
// manifest. RequiredCaseNames is not a case count: it names every case that
// must remain present, and adding a case never requires touching it.
type childCountLineDocument struct {
	RequiredCaseNames []string             `yaml:"requiredCaseNames"`
	Cases             []childCountLineCase `yaml:"cases"`
}

func loadChildCountLines(t *testing.T) childCountLineDocument {
	t.Helper()
	var doc childCountLineDocument
	dec := yaml.NewDecoder(bytes.NewReader(childCountLineData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/child_count_line.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("child_count_line.yaml must hold exactly one document: %v", err)
	}
	present := make(map[string]bool, len(doc.Cases))
	for _, c := range doc.Cases {
		present[c.Name] = true
		if c.Width <= 0 || c.Height <= 0 {
			t.Fatalf("child count line case %q renders at %dx%d; a non-positive size renders nothing to read", c.Name, c.Width, c.Height)
		}
		if c.Theme != "dark" && c.Theme != "light" {
			t.Fatalf("child count line case %q declares unknown theme %q", c.Name, c.Theme)
		}
	}
	if err := testutil.RequireFixtureNames("child count line fixture", "case", doc.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return doc
}

const (
	childCountLineParentLabel  = "parent session"
	childCountLineSiblingLabel = "plain session"
	childCountLineText         = "+ 2 child sessions"
)

// childCountLineForest is the forest every case renders: one project holding a
// parent that carries a count and a plain sibling that carries none, so each
// captured screen shows both the annotated and the unannotated shape.
func childCountLineForest() []*kit.TreeNode {
	return []*kit.TreeNode{{
		ID:    "project",
		Label: "acme/tool",
		Children: []*kit.TreeNode{
			{
				ID:    "parent",
				Label: childCountLineParentLabel,
				Meta:  map[string]string{kit.MetaChildCount: "2"},
			},
			{ID: "sibling", Label: childCountLineSiblingLabel},
		},
	}}
}

func childCountLineTree(t *testing.T, c childCountLineCase) kit.Tree {
	t.Helper()
	mode := theme.ModeDark
	if c.Theme == "light" {
		mode = theme.ModeLight
	}
	tr := kit.NewTree(theme.New(mode), staticSource{roots: childCountLineForest()})
	tr, cmd := tr.Load()
	if msg := runCmd(cmd); msg != nil {
		tr, _ = tr.Update(msg)
	}
	tr.SetSize(c.Width, c.Height)
	return tr
}

// TestTree_ChildCountLineGolden captures the whole rendered screen for every
// size and theme, so a reviewer reads the actual layout - the count on its own
// line, indented under its parent - instead of a substring assertion about it.
func TestTree_ChildCountLineGolden(t *testing.T) {
	doc := loadChildCountLines(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			golden.RequireEqual(t, []byte(childCountLineTree(t, c).View()))
		})
	}
}

// TestTree_ChildCountLinePlacementAndIndent proves the two properties the
// goldens display, as assertions that NAME what moved: the count renders on the
// line immediately BELOW its parent (never beside the title), and that line
// starts exactly one indent level to the right of the parent's title. A golden
// alone would go red without saying which of the two changed.
func TestTree_ChildCountLinePlacementAndIndent(t *testing.T) {
	doc := loadChildCountLines(t)
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			lines := strings.Split(stripANSI(childCountLineTree(t, c).View()), "\n")
			parent := -1
			for i, line := range lines {
				if strings.Contains(line, childCountLineParentLabel) {
					parent = i
					break
				}
			}
			if parent < 0 {
				t.Fatalf("no row carries the parent title %q; screen:\n%s", childCountLineParentLabel, strings.Join(lines, "\n"))
			}
			if strings.Contains(lines[parent], childCountLineText) {
				t.Errorf("the count is still on the parent's OWN line, not below it; row:\n%s", lines[parent])
			}
			if parent+1 >= len(lines) {
				t.Fatalf("the parent row is the last line, so it has no count line beneath it")
			}
			below := lines[parent+1]
			if !strings.Contains(below, childCountLineText) {
				t.Fatalf("the line directly below the parent does not carry %q; it reads:\n%q", childCountLineText, below)
			}
			// The parent's title column, and where the count line actually starts.
			titleColumn := strings.Index(lines[parent], childCountLineParentLabel)
			countColumn := strings.Index(below, childCountLineText)
			const indentStep = 2
			if want := titleColumn + indentStep; countColumn != want {
				t.Errorf("the count line starts at column %d, want %d: it must be exactly one indent level (%d cells) right of the parent title at column %d",
					countColumn, want, indentStep, titleColumn)
			}
		})
	}
}

// TestTree_ChildCountLineIsNotSelectable proves the count is PRESENTATION only.
// It renders as a line, but it is not a node: the forest exposes exactly the
// rows it had before, so no amount of moving the cursor can land on the count
// or pull it into a selection.
func TestTree_ChildCountLineIsNotSelectable(t *testing.T) {
	doc := loadChildCountLines(t)
	c := doc.Cases[0]
	tr := childCountLineTree(t, c)
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		node, ok := tr.CurrentNode()
		if !ok {
			t.Fatal("the tree reports no current node")
		}
		seen[node.ID] = true
		tr, _ = tr.Update(keyPress(t, "down"))
	}
	for _, id := range []string{"project", "parent", "sibling"} {
		if !seen[id] {
			t.Errorf("walking the whole tree never reached the %q row", id)
		}
	}
	if len(seen) != 3 {
		t.Errorf("the cursor reached %d distinct rows (%v), want exactly the 3 real nodes: the count line must never become one", len(seen), seen)
	}
}
