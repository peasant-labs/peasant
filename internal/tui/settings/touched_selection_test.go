package settings

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"reflect"
	"testing"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/touched_selection.yaml
var touchedSelectionData []byte

type touchedSelectionOperation string

const (
	touchedProjectOn  touchedSelectionOperation = "project-on"
	touchedProjectOff touchedSelectionOperation = "project-off"
	touchedBranchOn   touchedSelectionOperation = "branch-on"
	touchedBranchOff  touchedSelectionOperation = "branch-off"
	touchedSessionOn  touchedSelectionOperation = "session-on"
	touchedSessionOff touchedSelectionOperation = "session-off"
)

type touchedSelectionDocument struct {
	ExpectedCaseCount      int                    `yaml:"expectedCaseCount"`
	ExpectedFieldCaseCount int                    `yaml:"expectedFieldCaseCount"`
	ExpectedNames          []string               `yaml:"expectedNames"`
	ExpectedFieldNames     []string               `yaml:"expectedFieldNames"`
	FieldCases             []touchedFieldCase     `yaml:"fieldCases"`
	Cases                  []touchedSelectionCase `yaml:"cases"`
}

type touchedSelectionCase struct {
	Name                string                    `yaml:"name"`
	Operation           touchedSelectionOperation `yaml:"operation"`
	Current             config.SelectionConfig    `yaml:"current"`
	Scope               touchedScopeFixture       `yaml:"scope"`
	Expected            config.SelectionConfig    `yaml:"expected"`
	ExpectErrorContains string                    `yaml:"expectErrorContains"`
}

type touchedScopeFixture struct {
	Harness         string                  `yaml:"harness"`
	ClonePath       string                  `yaml:"clonePath"`
	GitRemote       string                  `yaml:"gitRemote"`
	ProjectName     string                  `yaml:"projectName"`
	Branch          string                  `yaml:"branch"`
	SessionID       string                  `yaml:"sessionId"`
	VisibleSessions []touchedSessionFixture `yaml:"visibleSessions"`
}

type touchedSessionFixture struct {
	Branch    string `yaml:"branch"`
	SessionID string `yaml:"sessionId"`
}

type touchedFieldKey string

const (
	touchedFieldDown        touchedFieldKey = "down"
	touchedFieldToggle      touchedFieldKey = "toggle"
	touchedFieldSelectUnder touchedFieldKey = "select-under"
	touchedFieldFilter      touchedFieldKey = "filter"
	touchedFieldCollapse    touchedFieldKey = "collapse"
	touchedFieldSpinner     touchedFieldKey = "spinner"
	touchedFieldRefresh     touchedFieldKey = "refresh"
)

type touchedFieldCase struct {
	Name                 string                 `yaml:"name"`
	Current              config.SelectionConfig `yaml:"current"`
	ProjectIdentity      string                 `yaml:"projectIdentity"`
	Harness              string                 `yaml:"harness"`
	ClonePath            string                 `yaml:"clonePath"`
	GitRemote            string                 `yaml:"gitRemote"`
	Branch               string                 `yaml:"branch"`
	SessionID            string                 `yaml:"sessionId"`
	Keys                 []touchedFieldKey      `yaml:"keys"`
	ExpectedSessionState string                 `yaml:"expectedSessionState"`
	ExpectedSetCount     int                    `yaml:"expectedSetCount"`
	ExpectReconcileError bool                   `yaml:"expectReconcileError"`
	WithPreview          bool                   `yaml:"withPreview"`
}

