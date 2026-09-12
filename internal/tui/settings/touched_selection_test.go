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
	ExpectedCaseCount           int                     `yaml:"expectedCaseCount"`
	ExpectedFieldCaseCount      int                     `yaml:"expectedFieldCaseCount"`
	ExpectedDerivationCaseCount int                     `yaml:"expectedDerivationCaseCount"`
	ExpectedSanitizeCaseCount   int                     `yaml:"expectedSanitizeCaseCount"`
	ExpectedNames               []string                `yaml:"expectedNames"`
	ExpectedFieldNames          []string                `yaml:"expectedFieldNames"`
	ExpectedDerivationNames     []string                `yaml:"expectedDerivationNames"`
	ExpectedSanitizeNames       []string                `yaml:"expectedSanitizeNames"`
	FieldCases                  []touchedFieldCase      `yaml:"fieldCases"`
	Cases                       []touchedSelectionCase  `yaml:"cases"`
	DerivationCases             []touchedDerivationCase `yaml:"derivationCases"`
	SanitizeCases               []touchedSanitizeCase   `yaml:"sanitizeCases"`
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

// touchedDerivationCase derives a forest through the production [FromTreeNodes]
// path and pins the harness-keyed selection it produces. A set providerID
// selects the harness-first provider -> remote -> worktree -> session shape;
// otherwise the case builds the project-first project -> branch -> session
// shape.
type touchedDerivationCase struct {
	Name            string                    `yaml:"name"`
	ProviderID      string                    `yaml:"providerID"`
	Harness         string                    `yaml:"harness"`
	ProjectIdentity string                    `yaml:"projectIdentity"`
	ClonePath       string                    `yaml:"clonePath"`
	GitRemote       string                    `yaml:"gitRemote"`
	ProjectName     string                    `yaml:"projectName"`
	Branches        []touchedDerivationBranch `yaml:"branches"`
	Expected        config.SelectionConfig    `yaml:"expected"`
}

type touchedDerivationBranch struct {
	Branch   string                     `yaml:"branch"`
	Sessions []touchedDerivationSession `yaml:"sessions"`
}

type touchedDerivationSession struct {
	ID    string `yaml:"id"`
	State string `yaml:"state"`
}

// touchedSanitizeCase pins the rewrite of display-only placeholder policies
// into explicit session rules against one available forest. expectChecked and
// expectUnchecked optionally assert what the canonical matcher selects once the
// sanitized value is applied to that forest.
type touchedSanitizeCase struct {
	Name            string                 `yaml:"name"`
	Forest          touchedDerivationCase  `yaml:"forest"`
	Saved           config.SelectionConfig `yaml:"saved"`
	ExpectChanged   bool                   `yaml:"expectChanged"`
	Expected        config.SelectionConfig `yaml:"expected"`
	ExpectChecked   []string               `yaml:"expectChecked"`
	ExpectUnchecked []string               `yaml:"expectUnchecked"`
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
	Name                       string                 `yaml:"name"`
	Current                    config.SelectionConfig `yaml:"current"`
	ProjectIdentity            string                 `yaml:"projectIdentity"`
	Harness                    string                 `yaml:"harness"`
	LinkedHarness              string                 `yaml:"linkedHarness"`
	ClonePath                  string                 `yaml:"clonePath"`
	LinkedClonePath            string                 `yaml:"linkedClonePath"`
	GitRemote                  string                 `yaml:"gitRemote"`
	Branch                     string                 `yaml:"branch"`
	SessionID                  string                 `yaml:"sessionId"`
	SecondSessionID            string                 `yaml:"secondSessionId"`
	LinkedSessionID            string                 `yaml:"linkedSessionId"`
	SecondProject              *touchedSecondProject  `yaml:"secondProject"`
	Keys                       []touchedFieldKey      `yaml:"keys"`
	ExpectedSessionState       string                 `yaml:"expectedSessionState"`
	ExpectedSecondSessionState string                 `yaml:"expectedSecondSessionState"`
	ExpectedSetCount           int                    `yaml:"expectedSetCount"`
	ExpectReconcileError       bool                   `yaml:"expectReconcileError"`
	// SeedWithoutSave opens the draft directly from current, without the disk
	// round trip, for a legacy selection the config loader rejects but a draft
	// can still hold (a display-placeholder branch exclusion).
	SeedWithoutSave bool                    `yaml:"seedWithoutSave"`
	WithPreview     bool                    `yaml:"withPreview"`
	Expected        *config.SelectionConfig `yaml:"expected"`
}

