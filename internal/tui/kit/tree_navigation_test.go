package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/tree_navigation.yaml
var treeNavigationData []byte

// navigationCase is one Tree interaction scenario: a forest, a visible-row
// budget, an ordered key sequence, and the observable expectations after it.
type navigationCase struct {
	Name             string            `yaml:"name"`
	Height           int               `yaml:"height"`
	Forest           []fixtureTreeNode `yaml:"forest"`
	Keys             []string          `yaml:"keys"`
	ExpectCursorID   string            `yaml:"expectCursorID"`
	ExpectVisibleIDs []string          `yaml:"expectVisibleIDs"`
	CheckSelection   bool              `yaml:"checkSelection"`
	ExpectChecked    []string          `yaml:"expectChecked"`
}

type navigationDocument struct {
	ExpectedCaseCount int              `yaml:"expectedCaseCount"`
	Cases             []navigationCase `yaml:"cases"`
}

// defaultNavigationForest is the two-project forest every case reuses when it
// does not declare its own. It is defined here (not repeated per fixture row)
// so a shape change is one edit.
func defaultNavigationForest() []fixtureTreeNode {
	return []fixtureTreeNode{
		{
			ID: "projA",
			Children: []fixtureTreeNode{
				{
					ID: "projA/b1",
					Children: []fixtureTreeNode{
						{ID: "projA/b1/s1", State: "unchecked"},
						{ID: "projA/b1/s2", State: "unchecked"},
					},
				},
				{
					ID: "projA/b2",
					Children: []fixtureTreeNode{
						{ID: "projA/b2/s1", State: "unchecked"},
					},
				},
			},
		},
		{
			ID: "projB",
			Children: []fixtureTreeNode{
				{
					ID: "projB/b1",
					Children: []fixtureTreeNode{
						{ID: "projB/b1/s1", State: "unchecked"},
					},
				},
			},
		},
	}
}

func loadNavigation(t *testing.T) navigationDocument {
	t.Helper()
	var doc navigationDocument
	dec := yaml.NewDecoder(bytes.NewReader(treeNavigationData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/tree_navigation.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("second document")
		}
		t.Fatalf("tree_navigation.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	return doc
}

// loadedTreeWithHeight builds a Tree over a static forest sized to a given
// visible-row budget and drives its load command to completion, so paging and
// scroll behaviour can be exercised at a realistic window height.
func loadedTreeWithHeight(t *testing.T, roots []*kit.TreeNode, height int) kit.Tree {
	t.Helper()
	tr := kit.NewTree(darkTheme(), staticSource{roots: roots})
	tr.SetSize(80, height)
	tr, cmd := tr.Load()
	msg := runCmd(cmd)
	if msg == nil {
		t.Fatal("load command produced no message")
	}
	tr, _ = tr.Update(msg)
	if tr.Loading() {
		t.Fatal("tree still loading after delivering its load result")
	}
	return tr
}

// orderedVisibleIDs reports the ids of the currently-visible rows, top to
// bottom, observed purely through cursor navigation: it rewinds to the first
// row with up-presses, then collects each row's id while down-presses still
// advance the cursor. It relies only on the long-established up/down gestures,
// never on the top/bottom/page gestures under test, so it is a neutral observer
// of which rows a collapse or expand left visible.
func orderedVisibleIDs(t *testing.T, tr kit.Tree) []string {
	t.Helper()
	for i := 0; i < 100000; i++ {
		before, ok := tr.CurrentNode()
		if !ok {
			return nil
		}
		next, _ := tr.Update(keyPress(t, "up"))
		after, _ := next.CurrentNode()
		tr = next
		if before.ID == after.ID {
			break
		}
	}
	var ids []string
	for i := 0; i < 100000; i++ {
		n, ok := tr.CurrentNode()
		if !ok {
			break
		}
		ids = append(ids, n.ID)
		next, _ := tr.Update(keyPress(t, "down"))
		nn, ok := next.CurrentNode()
		tr = next
		if !ok || nn.ID == n.ID {
			break
		}
	}
	return ids
}

// checkedIDs returns the ids of every node whose state is checked, in
// pre-order. It compares the CheckState value directly rather than its
// rendered string, so the key grep gate does not read the equality as a raw
// key-string comparison.
func checkedIDs(roots []*kit.TreeNode) []string {
	var out []string
	var visit func(*kit.TreeNode)
	visit = func(n *kit.TreeNode) {
		if n.State == kit.Checked {
			out = append(out, n.ID)
		}
		for _, c := range n.Children {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, x := range a {
		seen[x]++
	}
	for _, x := range b {
		seen[x]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

func equalOrdered(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestTree_Navigation replays each fixture's key sequence and asserts the
// observable cursor position, visible rows, and (where relevant) checked set -
// the mutation proof that top/bottom, paging, level and whole-forest
// expand/collapse, and select-under-project each move the tree the way the
// gesture promises.
func TestTree_Navigation(t *testing.T) {
	doc := loadNavigation(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			forest := c.Forest
			if len(forest) == 0 {
				forest = defaultNavigationForest()
			}
			tr := loadedTreeWithHeight(t, buildForest(t, forest), c.Height)
			for _, k := range c.Keys {
				tr, _ = tr.Update(keyPress(t, k))
			}
			node, ok := tr.CurrentNode()
			if !ok {
				t.Fatalf("no node under cursor after keys %v", c.Keys)
			}
			if node.ID != c.ExpectCursorID {
				t.Errorf("cursor id = %q, want %q (keys %v)", node.ID, c.ExpectCursorID, c.Keys)
			}
			if got := orderedVisibleIDs(t, tr); !equalOrdered(got, c.ExpectVisibleIDs) {
				t.Errorf("visible ids = %v, want %v (keys %v)", got, c.ExpectVisibleIDs, c.Keys)
			}
			if c.CheckSelection {
				if got := checkedIDs(tr.Roots()); !sameSet(got, c.ExpectChecked) {
					t.Errorf("checked ids = %v, want %v (keys %v)", got, c.ExpectChecked, c.Keys)
				}
			}
		})
	}
}
