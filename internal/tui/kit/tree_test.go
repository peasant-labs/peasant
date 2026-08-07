package kit_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/tree_propagation.yaml
var treePropagationData []byte

// fixtureTreeNode mirrors the propagation fixture's node shape. State is a
// leaf's authoritative TriState name; interior nodes derive theirs from rollup
// on load, so their fixture State (if any) is ignored.
type fixtureTreeNode struct {
	ID       string            `yaml:"id"`
	State    string            `yaml:"state"`
	Children []fixtureTreeNode `yaml:"children"`
}

type propagationAction struct {
	Toggle    string `yaml:"toggle"`
	SelectAll bool   `yaml:"selectAll"`
}

type propagationCase struct {
	Name    string              `yaml:"name"`
	Forest  []fixtureTreeNode   `yaml:"forest"`
	Actions []propagationAction `yaml:"actions"`
	Expect  map[string]string   `yaml:"expect"`
}

type propagationDocument struct {
	ExpectedCaseCount int               `yaml:"expectedCaseCount"`
	Cases             []propagationCase `yaml:"cases"`
}

func loadPropagation(t *testing.T) propagationDocument {
	t.Helper()
	var doc propagationDocument
	dec := yaml.NewDecoder(bytes.NewReader(treePropagationData))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode testdata/tree_propagation.yaml: %v", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("second document")
		}
		t.Fatalf("tree_propagation.yaml must hold exactly one document: %v", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	return doc
}

func parseFixtureState(t *testing.T, s string) kit.TriState {
	t.Helper()
	switch s {
	case "unchecked", "":
		return kit.Unchecked
	case "partial":
		return kit.Partial
	case "checked":
		return kit.Checked
	case "conflict":
		return kit.Conflict
	default:
		t.Fatalf("unknown fixture state %q", s)
		return kit.Unchecked
	}
}

func buildForest(t *testing.T, nodes []fixtureTreeNode) []*kit.TreeNode {
	t.Helper()
	var out []*kit.TreeNode
	for _, n := range nodes {
		node := &kit.TreeNode{ID: n.ID, State: parseFixtureState(t, n.State), Label: n.ID}
		node.Children = buildForest(t, n.Children)
		out = append(out, node)
	}
	return out
}

// staticSource is a kit.TreeSource returning a fixed forest immediately.
type staticSource struct{ roots []*kit.TreeNode }

func (s staticSource) Load(context.Context) ([]*kit.TreeNode, error) { return s.roots, nil }

// loadedTree builds a Tree over a static forest and drives its load command to
// completion synchronously, returning the loaded tree.
func loadedTree(t *testing.T, roots []*kit.TreeNode) kit.Tree {
	t.Helper()
	tr := kit.NewTree(darkTheme(), staticSource{roots: roots})
	tr.SetSize(80, 40)
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

// nodeStates walks a tree's roots collecting id -> state name.
func nodeStates(roots []*kit.TreeNode) map[string]string {
	out := map[string]string{}
	var visit func(*kit.TreeNode)
	visit = func(n *kit.TreeNode) {
		out[n.ID] = n.State.String()
		for _, c := range n.Children {
			visit(c)
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return out
}

// toggleByID moves the cursor down to the node with id and presses select.
func toggleByID(t *testing.T, tr kit.Tree, id string) kit.Tree {
	t.Helper()
	// Cursor starts at 0 (top); walk down until the current node matches.
	for i := 0; i < 10000; i++ {
		node, ok := tr.CurrentNode()
		if !ok {
			t.Fatalf("ran off the tree looking for id %q", id)
		}
		if node.ID == id {
			tr, _ = tr.Update(keyPress(t, "space"))
			return tr
		}
		tr, _ = tr.Update(keyPress(t, "down"))
	}
	t.Fatalf("did not reach id %q", id)
	return tr
}

// TestTree_Propagation runs the fixture-driven tri-state propagation cases.
func TestTree_Propagation(t *testing.T) {
	doc := loadPropagation(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			tr := loadedTree(t, buildForest(t, c.Forest))
			for _, a := range c.Actions {
				switch {
				case a.Toggle != "":
					tr = toggleByID(t, tr, a.Toggle)
				case a.SelectAll:
					tr, _ = tr.Update(keyPress(t, "a"))
				default:
					t.Fatalf("case %q has an empty action", c.Name)
				}
			}
			got := nodeStates(tr.Roots())
			if len(got) != len(c.Expect) {
				t.Fatalf("forest has %d nodes but expect map has %d - expect must be exhaustive", len(got), len(c.Expect))
			}
			for id, want := range c.Expect {
				if got[id] != want {
					t.Errorf("node %q: state=%s want %s", id, got[id], want)
				}
			}
		})
	}
}

// TestTree_ExpandCollapseHidesChildren proves collapse removes descendants from
// the visible rows and expand restores them.
func TestTree_ExpandCollapseHidesChildren(t *testing.T) {
	roots := buildForest(t, []fixtureTreeNode{{
		ID:       "p",
		Children: []fixtureTreeNode{{ID: "a", State: "unchecked"}, {ID: "b", State: "unchecked"}},
	}})
	tr := loadedTree(t, roots)
	// Cursor is on "p"; collapse it.
	tr, _ = tr.Update(keyPress(t, "left"))
	// After collapse, moving down should not reach a child.
	tr, _ = tr.Update(keyPress(t, "down"))
	node, ok := tr.CurrentNode()
	if !ok || node.ID != "p" {
		t.Fatalf("after collapse the only visible row should be p; got %v/%v", node, ok)
	}
	// Expand restores children.
	tr, _ = tr.Update(keyPress(t, "right"))
	tr, _ = tr.Update(keyPress(t, "down"))
	node, ok = tr.CurrentNode()
	if !ok || node.ID != "a" {
		t.Fatalf("after expand a child should be reachable; got %v/%v", node, ok)
	}
}

// TestTree_HasConflict reflects a Conflict node without persisting it.
func TestTree_HasConflict(t *testing.T) {
	clean := loadedTree(t, buildForest(t, []fixtureTreeNode{{
		ID: "p", Children: []fixtureTreeNode{{ID: "a", State: "checked"}},
	}}))
	if clean.HasConflict() {
		t.Fatal("clean tree reports a conflict")
	}
	conflicted := loadedTree(t, buildForest(t, []fixtureTreeNode{{
		ID: "p", Children: []fixtureTreeNode{{ID: "gone", State: "conflict"}},
	}}))
	if !conflicted.HasConflict() {
		t.Fatal("tree with a Conflict node should report HasConflict")
	}
}

// TestTriState_ClosedSet guards the enum's String/IsValid surface.
func TestTriState_ClosedSet(t *testing.T) {
	for _, s := range []kit.TriState{kit.Unchecked, kit.Partial, kit.Checked, kit.Conflict} {
		if !s.IsValid() {
			t.Errorf("%v should be valid", s)
		}
		name := s.String()
		if name == "unknown" {
			t.Errorf("%v stringifies to unknown", s)
		}
	}
	var bogus kit.TriState = 99
	if bogus.IsValid() {
		t.Error("out-of-range TriState should be invalid")
	}
	bogusName := bogus.String()
	if bogusName != "unknown" {
		t.Errorf("out-of-range TriState should stringify to unknown, got %q", bogusName)
	}
}
