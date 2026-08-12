package kit_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
)

//go:embed testdata/tree_affordances.yaml
var treeAffordanceData []byte

const (
	expectedTreeAffordanceCaseCount = 24
	expectedRejectedTreeKeyCount    = 4
)

type treeAffordanceStart string

const (
	treeAffordanceLoaded  treeAffordanceStart = ""
	treeAffordancePreLoad treeAffordanceStart = "pre-load"
	treeAffordanceEmpty   treeAffordanceStart = "empty"
)

func (s treeAffordanceStart) valid() bool {
	return s == treeAffordanceLoaded || s == treeAffordancePreLoad || s == treeAffordanceEmpty
}

type treeExpansionActionState string

const (
	treeExpansionActionUnspecified treeExpansionActionState = ""
	treeExpansionActionNone        treeExpansionActionState = "none"
	treeExpansionActionExpand      treeExpansionActionState = "expand"
	treeExpansionActionCollapse    treeExpansionActionState = "collapse"
)

func (s treeExpansionActionState) valid() bool {
	switch s {
	case treeExpansionActionUnspecified, treeExpansionActionNone, treeExpansionActionExpand, treeExpansionActionCollapse:
		return true
	default:
		return false
	}
}

type treeAffordanceCase struct {
	Name                    string                   `yaml:"name"`
	Start                   treeAffordanceStart      `yaml:"start"`
	Keys                    []string                 `yaml:"keys"`
	ExpectMode              string                   `yaml:"expectMode"`
	ExpectQuery             string                   `yaml:"expectQuery"`
	ExpectCursorID          string                   `yaml:"expectCursorID"`
	ExpectVisibleIDs        []string                 `yaml:"expectVisibleIDs"`
	ExpectOriginalIDs       []string                 `yaml:"expectOriginalIDs"`
	ExpectProjectedIDs      []string                 `yaml:"expectProjectedIDs"`
	ExpectOverflowTop       bool                     `yaml:"expectOverflowTop"`
	ExpectOverflowBottom    bool                     `yaml:"expectOverflowBottom"`
	ExpectCursorExpandGlyph string                   `yaml:"expectCursorExpandGlyph"`
	ExpectExpansionAction   treeExpansionActionState `yaml:"expectExpansionAction"`
	WantAvailable           []string                 `yaml:"wantAvailable"`
	WantUnavailable         []string                 `yaml:"wantUnavailable"`
	WantViewContains        []string                 `yaml:"wantViewContains"`
	WantViewMissing         []string                 `yaml:"wantViewMissing"`
}

type rejectedTreeKey struct {
	Name  string `yaml:"name"`
	Token string `yaml:"token"`
}

type boundedTreeSearchFixture struct {
	GeneratedSessionCount int      `yaml:"generatedSessionCount"`
	Query                 string   `yaml:"query"`
	ExpectedVisibleIDs    []string `yaml:"expectedVisibleIDs"`
	MaximumDurationMillis int      `yaml:"maximumDurationMillis"`
}

type treeAffordanceDocument struct {
	ExpectedCaseCount        int                      `yaml:"expectedCaseCount"`
	ExpectedRejectedKeyCount int                      `yaml:"expectedRejectedKeyCount"`
	Width                    int                      `yaml:"width"`
	Height                   int                      `yaml:"height"`
	Forest                   []fixtureTreeNode        `yaml:"forest"`
	RejectedKeys             []rejectedTreeKey        `yaml:"rejectedKeys"`
	Cases                    []treeAffordanceCase     `yaml:"cases"`
	BoundedSearch            boundedTreeSearchFixture `yaml:"boundedSearch"`
}