func loadTouchedSelectionDocument(t *testing.T) touchedSelectionDocument {
	t.Helper()
	var document touchedSelectionDocument
	decoder := yaml.NewDecoder(bytes.NewReader(touchedSelectionData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/touched_selection.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("touched_selection.yaml must hold exactly one document: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || document.ExpectedCaseCount != len(document.ExpectedNames) || len(document.Cases) == 0 {
		t.Fatalf("fixture manifest count=%d names=%d cases=%d", document.ExpectedCaseCount, len(document.ExpectedNames), len(document.Cases))
	}
	if document.ExpectedFieldCaseCount != len(document.FieldCases) || document.ExpectedFieldCaseCount != len(document.ExpectedFieldNames) || len(document.FieldCases) == 0 {
		t.Fatalf("field fixture manifest count=%d names=%d cases=%d", document.ExpectedFieldCaseCount, len(document.ExpectedFieldNames), len(document.FieldCases))
	}
	seen := map[string]bool{}
	for index, testCase := range document.Cases {
		testutil.RequireFixtureFields(t, "touched selection", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "operation", Value: string(testCase.Operation)},
			{Key: "scope.harness", Value: testCase.Scope.Harness},
			{Key: "scope.clonePath", Value: testCase.Scope.ClonePath},
		})
		if testCase.Name != document.ExpectedNames[index] {
			t.Fatalf("case[%d] name=%q, manifest=%q", index, testCase.Name, document.ExpectedNames[index])
		}
		if seen[testCase.Name] {
			t.Fatalf("duplicate touched-selection case %q", testCase.Name)
		}
		seen[testCase.Name] = true
		if !validTouchedOperation(testCase.Operation) {
			t.Fatalf("case %q has unknown operation %q", testCase.Name, testCase.Operation)
		}
		if len(testCase.Scope.VisibleSessions) == 0 {
			t.Fatalf("case %q has no visible session evidence", testCase.Name)
		}
	}
	for index, testCase := range document.FieldCases {
		testutil.RequireFixtureFields(t, "touched tree field", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "projectIdentity", Value: testCase.ProjectIdentity},
			{Key: "harness", Value: testCase.Harness},
			{Key: "sessionId", Value: testCase.SessionID},
		})
		if testCase.Name != document.ExpectedFieldNames[index] {
			t.Fatalf("fieldCase[%d] name=%q, manifest=%q", index, testCase.Name, document.ExpectedFieldNames[index])
		}
		for _, key := range testCase.Keys {
			if key != touchedFieldDown && key != touchedFieldToggle && key != touchedFieldSelectUnder &&
				key != touchedFieldFilter && key != touchedFieldCollapse && key != touchedFieldSpinner && key != touchedFieldRefresh {
				t.Fatalf("field case %q has unknown key %q", testCase.Name, key)
			}
		}
		if _, ok := touchedTriState(testCase.ExpectedSessionState); !ok {
			t.Fatalf("field case %q has unknown expected state %q", testCase.Name, testCase.ExpectedSessionState)
		}
	}
	return document
}

func validTouchedOperation(operation touchedSelectionOperation) bool {
	switch operation {
	case touchedProjectOn, touchedProjectOff, touchedBranchOn, touchedBranchOff, touchedSessionOn, touchedSessionOff:
		return true
	default:
		return false
	}
}

func TestReconcileTouchedSelectionFixture(t *testing.T) {
	document := loadTouchedSelectionDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			current := TreeSelection{Mode: testCase.Current.Mode, Harnesses: testCase.Current.Harnesses}
			scope := touchedScope(testCase)
			got, changed, err := reconcileTouchedSelection(current, UnmatchedBaseline{}, []selectionScope{scope}, testCase.Current.AutoIngestNewBranches)
			if testCase.ExpectErrorContains != "" {
				if err == nil || !bytes.Contains([]byte(err.Error()), []byte(testCase.ExpectErrorContains)) {
					t.Fatalf("reconcile error=%v, want text %q", err, testCase.ExpectErrorContains)
				}
				if changed || !reflect.DeepEqual(got, current) {
					t.Fatalf("failed reconciliation changed current\n got: %#v\nwant: %#v", got, current)
				}
				return
			}
			if err != nil {
				t.Fatalf("reconcile touched selection: %v", err)
			}
			if !changed {
				t.Fatal("fixture action reported no field-level change")
			}
			selection := got.ToSelectionConfig(testCase.Current.AutoIngestNewBranches)
			if !reflect.DeepEqual(selection, testCase.Expected) {
				t.Fatalf("reconciled selection mismatch\n got: %#v\nwant: %#v", selection, testCase.Expected)
			}
		})
	}
}