// touchedSecondProject adds one more project root to a field case's forest,
// carrying the same harness, so a case can assert what survives in the harness
// while the primary project is edited.
type touchedSecondProject struct {
	ProjectIdentity string `yaml:"projectIdentity"`
	ClonePath       string `yaml:"clonePath"`
	GitRemote       string `yaml:"gitRemote"`
	Branch          string `yaml:"branch"`
	SessionID       string `yaml:"sessionId"`
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
	if document.ExpectedDerivationCaseCount != len(document.DerivationCases) || document.ExpectedDerivationCaseCount != len(document.ExpectedDerivationNames) || len(document.DerivationCases) == 0 {
		t.Fatalf("derivation fixture manifest count=%d names=%d cases=%d", document.ExpectedDerivationCaseCount, len(document.ExpectedDerivationNames), len(document.DerivationCases))
	}
	if document.ExpectedSanitizeCaseCount != len(document.SanitizeCases) || document.ExpectedSanitizeCaseCount != len(document.ExpectedSanitizeNames) || len(document.SanitizeCases) == 0 {
		t.Fatalf("sanitize fixture manifest count=%d names=%d cases=%d", document.ExpectedSanitizeCaseCount, len(document.ExpectedSanitizeNames), len(document.SanitizeCases))
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
		if (testCase.LinkedClonePath == "") != (testCase.LinkedSessionID == "") {
			t.Fatalf("field case %q must set linkedClonePath and linkedSessionId together", testCase.Name)
		}
		if testCase.LinkedHarness != "" && testCase.LinkedClonePath == "" {
			t.Fatalf("field case %q sets linkedHarness without a linked session", testCase.Name)
		}
		if testCase.SecondSessionID != "" && testCase.SecondSessionID == testCase.SessionID {
			t.Fatalf("field case %q repeats its primary session as the second session", testCase.Name)
		}
		if testCase.SecondProject != nil {
			testutil.RequireFixtureFields(t, "touched tree field second project", testCase.Name, []testutil.FixtureField{
				{Key: "secondProject.projectIdentity", Value: testCase.SecondProject.ProjectIdentity},
				{Key: "secondProject.clonePath", Value: testCase.SecondProject.ClonePath},
				{Key: "secondProject.gitRemote", Value: testCase.SecondProject.GitRemote},
				{Key: "secondProject.branch", Value: testCase.SecondProject.Branch},
				{Key: "secondProject.sessionId", Value: testCase.SecondProject.SessionID},
			})
		}
		if (testCase.SecondProject == nil) != (testCase.ExpectedSecondSessionState == "") {
			t.Fatalf("field case %q must set secondProject and expectedSecondSessionState together", testCase.Name)
		}
	}
	for index, testCase := range document.DerivationCases {
		required := []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "harness", Value: testCase.Harness},
		}
		if testCase.ProviderID == "" {
			required = append(required,
				testutil.FixtureField{Key: "projectIdentity", Value: testCase.ProjectIdentity},
				testutil.FixtureField{Key: "clonePath", Value: testCase.ClonePath},
			)
		}
		testutil.RequireFixtureFields(t, "touched selection derivation", testCase.Name, required)
		if testCase.Name != document.ExpectedDerivationNames[index] {
			t.Fatalf("derivationCase[%d] name=%q, manifest=%q", index, testCase.Name, document.ExpectedDerivationNames[index])
		}
		if len(testCase.Branches) == 0 {
			t.Fatalf("derivation case %q has no branches", testCase.Name)
		}
		if testCase.Expected.Mode != config.SelectionModeSelected {
			t.Fatalf("derivation case %q expected mode=%q, want %q", testCase.Name, testCase.Expected.Mode, config.SelectionModeSelected)
		}
	}
	for index, testCase := range document.SanitizeCases {
		testutil.RequireFixtureFields(t, "touched selection sanitize", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "forest.harness", Value: testCase.Forest.Harness},
		})
		if testCase.Name != document.ExpectedSanitizeNames[index] {
			t.Fatalf("sanitizeCase[%d] name=%q, manifest=%q", index, testCase.Name, document.ExpectedSanitizeNames[index])
		}
		if testCase.Saved.Mode != config.SelectionModeSelected {
			t.Fatalf("sanitize case %q saved mode=%q, want %q", testCase.Name, testCase.Saved.Mode, config.SelectionModeSelected)
		}
		if len(testCase.Forest.Branches) == 0 {
			t.Fatalf("sanitize case %q has no forest branches", testCase.Name)
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
	roots []*kit.TreeNode
}

