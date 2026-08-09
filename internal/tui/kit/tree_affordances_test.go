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

//go:embed testdata/tree_affordances.yaml
var treeAffordanceData []byte

type treeAffordanceCase struct {
	Name                 string   `yaml:"name"`
	Keys                 []string `yaml:"keys"`
	ExpectScope          string   `yaml:"expectScope"`
	ExpectMode           string   `yaml:"expectMode"`
	ExpectQuery          string   `yaml:"expectQuery"`
	ExpectCursorID       string   `yaml:"expectCursorID"`
	ExpectVisibleIDs     []string `yaml:"expectVisibleIDs"`
	ExpectOverflowTop    bool     `yaml:"expectOverflowTop"`
	ExpectOverflowBottom bool     `yaml:"expectOverflowBottom"`
	WantAvailable        []string `yaml:"wantAvailable"`
	WantUnavailable      []string `yaml:"wantUnavailable"`
	WantViewContains     []string `yaml:"wantViewContains"`
	WantViewMissing      []string `yaml:"wantViewMissing"`
}

type treeAffordanceDocument struct {
	ExpectedCaseCount int                  `yaml:"expectedCaseCount"`
	Width             int                  `yaml:"width"`
	Height            int                  `yaml:"height"`
	Forest            []fixtureTreeNode    `yaml:"forest"`
	Cases             []treeAffordanceCase `yaml:"cases"`
}

func decodeTreeAffordances(data []byte) (treeAffordanceDocument, error) {
	var doc treeAffordanceDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/tree_affordances.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("tree_affordances.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != len(doc.Cases) || len(doc.Cases) == 0 {
		return doc, fmt.Errorf("tree_affordances.yaml expectedCaseCount=%d but has %d cases", doc.ExpectedCaseCount, len(doc.Cases))
	}
	if doc.Width <= 0 || doc.Height <= 0 || len(doc.Forest) == 0 {
		return doc, fmt.Errorf("tree_affordances.yaml must declare a positive region and non-empty forest")
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("tree_affordances.yaml case name %q is empty or duplicated", c.Name)
		}
		seen[c.Name] = true
		if c.ExpectScope == "" || c.ExpectMode == "" {
			return doc, fmt.Errorf("tree_affordances.yaml case %q must name scope and mode", c.Name)
		}
		if len(c.ExpectVisibleIDs)+len(c.WantAvailable)+len(c.WantUnavailable)+len(c.WantViewContains)+len(c.WantViewMissing) == 0 && c.ExpectCursorID == "" {
			return doc, fmt.Errorf("tree_affordances.yaml case %q has no observable assertion", c.Name)
		}
	}
	return doc, nil
}

func loadTreeAffordances(t *testing.T) treeAffordanceDocument {
	t.Helper()
	doc, err := decodeTreeAffordances(treeAffordanceData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func actionSetByName(t *testing.T, actions []keymap.ActionID) map[string]bool {
	t.Helper()
	out := make(map[string]bool, len(actions))
	for _, action := range actions {
		out[action.String()] = true
	}
	return out
}

func TestTree_ScopedSearchAndOverflowAffordances(t *testing.T) {
	t.Parallel()
	doc := loadTreeAffordances(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			tr := loadedTreeWithHeight(t, buildForest(t, doc.Forest), doc.Height)
			tr.SetSize(doc.Width, doc.Height)
			for _, pressed := range c.Keys {
				tr, _ = tr.Update(keyPress(t, pressed))
			}

			if got := tr.Scope().String(); got != c.ExpectScope {
				t.Errorf("scope = %q, want %q", got, c.ExpectScope)
			}
			state := tr.FilterState()
			if got := state.Mode.String(); got != c.ExpectMode {
				t.Errorf("filter mode = %q, want %q", got, c.ExpectMode)
			}
			if state.Query != c.ExpectQuery {
				t.Errorf("filter query = %q, want %q", state.Query, c.ExpectQuery)
			}
			if c.ExpectCursorID != "" {
				node, ok := tr.CurrentNode()
				if !ok || node.ID != c.ExpectCursorID {
					t.Errorf("cursor = %q (present=%t), want %q", nodeID(node), ok, c.ExpectCursorID)
				}
			}
			if len(c.ExpectVisibleIDs) > 0 {
				if got := orderedVisibleIDs(t, tr); !equalOrdered(got, c.ExpectVisibleIDs) {
					t.Errorf("visible rows = %v, want %v", got, c.ExpectVisibleIDs)
				}
			}
			overflow := tr.Overflow()
			if overflow.Top != c.ExpectOverflowTop || overflow.Bottom != c.ExpectOverflowBottom {
				t.Errorf("overflow = %+v, want top=%t bottom=%t", overflow, c.ExpectOverflowTop, c.ExpectOverflowBottom)
			}

			available := actionSetByName(t, tr.AvailableActions())
			for _, want := range c.WantAvailable {
				if !available[want] {
					t.Errorf("action %q is not available; got %v", want, available)
				}
			}
			for _, unwanted := range c.WantUnavailable {
				if available[unwanted] {
					t.Errorf("action %q is unexpectedly available; got %v", unwanted, available)
				}
			}

			view := stripANSI(tr.View())
			for _, want := range c.WantViewContains {
				if !strings.Contains(view, want) {
					t.Errorf("view must contain %q:\n%s", want, view)
				}
			}
			for _, missing := range c.WantViewMissing {
				if strings.Contains(view, missing) {
					t.Errorf("view must not contain %q:\n%s", missing, view)
				}
			}
		})
	}
}

func nodeID(node *kit.TreeNode) string {
	if node == nil {
		return ""
	}
	return node.ID
}

func TestTreeAffordanceFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), treeAffordanceData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeTreeAffordances(mutated); err == nil {
		t.Fatal("tree affordance fixture accepted an unknown field")
	}
}

func TestTreeAffordanceFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), treeAffordanceData...), []byte("\n---\n{}\n")...)
	if _, err := decodeTreeAffordances(mutated); err == nil {
		t.Fatal("tree affordance fixture accepted a trailing YAML document")
	}
}

func TestTreeAffordanceFixtureEnforcesRowCount(t *testing.T) {
	mutated := bytes.Replace(treeAffordanceData, []byte("expectedCaseCount: 13"), []byte("expectedCaseCount: 14"), 1)
	if _, err := decodeTreeAffordances(mutated); err == nil {
		t.Fatal("tree affordance fixture accepted a mismatched row-count guard")
	}
}