func touchedScope(testCase touchedSelectionCase) selectionScope {
	identity := fmt.Sprintf("%d:%s%s", len(testCase.Scope.Harness), testCase.Scope.Harness, testCase.Scope.ClonePath)
	root := &kit.TreeNode{
		ID:    identity,
		Label: testCase.Scope.ProjectName,
		Meta: map[string]string{
			MetaProjectIdentity: identity,
			MetaProjectHarness:  testCase.Scope.Harness,
			MetaClonePath:       testCase.Scope.ClonePath,
			MetaRemote:          testCase.Scope.GitRemote,
			MetaProjectName:     testCase.Scope.ProjectName,
		},
	}
	branches := map[string]*kit.TreeNode{}
	var order []string
	for _, visible := range testCase.Scope.VisibleSessions {
		branch := branches[visible.Branch]
		if branch == nil {
			branch = &kit.TreeNode{ID: visible.Branch, Label: visible.Branch, Meta: map[string]string{MetaBranch: visible.Branch}}
			branches[visible.Branch] = branch
			order = append(order, visible.Branch)
		}
		branch.Children = append(branch.Children, &kit.TreeNode{
			ID: visible.SessionID,
			Meta: map[string]string{
				MetaHarness:         testCase.Scope.Harness,
				MetaProjectIdentity: identity,
				MetaClonePath:       testCase.Scope.ClonePath,
				MetaRemote:          testCase.Scope.GitRemote,
				MetaProjectName:     testCase.Scope.ProjectName,
			},
		})
	}
	for _, branch := range order {
		root.Children = append(root.Children, branches[branch])
	}
	kind, selected := selectionScopeProject, testCase.Operation == touchedProjectOn
	switch testCase.Operation {
	case touchedBranchOn, touchedBranchOff:
		kind, selected = selectionScopeBranch, testCase.Operation == touchedBranchOn
	case touchedSessionOn, touchedSessionOff:
		kind, selected = selectionScopeSession, testCase.Operation == touchedSessionOn
	}
	return selectionScope{
		kind:            kind,
		root:            root,
		projectIdentity: identity,
		harness:         testCase.Scope.Harness,
		clonePath:       ingest.ClonePath(testCase.Scope.ClonePath),
		branch:          testCase.Scope.Branch,
		sessionID:       testCase.Scope.SessionID,
		selected:        selected,
	}
}

type touchedStaticTreeSource struct {
	root *kit.TreeNode
}

func (source touchedStaticTreeSource) Load(context.Context) ([]*kit.TreeNode, error) {
	return []*kit.TreeNode{cloneTouchedNode(source.root)}, nil
}

func cloneTouchedNode(node *kit.TreeNode) *kit.TreeNode {
	copy := &kit.TreeNode{ID: node.ID, Label: node.Label, State: node.State}
	if node.Meta != nil {
		copy.Meta = make(map[string]string, len(node.Meta))
		for key, value := range node.Meta {
			copy.Meta[key] = value
		}
	}
	for _, child := range node.Children {
		copy.Children = append(copy.Children, cloneTouchedNode(child))
	}
	return copy
}

var _ kit.TreeSource = touchedStaticTreeSource{}

func TestTreeFieldTouchedSelectionFixture(t *testing.T) {
	document := loadTouchedSelectionDocument(t)
	for _, testCase := range document.FieldCases {
		t.Run(testCase.Name, func(t *testing.T) {
			configured := config.BaseConfig()
			configured.Selection = testCase.Current
			path := t.TempDir() + "/config.yaml"
			if err := config.SaveAtomic(path, configured); err != nil {
				t.Fatalf("save field fixture config: %v", err)
			}
			draft, err := NewDraft(path, configured)
			if err != nil {
				t.Fatalf("open field fixture draft: %v", err)
			}
			root := touchedFieldRoot(testCase)
			setCount := 0
			accessor := Accessor[TreeSelection]{
				Get: func(current *config.Config) TreeSelection {
					return TreeSelection{Mode: current.Selection.Mode, Harnesses: current.Selection.Harnesses}
				},
				Set: func(current *config.Config, value TreeSelection) {
					setCount++
					current.Selection.Mode = value.Mode
					current.Selection.Harnesses = value.Harnesses
				},
			}
			options := []TreeOption{WithSelectionRestoration(), WithFacet(MetaHarness, "harness")}
			if testCase.WithPreview {
				options = append(options, WithPreviewBodySource(touchedPreviewSource{}))
			}
			registry := Registry{Sections: []Section{{
				Key: "transcripts", Title: "select transcripts",
				Fields: []Field{Tree("selection", "transcripts", accessor, touchedStaticTreeSource{root: root}, options...)},
			}}}
			field := registry.Sections[0].Fields[0].(*treeField)
			flow := NewFlow(theme.New(theme.ModeDark), registry, draft)
			flow.SetSize(100, 24)
			flow = drainInit(flow)
			before := TreeSelection{Mode: draft.Working().Selection.Mode, Harnesses: cloneHarnessMap(draft.Working().Selection.Harnesses)}
			beforeMarkers := touchedMarkerSnapshot(field.selectionRoots())

			for _, fixtureKey := range testCase.Keys {
				switch fixtureKey {
				case touchedFieldRefresh:
					var refresh tea.Cmd
					field.tree, refresh = field.tree.Load()
					for _, message := range runAll(refresh) {
						flow, _ = flow.Update(message)
					}
				case touchedFieldSpinner:
					flow, _ = flow.Update(spinner.TickMsg{})
				default:
					var command tea.Cmd
					flow, command = flow.Update(key(touchedFieldKeyPress(fixtureKey)))
					for _, message := range runAll(command) {
						flow, _ = flow.Update(message)
					}
				}
			}
			if setCount != testCase.ExpectedSetCount {
				t.Fatalf("accessor Set count=%d, want %d", setCount, testCase.ExpectedSetCount)
			}
			after := TreeSelection{Mode: draft.Working().Selection.Mode, Harnesses: draft.Working().Selection.Harnesses}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("no-op/rollback changed Draft.Working\n got: %#v\nwant: %#v", after, before)
			}
			if afterMarkers := touchedMarkerSnapshot(field.selectionRoots()); !reflect.DeepEqual(afterMarkers, beforeMarkers) {
				t.Fatalf("no-op/rollback changed private markers\n got: %#v\nwant: %#v", afterMarkers, beforeMarkers)
			}
			if (field.reconcileErr != nil) != testCase.ExpectReconcileError {
				t.Fatalf("reconcileErr=%v, expected present=%v", field.reconcileErr, testCase.ExpectReconcileError)
			}
			if field.reconcileErr != nil {
				for _, part := range []string{"what:", "why:", "where:", "when:", "meaning:", "fix:"} {
					if !bytes.Contains([]byte(field.reconcileErr.Error()), []byte(part)) {
						t.Fatalf("actionable error missing %q: %v", part, field.reconcileErr)
					}
				}
			}
			sessions := sessionNodes(field.selectionRoots())
			state, ok := touchedTriState(testCase.ExpectedSessionState)
			if !ok || sessions[testCase.SessionID] == nil || sessions[testCase.SessionID].State != state {
				t.Fatalf("session state=%v, want %s", sessions[testCase.SessionID], testCase.ExpectedSessionState)
			}
		})
	}
}