func (source touchedStaticTreeSource) Load(context.Context) ([]*kit.TreeNode, error) {
	roots := make([]*kit.TreeNode, 0, len(source.roots))
	for _, root := range source.roots {
		roots = append(roots, cloneTouchedNode(root))
	}
	return roots, nil
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
			if !testCase.SeedWithoutSave {
				if err := config.SaveAtomic(path, configured); err != nil {
					t.Fatalf("save field fixture config: %v", err)
				}
			}
			draft, err := NewDraft(path, configured)
			if err != nil {
				t.Fatalf("open field fixture draft: %v", err)
			}
			root := touchedFieldRoots(testCase)
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
				Fields: []Field{Tree("selection", "transcripts", accessor, touchedStaticTreeSource{roots: root}, options...)},
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
			for harness, configured := range after.Harnesses {
				for _, project := range configured.Projects {
					if containsUnknownBranchPlaceholder(project.Branches) {
						t.Fatalf("field case %q persisted the unknown-branch placeholder in harness %q: %#v", testCase.Name, harness, project)
					}
				}
				for _, exclusion := range configured.Exclusions.Branches {
					if containsUnknownBranchPlaceholder(exclusion.Branches) {
						t.Fatalf("field case %q persisted the unknown-branch placeholder in harness %q: %#v", testCase.Name, harness, exclusion)
					}
				}
			}
			want := before
			if testCase.Expected != nil {
				want = TreeSelection{Mode: testCase.Expected.Mode, Harnesses: testCase.Expected.Harnesses}
			}
			if !reflect.DeepEqual(after, want) {
				t.Fatalf("tree field selection mismatch\n got: %#v\nwant: %#v", after, want)
			}
			if testCase.Expected == nil {
				if afterMarkers := touchedMarkerSnapshot(field.selectionRoots()); !reflect.DeepEqual(afterMarkers, beforeMarkers) {
					t.Fatalf("no-op/rollback changed private markers\n got: %#v\nwant: %#v", afterMarkers, beforeMarkers)
				}
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
			if testCase.SecondProject != nil {
				secondState, ok := touchedTriState(testCase.ExpectedSecondSessionState)
				if !ok || sessions[testCase.SecondProject.SessionID] == nil || sessions[testCase.SecondProject.SessionID].State != secondState {
					t.Fatalf("second session state=%v, want %s", sessions[testCase.SecondProject.SessionID], testCase.ExpectedSecondSessionState)
				}
			}
		})
	}
}

// touchedFieldRoots builds the field case's forest: the primary project root
// plus the optional second project root that shares the case's harness.
func touchedFieldRoots(testCase touchedFieldCase) []*kit.TreeNode {
	roots := []*kit.TreeNode{touchedFieldRoot(testCase)}
	second := testCase.SecondProject
	if second == nil {
		return roots
	}
	roots = append(roots, &kit.TreeNode{
		ID: second.ProjectIdentity,
		Meta: map[string]string{
			MetaProjectIdentity: second.ProjectIdentity,
			MetaClonePath:       second.ClonePath,
			MetaRemote:          second.GitRemote,
			MetaProjectHarness:  testCase.Harness,
		},
		Children: []*kit.TreeNode{{
			ID: second.Branch, Label: second.Branch, Meta: map[string]string{MetaBranch: second.Branch},
			Children: []*kit.TreeNode{{
				ID: second.SessionID,
				Meta: map[string]string{
					MetaHarness:         testCase.Harness,
					MetaProjectIdentity: second.ProjectIdentity,
					MetaClonePath:       second.ClonePath,
					MetaRemote:          second.GitRemote,
				},
			}},
		}},
	})
	return roots
}

