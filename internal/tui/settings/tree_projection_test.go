package settings

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/keymap"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/tree_projection.yaml
var treeProjectionData []byte

const (
	expectedTreeProjectionSessionCount             = 4
	expectedTreeProjectionCaseCount                = 14
	expectedTreeProjectionAnchorMutationProbeCount = 1
	expectedTreeProjectionDuplicateProbeCount      = 1
)

type projectionSession struct {
	ID      string `yaml:"id"`
	Label   string `yaml:"label"`
	Harness string `yaml:"harness"`
	Remote  string `yaml:"remote"`
	Project string `yaml:"project"`
	Branch  string `yaml:"branch"`
}

type projectionRowAssertion struct {
	Label        string   `yaml:"label"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

type projectionCase struct {
	Name                    string                   `yaml:"name"`
	Keys                    []string                 `yaml:"keys"`
	Height                  int                      `yaml:"height"`
	WorkingSelection        *config.SelectionConfig  `yaml:"workingSelection"`
	ExpectPane              string                   `yaml:"expectPane"`
	ExpectCursorID          string                   `yaml:"expectCursorID"`
	ExpectPreviewID         string                   `yaml:"expectPreviewID"`
	ExpectMode              string                   `yaml:"expectMode"`
	ExpectQuery             string                   `yaml:"expectQuery"`
	ExpectSelected          []string                 `yaml:"expectSelected"`
	ExpectUnselected        []string                 `yaml:"expectUnselected"`
	CheckHiddenSelected     bool                     `yaml:"checkHiddenSelected"`
	ExpectHiddenSelected    int                      `yaml:"expectHiddenSelected"`
	CheckOverflow           bool                     `yaml:"checkOverflow"`
	ExpectOverflowTop       bool                     `yaml:"expectOverflowTop"`
	ExpectOverflowBottom    bool                     `yaml:"expectOverflowBottom"`
	AnchorMutationProbe     bool                     `yaml:"anchorMutationProbe"`
	DuplicateAnchorProbe    bool                     `yaml:"duplicateAnchorProbe"`
	SetupKeys               []string                 `yaml:"setupKeys"`
	FacetProjectionKeys     []string                 `yaml:"facetProjectionKeys"`
	FacetClearKeys          []string                 `yaml:"facetClearKeys"`
	ProjectionKeys          []string                 `yaml:"projectionKeys"`
	ClearKeys               []string                 `yaml:"clearKeys"`
	AnchorProjectID         string                   `yaml:"anchorProjectID"`
	SurvivingProjectID      string                   `yaml:"survivingProjectID"`
	DuplicateBranchID       string                   `yaml:"duplicateBranchID"`
	ExpectProjectedCursorID string                   `yaml:"expectProjectedCursorID"`
	ExpectMinimumOffset     int                      `yaml:"expectMinimumOffset"`
	WantVisibleRows         []string                 `yaml:"wantVisibleRows"`
	WantMissingRows         []string                 `yaml:"wantMissingRows"`
	WantViewContains        []string                 `yaml:"wantViewContains"`
	WantViewMissing         []string                 `yaml:"wantViewMissing"`
	RowAssertions           []projectionRowAssertion `yaml:"rowAssertions"`
}

type projectionDocument struct {
	Fixture                           string                 `yaml:"fixture"`
	ExpectedSessionCount              int                    `yaml:"expectedSessionCount"`
	ExpectedCaseCount                 int                    `yaml:"expectedCaseCount"`
	ExpectedAnchorMutationProbeCount  int                    `yaml:"expectedAnchorMutationProbeCount"`
	ExpectedDuplicateAnchorProbeCount int                    `yaml:"expectedDuplicateAnchorProbeCount"`
	Width                             int                    `yaml:"width"`
	Height                            int                    `yaml:"height"`
	SavedSelection                    config.SelectionConfig `yaml:"savedSelection"`
	ImportedSessionIDs                []string               `yaml:"importedSessionIDs"`
	Sessions                          []projectionSession    `yaml:"sessions"`
	Cases                             []projectionCase       `yaml:"cases"`
}

func projectionValuesPresent(values ...[]string) bool {
	for _, group := range values {
		for _, value := range group {
			if strings.TrimSpace(value) == "" {
				return false
			}
		}
	}
	return true
}

func decodeTreeProjection(data []byte) (projectionDocument, error) {
	var doc projectionDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/tree_projection.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("tree_projection.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedSessionCount != expectedTreeProjectionSessionCount || len(doc.Sessions) != expectedTreeProjectionSessionCount {
		return doc, fmt.Errorf("tree_projection.yaml sessions: declared=%d actual=%d required=%d",
			doc.ExpectedSessionCount, len(doc.Sessions), expectedTreeProjectionSessionCount)
	}
	if doc.ExpectedCaseCount != expectedTreeProjectionCaseCount || len(doc.Cases) != expectedTreeProjectionCaseCount {
		return doc, fmt.Errorf("tree_projection.yaml cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedTreeProjectionCaseCount)
	}
	if doc.Fixture == "" || doc.Width <= 0 || doc.Height <= 0 || !doc.SavedSelection.Mode.IsValid() {
		return doc, fmt.Errorf("tree_projection.yaml must declare a fixture, positive region, and valid saved mode")
	}
	seenSessions := map[string]bool{}
	for _, session := range doc.Sessions {
		if session.ID == "" || session.Label == "" || session.Harness == "" || session.Branch == "" || seenSessions[session.ID] {
			return doc, fmt.Errorf("tree_projection.yaml contains an invalid or duplicate session: %#v", session)
		}
		seenSessions[session.ID] = true
	}
	seenCases := map[string]bool{}
	anchorMutationProbes := 0
	duplicateAnchorProbes := 0
	for _, c := range doc.Cases {
		if c.Name == "" || c.ExpectMode == "" || seenCases[c.Name] ||
			!projectionValuesPresent(c.Keys, c.ExpectSelected, c.ExpectUnselected, c.WantVisibleRows, c.WantMissingRows, c.WantViewContains, c.WantViewMissing) {
			return doc, fmt.Errorf("tree_projection.yaml contains an invalid or duplicate case: %#v", c)
		}
		seenCases[c.Name] = true
		if c.AnchorMutationProbe {
			anchorMutationProbes++
		}
		if c.DuplicateAnchorProbe {
			duplicateAnchorProbes++
			if len(c.SetupKeys) == 0 || len(c.FacetProjectionKeys) == 0 || len(c.FacetClearKeys) == 0 ||
				len(c.ProjectionKeys) == 0 || len(c.ClearKeys) == 0 ||
				!projectionValuesPresent(c.SetupKeys, c.FacetProjectionKeys, c.FacetClearKeys, c.ProjectionKeys, c.ClearKeys) || c.AnchorProjectID == "" ||
				c.SurvivingProjectID == "" || c.DuplicateBranchID == "" || c.ExpectProjectedCursorID == "" || c.ExpectMinimumOffset < 1 {
				return doc, fmt.Errorf("tree_projection.yaml duplicate-anchor probe %q is incomplete", c.Name)
			}
		}
		if c.WorkingSelection != nil && !c.WorkingSelection.Mode.IsValid() {
			return doc, fmt.Errorf("tree_projection.yaml case %q has invalid working selection mode %q", c.Name, c.WorkingSelection.Mode)
		}
		if len(c.ExpectSelected)+len(c.ExpectUnselected)+len(c.WantVisibleRows)+len(c.WantMissingRows)+len(c.WantViewContains)+len(c.WantViewMissing)+len(c.RowAssertions) == 0 && c.ExpectCursorID == "" && c.ExpectPreviewID == "" && !c.CheckOverflow {
			return doc, fmt.Errorf("tree_projection.yaml case %q has no observable assertion", c.Name)
		}
		for _, row := range c.RowAssertions {
			if strings.TrimSpace(row.Label) == "" || len(row.WantContains)+len(row.WantMissing) == 0 ||
				!projectionValuesPresent(row.WantContains, row.WantMissing) {
				return doc, fmt.Errorf("tree_projection.yaml case %q has an empty row assertion", c.Name)
			}
		}
	}
	if doc.ExpectedAnchorMutationProbeCount != expectedTreeProjectionAnchorMutationProbeCount || anchorMutationProbes != expectedTreeProjectionAnchorMutationProbeCount {
		return doc, fmt.Errorf("tree_projection.yaml anchor mutation probes: declared=%d actual=%d required=%d",
			doc.ExpectedAnchorMutationProbeCount, anchorMutationProbes, expectedTreeProjectionAnchorMutationProbeCount)
	}
	if doc.ExpectedDuplicateAnchorProbeCount != expectedTreeProjectionDuplicateProbeCount || duplicateAnchorProbes != expectedTreeProjectionDuplicateProbeCount {
		return doc, fmt.Errorf("tree_projection.yaml duplicate-anchor probes: declared=%d actual=%d required=%d",
			doc.ExpectedDuplicateAnchorProbeCount, duplicateAnchorProbes, expectedTreeProjectionDuplicateProbeCount)
	}
	return doc, nil
}

func loadTreeProjection(t *testing.T) projectionDocument {
	t.Helper()
	doc, err := decodeTreeProjection(treeProjectionData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

type projectionSource struct {
	fixture  string
	imported map[string]bool
}

func (s projectionSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	roots, err := scannerfix.Load(s.fixture)
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		walkNodes(root, func(node *kit.TreeNode) {
			if !s.imported[node.ID] {
				return
			}
			if node.Meta == nil {
				node.Meta = map[string]string{}
			}
			node.Meta[MetaIngested] = MetaIngestedValue
		})
	}
	return roots, nil
}

type projectionPreviewSource struct{}

type projectionPreviewBody string

func (b projectionPreviewBody) Render(_ int) string { return string(b) }

func (projectionPreviewSource) Body(id string) (kit.PreviewBody, error) {
	return projectionPreviewBody("preview for " + id), nil
}

func mountedProjectionFlow(t *testing.T, doc projectionDocument, c projectionCase) (Flow, *Draft, *treeField) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Selection = doc.SavedSelection
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed projection config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read projection config: %v", err)
	}
	loaded, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse projection config: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("NewDraft: %v", err)
	}
	if c.WorkingSelection != nil {
		draft.Working().Selection = *c.WorkingSelection
	}
	imported := map[string]bool{}
	for _, id := range doc.ImportedSessionIDs {
		imported[id] = true
	}
	field := Tree("selection", "", selectionAccessor(), projectionSource{fixture: doc.Fixture, imported: imported},
		WithFacet(MetaHarness, "harness"), WithPreviewBodySource(projectionPreviewSource{}),
		WithPreviewRatio(0.5), WithDraftSelectionState())
	reg := Registry{Sections: []Section{{Key: "selection", Title: "select sessions", Fields: []Field{field}}}}
	flow := NewFlow(theme.New(theme.ModeDark), reg, draft)
	height := doc.Height
	if c.Height > 0 {
		height = c.Height
	}
	flow.SetSize(doc.Width, height)
	flow = drainInit(flow)
	return flow, draft, field.(*treeField)
}

func projectionKey(t *testing.T, value string) tea.KeyPressMsg {
	t.Helper()
	switch value {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+h":
		return tea.KeyPressMsg{Code: 'h', Mod: tea.ModCtrl}
	case "ctrl+l":
		return tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}
	default:
		runes := []rune(value)
		if len(runes) != 1 {
			t.Fatalf("projection fixture names unsupported key %q", value)
		}
		return tea.KeyPressMsg{Code: runes[0], Text: value}
	}
}

func driveProjectionFlow(t *testing.T, flow Flow, keys []string) Flow {
	t.Helper()
	for _, value := range keys {
		var cmd tea.Cmd
		flow, cmd = flow.Update(projectionKey(t, value))
		for _, msg := range runAll(cmd) {
			flow, _ = flow.Update(msg)
		}
	}
	return flow
}

func lineContaining(view, label string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	return ""
}

func projectionSelects(sel config.SelectionConfig, session projectionSession) bool {
	match := config.CompileSelectionMatcher(sel).MatchDiscovery(
		ingest.Harness(session.Harness), session.Remote, session.Project, session.Branch,
		ingest.SessionID(session.ID), sel.AutoIngestNewBranches)
	return match == ingest.BranchMatchYes
}

func projectionHiddenSelectedCount(full, visible []*kit.TreeNode) int {
	visibleLeaves := map[*kit.TreeNode]bool{}
	for _, root := range visible {
		walkNodes(root, func(node *kit.TreeNode) {
			if len(node.Children) == 0 {
				visibleLeaves[node] = true
			}
		})
	}
	hidden := 0
	for _, root := range full {
		walkNodes(root, func(node *kit.TreeNode) {
			if len(node.Children) == 0 && node.State == kit.Checked && !visibleLeaves[node] {
				hidden++
			}
		})
	}
	return hidden
}

func assertSimplifiedSelectionChrome(t *testing.T, view string) {
	t.Helper()
	if got := strings.Count(view, "search:"); got != 1 {
		t.Errorf("selection field renders %d search bars, want exactly one:\n%s", got, view)
	}
	for _, forbidden := range []string{
		"transcripts", "scope:", "previous scope", "next scope", "search scope",
		"tracked =", "imported =", "selected sessions:", "hidden by filters:", "view only:",
	} {
		if strings.Contains(view, forbidden) {
			t.Errorf("simplified selection field renders removed text %q:\n%s", forbidden, view)
		}
	}
}

func projectionSessionByID(t *testing.T, sessions []projectionSession, id string) projectionSession {
	t.Helper()
	for _, session := range sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("projection fixture has no session %q", id)
	return projectionSession{}
}

func TestFlow_TreeProjectionPreservesCanonicalSelection(t *testing.T) {
	doc := loadTreeProjection(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			flow, draft, field := mountedProjectionFlow(t, doc, c)
			flow = driveProjectionFlow(t, flow, c.Keys)
			view := stripANSIForSettings(flow.View())
			assertSimplifiedSelectionChrome(t, view)

			state := field.tree.FilterState()
			if got := state.Mode.String(); got != c.ExpectMode || state.Query != c.ExpectQuery {
				t.Errorf("filter = %s/%q, want %s/%q", got, state.Query, c.ExpectMode, c.ExpectQuery)
			}
			if c.ExpectPane != "" && field.split.ActivePane().String() != c.ExpectPane {
				t.Errorf("active pane = %s, want %s", field.split.ActivePane(), c.ExpectPane)
			}
			if c.ExpectCursorID != "" {
				node, ok := field.tree.CurrentNode()
				if !ok || node.ID != c.ExpectCursorID {
					got := ""
					if node != nil {
						got = node.ID
					}
					t.Errorf("cursor = %q (present=%t), want %q", got, ok, c.ExpectCursorID)
				}
			}
			if c.ExpectPreviewID != "" {
				id, ok := field.split.HighlightedID()
				if !ok || id != c.ExpectPreviewID {
					t.Errorf("preview identity = %q (present=%t), want %q", id, ok, c.ExpectPreviewID)
				}
			}
			if c.CheckOverflow {
				overflow := field.tree.Overflow()
				if overflow.Top != c.ExpectOverflowTop || overflow.Bottom != c.ExpectOverflowBottom {
					t.Errorf("overflow = %+v, want top=%t bottom=%t", overflow, c.ExpectOverflowTop, c.ExpectOverflowBottom)
				}
			}

			selection := draft.Working().Selection
			for _, id := range c.ExpectSelected {
				if !projectionSelects(selection, projectionSessionByID(t, doc.Sessions, id)) {
					t.Errorf("current selection does not include %q: %#v", id, selection)
				}
			}
			for _, id := range c.ExpectUnselected {
				if projectionSelects(selection, projectionSessionByID(t, doc.Sessions, id)) {
					t.Errorf("current selection unexpectedly includes %q: %#v", id, selection)
				}
			}
			if c.CheckHiddenSelected {
				if got := projectionHiddenSelectedCount(field.full, field.tree.VisibleRoots()); got != c.ExpectHiddenSelected {
					t.Errorf("hidden selected leaves = %d, want %d", got, c.ExpectHiddenSelected)
				}
			}

			for _, want := range c.WantVisibleRows {
				if !strings.Contains(view, want) {
					t.Errorf("visible row %q missing:\n%s", want, view)
				}
			}
			for _, missing := range c.WantMissingRows {
				if strings.Contains(view, missing) {
					t.Errorf("filtered row %q is still visible:\n%s", missing, view)
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
			for _, assertion := range c.RowAssertions {
				line := lineContaining(view, assertion.Label)
				if line == "" {
					t.Errorf("row %q is absent:\n%s", assertion.Label, view)
					continue
				}
				for _, want := range assertion.WantContains {
					if !strings.Contains(line, want) {
						t.Errorf("row %q must contain %q: %s", assertion.Label, want, line)
					}
				}
				for _, missing := range assertion.WantMissing {
					if strings.Contains(line, missing) {
						t.Errorf("row %q must not contain %q: %s", assertion.Label, missing, line)
					}
				}
			}
		})
	}
}

func TestTreeProjectionFixtureAnchorProbeDetectsCursorReset(t *testing.T) {
	doc := loadTreeProjection(t)
	probes := 0
	for _, c := range doc.Cases {
		if !c.AnchorMutationProbe {
			continue
		}
		probes++
		if len(c.Keys) < 2 || c.Keys[len(c.Keys)-1] != "f" || c.ExpectCursorID == "" {
			t.Fatalf("anchor mutation probe %q must navigate to a named cursor before one facet key", c.Name)
		}
		flow, _, field := mountedProjectionFlow(t, doc, c)
		flow = driveProjectionFlow(t, flow, c.Keys[:len(c.Keys)-1])
		_ = flow
		values := field.facetValues()
		if len(values) == 0 {
			t.Fatalf("anchor mutation probe %q has no facet value", c.Name)
		}
		reset := field.tree.WithRoots(pruneForest(field.full, field.facetKey, values[0]))
		node, ok := reset.CurrentNode()
		if ok && node.ID == c.ExpectCursorID {
			t.Fatalf("anchor mutation probe %q would not catch reset-to-first-row projection", c.Name)
		}
	}
	if probes != expectedTreeProjectionAnchorMutationProbeCount {
		t.Fatalf("anchor mutation probes = %d, want %d", probes, expectedTreeProjectionAnchorMutationProbeCount)
	}
}

func projectionBranchUnder(t *testing.T, roots []*kit.TreeNode, projectID, branchID string) *kit.TreeNode {
	t.Helper()
	for _, root := range roots {
		if root.ID != projectID {
			continue
		}
		for _, branch := range root.Children {
			if branch.ID == branchID {
				return branch
			}
		}
		t.Fatalf("project %q has no branch %q", projectID, branchID)
	}
	t.Fatalf("projection forest has no project %q", projectID)
	return nil
}

func projectionHasBranchUnder(roots []*kit.TreeNode, projectID, branchID string) bool {
	for _, root := range roots {
		if root.ID != projectID {
			continue
		}
		for _, branch := range root.Children {
			if branch.ID == branchID {
				return true
			}
		}
	}
	return false
}

func projectionNodeID(node *kit.TreeNode) string {
	if node == nil {
		return ""
	}
	return node.ID
}

func projectionActionAvailable(actions []keymap.ActionID, target keymap.ActionID) bool {
	for _, action := range actions {
		if action == target {
			return true
		}
	}
	return false
}

func TestTreeProjectionDuplicateBranchRestoresExactCanonicalAnchor(t *testing.T) {
	doc := loadTreeProjection(t)
	probes := 0
	for _, c := range doc.Cases {
		if !c.DuplicateAnchorProbe {
			continue
		}
		probes++
		t.Run(c.Name, func(t *testing.T) {
			flow, draft, field := mountedProjectionFlow(t, doc, c)
			canonicalRoots := field.tree.Roots()
			anchoredBranch := projectionBranchUnder(t, canonicalRoots, c.AnchorProjectID, c.DuplicateBranchID)
			survivingBranch := projectionBranchUnder(t, canonicalRoots, c.SurvivingProjectID, c.DuplicateBranchID)
			if anchoredBranch == survivingBranch || anchoredBranch.ID != survivingBranch.ID {
				t.Fatal("duplicate-branch fixture does not contain distinct canonical pointers with one shared ID")
			}

			flow = driveProjectionFlow(t, flow, c.SetupKeys)
			current, ok := field.tree.CurrentNode()
			if !ok || current != anchoredBranch {
				t.Fatalf("setup cursor=%p/%v want anchored branch pointer=%p", current, ok, anchoredBranch)
			}
			originalOffset := field.tree.ViewportOffset()
			if originalOffset < c.ExpectMinimumOffset {
				t.Fatalf("setup viewport offset=%d want at least %d", originalOffset, c.ExpectMinimumOffset)
			}
			originalCollapsed := projectionActionAvailable(field.tree.AvailableActions(), keymap.ActionExpand)
			if !originalCollapsed {
				t.Fatal("setup did not collapse the anchored duplicate branch")
			}
			previewBefore, previewOK := field.split.HighlightedID()
			if !previewOK || previewBefore != c.DuplicateBranchID {
				t.Fatalf("setup preview=%q/%t want duplicate branch %q", previewBefore, previewOK, c.DuplicateBranchID)
			}
			selectionBefore, err := yaml.Marshal(draft.Working().Selection)
			if err != nil {
				t.Fatalf("marshal selection before duplicate projection: %v", err)
			}

			assertProjected := func(route string) {
				t.Helper()
				visibleRoots := field.tree.VisibleRoots()
				if projectionHasBranchUnder(visibleRoots, c.AnchorProjectID, c.DuplicateBranchID) {
					t.Fatalf("%s projection retained duplicate branch under hidden project %q", route, c.AnchorProjectID)
				}
				if !projectionHasBranchUnder(visibleRoots, c.SurvivingProjectID, c.DuplicateBranchID) {
					t.Fatalf("%s projection dropped duplicate branch under surviving project %q", route, c.SurvivingProjectID)
				}
				projected, ok := field.tree.CurrentNode()
				if !ok || projectionNodeID(projected) != c.ExpectProjectedCursorID || projected == survivingBranch {
					t.Fatalf("%s projected cursor=%q/%p want project %q, never surviving duplicate %p",
						route, projectionNodeID(projected), projected, c.ExpectProjectedCursorID, survivingBranch)
				}
				projectedPreview, projectedPreviewOK := field.split.HighlightedID()
				if !projectedPreviewOK || projectedPreview != c.ExpectProjectedCursorID {
					t.Fatalf("%s projected preview=%q/%t want %q", route, projectedPreview, projectedPreviewOK, c.ExpectProjectedCursorID)
				}
			}
			assertRestored := func(route string) {
				t.Helper()
				restored, ok := field.tree.CurrentNode()
				if !ok || restored != anchoredBranch {
					t.Fatalf("%s restored cursor=%q/%p/%t want exact anchored branch pointer %q/%p",
						route, projectionNodeID(restored), restored, ok, anchoredBranch.ID, anchoredBranch)
				}
				if got := field.tree.ViewportOffset(); got != originalOffset {
					t.Fatalf("%s restored viewport offset=%d want exact setup offset=%d", route, got, originalOffset)
				}
				if collapsed := projectionActionAvailable(field.tree.AvailableActions(), keymap.ActionExpand); collapsed != originalCollapsed {
					t.Fatalf("%s restored expansion state=%t want setup state=%t", route, collapsed, originalCollapsed)
				}
				previewAfter, previewAfterOK := field.split.HighlightedID()
				if !previewAfterOK || previewAfter != previewBefore {
					t.Fatalf("%s restored preview=%q/%t want setup preview=%q", route, previewAfter, previewAfterOK, previewBefore)
				}
				selectionAfter, err := yaml.Marshal(draft.Working().Selection)
				if err != nil {
					t.Fatalf("marshal selection after %s duplicate projection: %v", route, err)
				}
				if !bytes.Equal(selectionAfter, selectionBefore) {
					t.Fatalf("%s duplicate projection changed selection\nbefore=%s\nafter=%s", route, selectionBefore, selectionAfter)
				}
			}

			flow = driveProjectionFlow(t, flow, c.FacetProjectionKeys)
			assertProjected("facet")
			flow = driveProjectionFlow(t, flow, c.FacetClearKeys)
			assertRestored("facet")
			flow = driveProjectionFlow(t, flow, c.ProjectionKeys)
			assertProjected("text")
			flow = driveProjectionFlow(t, flow, c.ClearKeys)
			_ = flow
			assertRestored("text")
		})
	}
	if probes != expectedTreeProjectionDuplicateProbeCount {
		t.Fatalf("duplicate-anchor probes=%d want=%d", probes, expectedTreeProjectionDuplicateProbeCount)
	}
}

func stripANSIForSettings(value string) string {
	var out strings.Builder
	inEscape := false
	for _, r := range value {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if r == 'm' {
				inEscape = false
			}
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func TestTreeProjectionFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), treeProjectionData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted an unknown field")
	}
}

func TestTreeProjectionFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), treeProjectionData...), []byte("\n---\n{}\n")...)
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted a trailing document")
	}
}

func TestTreeProjectionFixtureEnforcesRowCount(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedTreeProjectionCaseCount))
	changed := []byte(fmt.Sprintf("expectedCaseCount: %d", expectedTreeProjectionCaseCount+1))
	mutated := bytes.Replace(treeProjectionData, declared, changed, 1)
	if bytes.Equal(mutated, treeProjectionData) {
		t.Fatal("tree projection case-count mutation did not alter the fixture")
	}
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted a mismatched row-count guard")
	}
}

func TestTreeProjectionFixtureEnforcesSessionCount(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedSessionCount: %d", expectedTreeProjectionSessionCount))
	changed := []byte(fmt.Sprintf("expectedSessionCount: %d", expectedTreeProjectionSessionCount+1))
	mutated := bytes.Replace(treeProjectionData, declared, changed, 1)
	if bytes.Equal(mutated, treeProjectionData) {
		t.Fatal("tree projection session-count mutation did not alter the fixture")
	}
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted a mismatched session-count guard")
	}
}

func TestTreeProjectionFixtureEnforcesAnchorMutationProbeCount(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedAnchorMutationProbeCount: %d", expectedTreeProjectionAnchorMutationProbeCount))
	changed := []byte(fmt.Sprintf("expectedAnchorMutationProbeCount: %d", expectedTreeProjectionAnchorMutationProbeCount+1))
	mutated := bytes.Replace(treeProjectionData, declared, changed, 1)
	if bytes.Equal(mutated, treeProjectionData) {
		t.Fatal("tree projection anchor-probe count mutation did not alter the fixture")
	}
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted a mismatched anchor-probe count")
	}
}

func TestTreeProjectionFixtureEnforcesDuplicateAnchorProbeCount(t *testing.T) {
	declared := []byte(fmt.Sprintf("expectedDuplicateAnchorProbeCount: %d", expectedTreeProjectionDuplicateProbeCount))
	changed := []byte(fmt.Sprintf("expectedDuplicateAnchorProbeCount: %d", expectedTreeProjectionDuplicateProbeCount+1))
	mutated := bytes.Replace(treeProjectionData, declared, changed, 1)
	if bytes.Equal(mutated, treeProjectionData) {
		t.Fatal("tree projection duplicate-anchor count mutation did not alter the fixture")
	}
	if _, err := decodeTreeProjection(mutated); err == nil {
		t.Fatal("tree projection fixture accepted a mismatched duplicate-anchor count")
	}
}