func treeFixtureValuesPresent(values ...[]string) bool {
	for _, group := range values {
		for _, value := range group {
			if strings.TrimSpace(value) == "" {
				return false
			}
		}
	}
	return true
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
	if doc.ExpectedCaseCount != expectedTreeAffordanceCaseCount || len(doc.Cases) != expectedTreeAffordanceCaseCount {
		return doc, fmt.Errorf("tree_affordances.yaml cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedTreeAffordanceCaseCount)
	}
	if doc.ExpectedRejectedKeyCount != expectedRejectedTreeKeyCount || len(doc.RejectedKeys) != expectedRejectedTreeKeyCount {
		return doc, fmt.Errorf("tree_affordances.yaml rejected keys: declared=%d actual=%d required=%d",
			doc.ExpectedRejectedKeyCount, len(doc.RejectedKeys), expectedRejectedTreeKeyCount)
	}
	if doc.Width <= 0 || doc.Height <= 0 || len(doc.Forest) == 0 {
		return doc, fmt.Errorf("tree_affordances.yaml must declare a positive region and non-empty forest")
	}
	if doc.BoundedSearch.GeneratedSessionCount != 4000 || strings.TrimSpace(doc.BoundedSearch.Query) == "" ||
		len(doc.BoundedSearch.ExpectedVisibleIDs) != 3 || doc.BoundedSearch.MaximumDurationMillis <= 0 ||
		!treeFixtureValuesPresent(doc.BoundedSearch.ExpectedVisibleIDs) {
		return doc, fmt.Errorf("tree_affordances.yaml must pin the complete 4000-session bounded-search path")
	}
	seenRejectedKeys := map[string]bool{}
	seenRejectedTokens := map[string]bool{}
	for _, rejected := range doc.RejectedKeys {
		if strings.TrimSpace(rejected.Name) == "" || strings.TrimSpace(rejected.Token) == "" ||
			seenRejectedKeys[rejected.Name] || seenRejectedTokens[rejected.Token] {
			return doc, fmt.Errorf("tree_affordances.yaml rejected key %q is empty or duplicated", rejected.Name)
		}
		seenRejectedKeys[rejected.Name] = true
		seenRejectedTokens[rejected.Token] = true
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] || !c.Start.valid() ||
			!treeFixtureValuesPresent(c.Keys, c.ExpectVisibleIDs, c.ExpectOriginalIDs, c.ExpectProjectedIDs,
				c.WantAvailable, c.WantUnavailable, c.WantViewContains, c.WantViewMissing) {
			return doc, fmt.Errorf("tree_affordances.yaml case %q is empty, duplicated, has an invalid start, or contains a blank fixture value", c.Name)
		}
		seen[c.Name] = true
		if c.ExpectMode == "" {
			return doc, fmt.Errorf("tree_affordances.yaml case %q must name its search lifecycle mode", c.Name)
		}
		if c.ExpectCursorExpandGlyph != "" && c.ExpectCursorExpandGlyph != "▾" && c.ExpectCursorExpandGlyph != "▸" {
			return doc, fmt.Errorf("tree_affordances.yaml case %q has invalid cursor expand glyph %q", c.Name, c.ExpectCursorExpandGlyph)
		}
		if !c.ExpectExpansionAction.valid() {
			return doc, fmt.Errorf("tree_affordances.yaml case %q has invalid expansion action state %q", c.Name, c.ExpectExpansionAction)
		}
		if (c.ExpectCursorExpandGlyph != "" || c.ExpectExpansionAction != treeExpansionActionUnspecified) && c.ExpectCursorID == "" {
			return doc, fmt.Errorf("tree_affordances.yaml case %q asserts cursor expansion state without a cursor ID", c.Name)
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

func TestTree_GlobalSearchAndOverflowAffordances(t *testing.T) {
	t.Parallel()
	doc := loadTreeAffordances(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			roots := buildForest(t, doc.Forest)
			var tr kit.Tree
			switch c.Start {
			case treeAffordanceLoaded:
				tr = loadedTreeWithHeight(t, roots, doc.Height)
			case treeAffordancePreLoad:
				tr = kit.NewTree(darkTheme(), staticSource{roots: roots})
			case treeAffordanceEmpty:
				tr = loadedTreeWithHeight(t, nil, doc.Height)
			default:
				t.Fatalf("unsupported tree start state %q", c.Start)
			}
			tr.SetSize(doc.Width, doc.Height)
			for _, pressed := range c.Keys {
				tr, _ = tr.Update(keyPress(t, pressed))
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
			assertTreeProjectionIdentity(t, roots, tr.VisibleRoots(), c.ExpectOriginalIDs, c.ExpectProjectedIDs)
			overflow := tr.Overflow()
			if overflow.Top != c.ExpectOverflowTop || overflow.Bottom != c.ExpectOverflowBottom {
				t.Errorf("overflow = %+v, want top=%t bottom=%t", overflow, c.ExpectOverflowTop, c.ExpectOverflowBottom)
			}

			available := actionSetByName(t, tr.AvailableActions())
			if c.ExpectExpansionAction != treeExpansionActionUnspecified {
				wantExpand := c.ExpectExpansionAction == treeExpansionActionExpand
				wantCollapse := c.ExpectExpansionAction == treeExpansionActionCollapse
				if got := available[keymap.ActionExpand.String()]; got != wantExpand {
					t.Errorf("expand action available = %t, want %t for state %q", got, wantExpand, c.ExpectExpansionAction)
				}
				if got := available[keymap.ActionCollapse.String()]; got != wantCollapse {
					t.Errorf("collapse action available = %t, want %t for state %q", got, wantCollapse, c.ExpectExpansionAction)
				}
			}
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
			if c.ExpectCursorExpandGlyph != "" {
				if got := renderedCursorExpandGlyph(t, tr, view); got != c.ExpectCursorExpandGlyph {
					t.Errorf("cursor expand glyph = %q, want %q:\n%s", got, c.ExpectCursorExpandGlyph, view)
				}
			}
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

func renderedCursorExpandGlyph(t *testing.T, tr kit.Tree, view string) string {
	t.Helper()
	lines := strings.Split(view, "\n")
	viewportRow := tr.Cursor() - tr.ViewportOffset()
	if viewportRow < 0 || viewportRow >= len(lines) {
		t.Fatalf("cursor viewport row %d is outside %d rendered rows", viewportRow, len(lines))
	}
	boxAt := strings.Index(lines[viewportRow], "[")
	if boxAt < 0 {
		t.Fatalf("cursor row has no checkbox: %q", lines[viewportRow])
	}
	prefix := strings.TrimRight(lines[viewportRow][:boxAt], " ")
	runes := []rune(prefix)
	if len(runes) == 0 {
		t.Fatalf("cursor row has no expansion glyph before its checkbox: %q", lines[viewportRow])
	}
	return string(runes[len(runes)-1])
}

func treeNodesByID(t *testing.T, roots []*kit.TreeNode) map[string]*kit.TreeNode {
	t.Helper()
	out := map[string]*kit.TreeNode{}
	var visit func(*kit.TreeNode)
	visit = func(node *kit.TreeNode) {
		if _, exists := out[node.ID]; exists {
			t.Fatalf("pointer-identity fixture requires unique IDs; duplicate %q", node.ID)
		}
		out[node.ID] = node
		for _, child := range node.Children {
			visit(child)
		}
	}
	for _, root := range roots {
		visit(root)
	}
	return out
}

func assertTreeProjectionIdentity(t *testing.T, canonical, visible []*kit.TreeNode, originalIDs, projectedIDs []string) {
	t.Helper()
	canonicalByID := treeNodesByID(t, canonical)
	visibleByID := treeNodesByID(t, visible)
	for _, id := range originalIDs {
		if canonicalByID[id] == nil || visibleByID[id] == nil || canonicalByID[id] != visibleByID[id] {
			t.Errorf("visible node %q does not retain its canonical pointer", id)
		}
	}
	for _, id := range projectedIDs {
		if canonicalByID[id] == nil || visibleByID[id] == nil || canonicalByID[id] == visibleByID[id] {
			t.Errorf("context node %q is not a shallow projection", id)
		}
	}
}

func TestTree_GlobalSearchHandlesFourThousandSessionsWithinBound(t *testing.T) {
	doc := loadTreeAffordances(t)
	fixture := doc.BoundedSearch
	sessions := make([]*kit.TreeNode, 0, fixture.GeneratedSessionCount)
	for index := 0; index < fixture.GeneratedSessionCount; index++ {
		label := fmt.Sprintf("recorded session %04d", index)
		if index == fixture.GeneratedSessionCount-1 {
			label = fixture.Query
		}
		sessions = append(sessions, &kit.TreeNode{ID: fmt.Sprintf("session-%04d", index), Label: label})
	}
	branch := &kit.TreeNode{ID: "large-branch", Label: "main", Children: sessions}
	project := &kit.TreeNode{ID: "large-project", Label: "large project", Children: []*kit.TreeNode{branch}}
	tr := loadedTreeWithHeight(t, []*kit.TreeNode{project}, 8)
	tr.SetSize(80, 8)

	started := time.Now()
	tr, _ = tr.Update(keyPress(t, "/"))
	for _, value := range fixture.Query {
		tr, _ = tr.Update(tea.KeyPressMsg{Code: value, Text: string(value)})
	}
	tr, _ = tr.Update(keyPress(t, "enter"))
	elapsed := time.Since(started)
	if elapsed > time.Duration(fixture.MaximumDurationMillis)*time.Millisecond {
		t.Fatalf("4000-session global search took %s, fixture bound is %dms", elapsed, fixture.MaximumDurationMillis)
	}
	if got := orderedVisibleIDs(t, tr); !equalOrdered(got, fixture.ExpectedVisibleIDs) {
		t.Fatalf("4000-session visible rows = %v, want %v", got, fixture.ExpectedVisibleIDs)
	}
	if tr.Roots()[0] != project || tr.VisibleRoots()[0] == project ||
		tr.VisibleRoots()[0].Children[0] == branch || tr.VisibleRoots()[0].Children[0].Children[0] != sessions[len(sessions)-1] {
		t.Fatal("4000-session projection did not preserve full roots, shallow ancestors, and canonical matching session")
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

func TestTreeAffordanceFixtureRejectsUnknownAndMultiGraphemeKeys(t *testing.T) {
	doc := loadTreeAffordances(t)
	for _, rejected := range doc.RejectedKeys {
		rejected := rejected
		t.Run(rejected.Name, func(t *testing.T) {
			if _, ok := parseFixtureKeyPress(rejected.Token); ok {
				t.Errorf("fixture key parser accepted rejected token %q", rejected.Token)
			}
		})
	}
}

func TestTreeAffordanceFixtureEnforcesRowCount(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedTreeAffordanceCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedTreeAffordanceCaseCount+1))
	mutated := bytes.Replace(treeAffordanceData, declared, changed, 1)
	if bytes.Equal(mutated, treeAffordanceData) {
		t.Fatal("tree affordance count mutation did not alter the fixture")
	}
	if _, err := decodeTreeAffordances(mutated); err == nil {
		t.Fatal("tree affordance fixture accepted a mismatched row-count guard")
	}
}