func touchedFieldRoot(testCase touchedFieldCase) *kit.TreeNode {
	root := &kit.TreeNode{
		ID: testCase.ProjectIdentity,
		Meta: map[string]string{
			MetaProjectIdentity: testCase.ProjectIdentity,
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
	if testCase.LinkedHarness == "" || testCase.LinkedHarness == testCase.Harness {
		root.Meta[MetaProjectHarness] = testCase.Harness
	}
	if testCase.LinkedClonePath != "" {
		delete(root.Meta, MetaClonePath)
		linkedHarness := testCase.LinkedHarness
		if linkedHarness == "" {
			linkedHarness = testCase.Harness
		}
		root.Children[0].Children = append(root.Children[0].Children, &kit.TreeNode{
			ID: testCase.LinkedSessionID,
			Meta: map[string]string{
				MetaHarness:         linkedHarness,
				MetaProjectIdentity: testCase.ProjectIdentity,
				MetaClonePath:       testCase.LinkedClonePath,
				MetaRemote:          testCase.GitRemote,
			},
		})
	}
	if testCase.SecondSessionID != "" {
		root.Children[0].Children = append(root.Children[0].Children, &kit.TreeNode{
			ID: testCase.SecondSessionID,
			Meta: map[string]string{
				MetaHarness:         testCase.Harness,
				MetaProjectIdentity: testCase.ProjectIdentity,
				MetaClonePath:       testCase.ClonePath,
				MetaRemote:          testCase.GitRemote,
			},
		})
	}
	return root
}

// TestFromTreeNodesUnknownBranchDerivationFixture pins the one derivation
// boundary the saved configuration comes from: a fully checked unknown-branch
// group never becomes a branch name. Where a branch rule cannot express the
// group, its sessions fall back to explicit session IDs.
func TestFromTreeNodesUnknownBranchDerivationFixture(t *testing.T) {
	document := loadTouchedSelectionDocument(t)
	for _, testCase := range document.DerivationCases {
		t.Run(testCase.Name, func(t *testing.T) {
			got := FromTreeNodes(touchedDerivationRoots(t, testCase))
			want := TreeSelection{Mode: testCase.Expected.Mode, Harnesses: testCase.Expected.Harnesses}
			if !selectionsEqual(got, want) {
				t.Fatalf("derived selection mismatch\n got: %#v\nwant: %#v", got, want)
			}
			for harness, configured := range got.Harnesses {
				for _, project := range configured.Projects {
					if containsUnknownBranchPlaceholder(project.Branches) {
						t.Fatalf("harness %q project %#v persists the unknown-branch placeholder", harness, project)
					}
				}
				for _, exclusion := range configured.Exclusions.Branches {
					if containsUnknownBranchPlaceholder(exclusion.Branches) {
						t.Fatalf("harness %q exclusion %#v persists the unknown-branch placeholder", harness, exclusion)
					}
				}
			}
		})
	}
}

// TestSanitizePlaceholderSelectionFixture pins the rewrite itself: placeholder
// policies become explicit session rules, drop without widening, and leave a
// placeholder-free selection unchanged.
func TestSanitizePlaceholderSelectionFixture(t *testing.T) {
	document := loadTouchedSelectionDocument(t)
	for _, testCase := range document.SanitizeCases {
		t.Run(testCase.Name, func(t *testing.T) {
			projects := availableProjectsFromForest(touchedDerivationRoots(t, testCase.Forest))
			got, changed := sanitizePlaceholderSelection(testCase.Saved, projects)
			if changed != testCase.ExpectChanged {
				t.Fatalf("sanitize changed=%v, want %v\n got: %#v", changed, testCase.ExpectChanged, got)
			}
			gotSelection := TreeSelection{Mode: got.Mode, Harnesses: got.Harnesses}
			wantSelection := TreeSelection{Mode: testCase.Expected.Mode, Harnesses: testCase.Expected.Harnesses}
			if !selectionsEqual(gotSelection, wantSelection) {
				t.Fatalf("sanitized selection mismatch\n got: %#v\nwant: %#v", got, testCase.Expected)
			}
			for harness, configured := range got.Harnesses {
				for _, project := range configured.Projects {
					if containsUnknownBranchPlaceholder(project.Branches) {
						t.Fatalf("harness %q project %#v persists the unknown-branch placeholder", harness, project)
					}
				}
				for _, exclusion := range configured.Exclusions.Branches {
					if containsUnknownBranchPlaceholder(exclusion.Branches) {
						t.Fatalf("harness %q exclusion %#v persists the unknown-branch placeholder", harness, exclusion)
					}
				}
			}
			if len(testCase.ExpectChecked) > 0 || len(testCase.ExpectUnchecked) > 0 {
				roots := touchedDerivationRoots(t, testCase.Forest)
				PrepopulateSelection(roots, got)
				states := sessionNodes(roots)
				for _, sessionID := range testCase.ExpectChecked {
					if node := states[sessionID]; node == nil || node.State != kit.Checked {
						t.Fatalf("session %q state=%v, want checked", sessionID, node)
					}
				}
				for _, sessionID := range testCase.ExpectUnchecked {
					if node := states[sessionID]; node == nil || node.State != kit.Unchecked {
						t.Fatalf("session %q state=%v, want unchecked", sessionID, node)
					}
				}
			}
		})
	}
}

func touchedDerivationRoot(t *testing.T, testCase touchedDerivationCase) *kit.TreeNode {
	t.Helper()
	root := &kit.TreeNode{
		ID:    testCase.ProjectIdentity,
		Label: testCase.ProjectName,
		Meta: map[string]string{
			MetaProjectIdentity: testCase.ProjectIdentity,
			MetaClonePath:       testCase.ClonePath,
			MetaRemote:          testCase.GitRemote,
			MetaProjectName:     testCase.ProjectName,
			MetaProjectHarness:  testCase.Harness,
		},
	}
	for _, branch := range testCase.Branches {
		branchNode := &kit.TreeNode{
			ID:    "branch:" + branch.Branch,
			Label: branch.Branch,
			Meta:  map[string]string{MetaBranch: branch.Branch},
		}
		for _, session := range branch.Sessions {
			state, ok := touchedTriState(session.State)
			if !ok {
				t.Fatalf("derivation case %q session %q has unknown state %q", testCase.Name, session.ID, session.State)
			}
			branchNode.Children = append(branchNode.Children, &kit.TreeNode{
				ID:    session.ID,
				State: state,
				Meta: map[string]string{
					MetaHarness:         testCase.Harness,
					MetaProjectIdentity: testCase.ProjectIdentity,
					MetaClonePath:       testCase.ClonePath,
					MetaRemote:          testCase.GitRemote,
					MetaProjectName:     testCase.ProjectName,
				},
			})
		}
		root.Children = append(root.Children, branchNode)
	}
	rollup(root)
	return root
}

// touchedHarnessFirstDerivationRoot builds the compatibility
// provider -> remote -> worktree -> session shape, whose session leaves carry
// no harness meta and therefore take the harness-first derivation.
func touchedHarnessFirstDerivationRoot(t *testing.T, testCase touchedDerivationCase) *kit.TreeNode {
	t.Helper()
	remote := &kit.TreeNode{
		ID:    testCase.ProjectIdentity,
		Label: testCase.ProjectName,
		Meta: map[string]string{
			MetaRemote:      testCase.GitRemote,
			MetaProjectName: testCase.ProjectName,
		},
	}
	for _, branch := range testCase.Branches {
		branchNode := &kit.TreeNode{
			ID:    testCase.ProjectIdentity + "/" + branch.Branch,
			Label: branch.Branch,
			Meta:  map[string]string{MetaBranch: branch.Branch},
		}
		for _, session := range branch.Sessions {
			state, ok := touchedTriState(session.State)
			if !ok {
				t.Fatalf("derivation case %q session %q has unknown state %q", testCase.Name, session.ID, session.State)
			}
			branchNode.Children = append(branchNode.Children, &kit.TreeNode{ID: session.ID, State: state})
		}
		remote.Children = append(remote.Children, branchNode)
	}
	root := &kit.TreeNode{
		ID:       testCase.ProviderID,
		Label:    testCase.ProviderID,
		Meta:     map[string]string{"kind": "provider"},
		Children: []*kit.TreeNode{remote},
	}
	rollup(root)
	return root
}

func touchedDerivationRoots(t *testing.T, testCase touchedDerivationCase) []*kit.TreeNode {
	t.Helper()
	if testCase.ProviderID != "" {
		return []*kit.TreeNode{touchedHarnessFirstDerivationRoot(t, testCase)}
	}
	return []*kit.TreeNode{touchedDerivationRoot(t, testCase)}
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