func touchedFieldRoot(testCase touchedFieldCase) *kit.TreeNode {
	return &kit.TreeNode{
		ID: testCase.ProjectIdentity,
		Meta: map[string]string{
			MetaProjectIdentity: testCase.ProjectIdentity,
			MetaProjectHarness:  testCase.Harness,
			MetaClonePath:       testCase.ClonePath,
			MetaRemote:          testCase.GitRemote,
		},
		Children: []*kit.TreeNode{{
			ID: testCase.Branch, Label: testCase.Branch, Meta: map[string]string{MetaBranch: testCase.Branch},
			Children: []*kit.TreeNode{{
				ID: testCase.SessionID,
				Meta: map[string]string{
					MetaHarness:         testCase.Harness,
					MetaProjectIdentity: testCase.ProjectIdentity,
					MetaClonePath:       testCase.ClonePath,
					MetaRemote:          testCase.GitRemote,
				},
			}},
		}},
	}
}

func touchedFieldKeyPress(key touchedFieldKey) string {
	switch key {
	case touchedFieldDown:
		return "j"
	case touchedFieldToggle:
		return "space"
	case touchedFieldSelectUnder:
		return "A"
	case touchedFieldFilter:
		return "f"
	case touchedFieldCollapse:
		return "h"
	default:
		return ""
	}
}

func touchedTriState(value string) (kit.TriState, bool) {
	switch value {
	case "unchecked":
		return kit.Unchecked, true
	case "checked":
		return kit.Checked, true
	case "conflict":
		return kit.Conflict, true
	default:
		return kit.Unchecked, false
	}
}

func sessionNodes(roots []*kit.TreeNode) map[string]*kit.TreeNode {
	result := map[string]*kit.TreeNode{}
	for _, root := range roots {
		walkNodes(root, func(node *kit.TreeNode) {
			if harnessOf(node) != "" {
				result[node.ID] = node
			}
		})
	}
	return result
}

type touchedPreviewBody struct{}

func (touchedPreviewBody) Render(int) string { return "preview" }

type touchedPreviewSource struct{}

func (touchedPreviewSource) Body(string) (kit.PreviewBody, error) { return touchedPreviewBody{}, nil }

var _ kit.BodySource = touchedPreviewSource{}

type touchedMarkerState struct {
	BranchValue    string
	BranchPresent  bool
	SessionValue   string
	SessionPresent bool
}

func touchedMarkerSnapshot(roots []*kit.TreeNode) map[string]touchedMarkerState {
	result := map[string]touchedMarkerState{}
	for _, root := range roots {
		walkNodes(root, func(node *kit.TreeNode) {
			branchValue, branchPresent := markerValue(node, metaExplicitBranchSelection)
			sessionValue, sessionPresent := markerValue(node, metaExplicitSessionSelection)
			result[node.ID] = touchedMarkerState{
				BranchValue: branchValue, BranchPresent: branchPresent,
				SessionValue: sessionValue, SessionPresent: sessionPresent,
			}
		})
	}
	return result
}
