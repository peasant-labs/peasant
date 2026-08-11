package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/mounted_flow.yaml
var mountedFlowData []byte

type mountedFlowOperation string

const (
	mountedFlowEditRefreshSave mountedFlowOperation = "edit-refresh-save"
	mountedFlowLoadOnly        mountedFlowOperation = "load-only"
)

type mountedPathState string

const (
	mountedPathDirectory mountedPathState = "directory"
	mountedPathSymlink   mountedPathState = "symlink"
	mountedPathMoved     mountedPathState = "moved"
)

type mountedFlowDocument struct {
	ExpectedCaseCount int               `yaml:"expectedCaseCount"`
	Cases             []mountedFlowCase `yaml:"cases"`
}

type mountedFlowCase struct {
	Name      string                  `yaml:"name"`
	Operation mountedFlowOperation    `yaml:"operation"`
	Paths     []mountedPathFixture    `yaml:"paths"`
	Listings  []mountedListingFixture `yaml:"listings"`
	Saved     mountedSelectionFixture `yaml:"saved"`
	Expect    mountedFlowExpectation  `yaml:"expect"`
}

type mountedPathFixture struct {
	Key    string           `yaml:"key"`
	State  mountedPathState `yaml:"state"`
	Target string           `yaml:"target"`
}

type mountedListingFixture struct {
	Harness     string `yaml:"harness"`
	ProjectName string `yaml:"projectName"`
	GitRemote   string `yaml:"gitRemote"`
	PathKey     string `yaml:"pathKey"`
	Branch      string `yaml:"branch"`
	SessionID   string `yaml:"sessionId"`
	Title       string `yaml:"title"`
}

type mountedSelectionFixture struct {
	Mode                  config.SelectionMode            `yaml:"mode"`
	AutoIngestNewBranches bool                            `yaml:"autoIngestNewBranches"`
	Projects              []mountedProjectFixture         `yaml:"projects"`
	Sessions              []mountedSessionFixture         `yaml:"sessions"`
	BranchExclusions      []mountedBranchExclusionFixture `yaml:"branchExclusions"`
	SessionExclusions     []mountedSessionFixture         `yaml:"sessionExclusions"`
}

type mountedProjectFixture struct {
	Harness   string   `yaml:"harness"`
	GitRemote string   `yaml:"gitRemote"`
	Name      string   `yaml:"name"`
	PathKeys  []string `yaml:"pathKeys"`
	Branches  []string `yaml:"branches"`
}

type mountedSessionFixture struct {
	Harness string   `yaml:"harness"`
	IDs     []string `yaml:"ids"`
}

type mountedBranchExclusionFixture struct {
	Harness  string   `yaml:"harness"`
	PathKey  string   `yaml:"pathKey"`
	Branches []string `yaml:"branches"`
}

type mountedFlowExpectation struct {
	Roots                         []mountedRootExpectation `yaml:"roots"`
	SameRemotePathKeys            []string                 `yaml:"sameRemotePathKeys"`
	EditorSessions                []string                 `yaml:"editorSessions"`
	UnavailableListings           []string                 `yaml:"unavailableListings"`
	InitiallyCheckedSessions      []string                 `yaml:"initiallyCheckedSessions"`
	InitiallyUncheckedSessions    []string                 `yaml:"initiallyUncheckedSessions"`
	EditRootPathKey               string                   `yaml:"editRootPathKey"`
	AfterRefreshCheckedSessions   []string                 `yaml:"afterRefreshCheckedSessions"`
	AfterRefreshUncheckedSessions []string                 `yaml:"afterRefreshUncheckedSessions"`
	LoadCount                     int                      `yaml:"loadCount"`
	Saved                         mountedSelectionFixture  `yaml:"saved"`
}

type mountedRootExpectation struct {
	PathKey            string `yaml:"pathKey"`
	RemoteMultiplicity string `yaml:"remoteMultiplicity"`
	NameMultiplicity   string `yaml:"nameMultiplicity"`
}

func loadMountedFlowDocument(t *testing.T) mountedFlowDocument {
	t.Helper()
	var document mountedFlowDocument
	decoder := yaml.NewDecoder(bytes.NewReader(mountedFlowData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/mounted_flow.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("mounted_flow.yaml must hold exactly one document: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || len(document.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", document.ExpectedCaseCount, len(document.Cases))
	}
	for _, testCase := range document.Cases {
		validateMountedFlowCase(t, testCase)
	}
	return document
}

func validateMountedFlowCase(t *testing.T, testCase mountedFlowCase) {
	t.Helper()
	testutil.RequireFixtureFields(t, "mounted flow", testCase.Name, []testutil.FixtureField{
		{Key: "name", Value: testCase.Name},
		{Key: "operation", Value: string(testCase.Operation)},
		{Key: "saved.mode", Value: testCase.Saved.Mode.String()},
		{Key: "expect.saved.mode", Value: testCase.Expect.Saved.Mode.String()},
	})
	if testCase.Operation != mountedFlowEditRefreshSave && testCase.Operation != mountedFlowLoadOnly {
		t.Fatalf("mounted flow case %q has unknown operation %q", testCase.Name, testCase.Operation)
	}
	if !testCase.Saved.Mode.IsValid() || !testCase.Expect.Saved.Mode.IsValid() {
		t.Fatalf("mounted flow case %q has an invalid selection mode", testCase.Name)
	}
	if len(testCase.Paths) == 0 || len(testCase.Listings) == 0 || len(testCase.Expect.Roots) == 0 {
		t.Fatalf("mounted flow case %q must define paths, listings, and expected roots", testCase.Name)
	}
	if testCase.Expect.LoadCount < 1 {
		t.Fatalf("mounted flow case %q must expect at least one async load", testCase.Name)
	}

	pathKeys := map[string]struct{}{}
	for _, path := range testCase.Paths {
		validateMountedPathKey(t, testCase.Name, path.Key)
		if _, exists := pathKeys[path.Key]; exists {
			t.Fatalf("mounted flow case %q repeats path key %q", testCase.Name, path.Key)
		}
		pathKeys[path.Key] = struct{}{}
		if path.State != mountedPathDirectory && path.State != mountedPathSymlink && path.State != mountedPathMoved {
			t.Fatalf("mounted flow case %q path %q has unknown state %q", testCase.Name, path.Key, path.State)
		}
		if path.State != mountedPathDirectory {
			validateMountedPathKey(t, testCase.Name, path.Target)
			pathKeys[path.Target] = struct{}{}
		} else if path.Target != "" {
			t.Fatalf("mounted flow case %q directory %q must not define a target", testCase.Name, path.Key)
		}
	}
	for _, listing := range testCase.Listings {
		if listing.Harness == "" || listing.SessionID == "" {
			t.Fatalf("mounted flow case %q has a listing without harness or session ID", testCase.Name)
		}
		requireMountedPathReference(t, testCase.Name, pathKeys, listing.PathKey)
	}
	validateMountedSelectionPaths(t, testCase.Name, pathKeys, testCase.Saved)
	validateMountedSelectionPaths(t, testCase.Name, pathKeys, testCase.Expect.Saved)
	for _, root := range testCase.Expect.Roots {
		requireMountedPathReference(t, testCase.Name, pathKeys, root.PathKey)
		if !validMountedMultiplicity(root.RemoteMultiplicity) || !validMountedMultiplicity(root.NameMultiplicity) {
			t.Fatalf("mounted flow case %q root %q must use explicit unique or ambiguous multiplicity", testCase.Name, root.PathKey)
		}
	}
	for _, pathKey := range testCase.Expect.SameRemotePathKeys {
		requireMountedPathReference(t, testCase.Name, pathKeys, pathKey)
	}
	if testCase.Operation == mountedFlowEditRefreshSave {
		requireMountedPathReference(t, testCase.Name, pathKeys, testCase.Expect.EditRootPathKey)
		if testCase.Expect.LoadCount < 2 || len(testCase.Expect.AfterRefreshCheckedSessions) == 0 {
			t.Fatalf("mounted flow case %q must assert the refreshed on-screen selection", testCase.Name)
		}
	} else if testCase.Expect.EditRootPathKey != "" || len(testCase.Expect.AfterRefreshCheckedSessions) > 0 || len(testCase.Expect.AfterRefreshUncheckedSessions) > 0 {
		t.Fatalf("mounted flow load-only case %q must not define edit or refresh expectations", testCase.Name)
	}
}

func validateMountedPathKey(t *testing.T, caseName, key string) {
	t.Helper()
	if key == "" || filepath.IsAbs(key) || filepath.Clean(key) != key || key == "." || key == ".." || strings.HasPrefix(key, ".."+string(filepath.Separator)) {
		t.Fatalf("mounted flow case %q has unsafe path key %q", caseName, key)
	}
}

func requireMountedPathReference(t *testing.T, caseName string, pathKeys map[string]struct{}, key string) {
	t.Helper()
	if _, ok := pathKeys[key]; !ok {
		t.Fatalf("mounted flow case %q references unknown path key %q", caseName, key)
	}
}

func validateMountedSelectionPaths(t *testing.T, caseName string, pathKeys map[string]struct{}, selection mountedSelectionFixture) {
	t.Helper()
	for _, project := range selection.Projects {
		if project.Harness == "" {
			t.Fatalf("mounted flow case %q has a project without a harness", caseName)
		}
		for _, pathKey := range project.PathKeys {
			requireMountedPathReference(t, caseName, pathKeys, pathKey)
		}
	}
	for _, sessions := range selection.Sessions {
		if sessions.Harness == "" || len(sessions.IDs) == 0 {
			t.Fatalf("mounted flow case %q has an incomplete explicit-session fixture", caseName)
		}
	}
	for _, exclusion := range selection.BranchExclusions {
		if exclusion.Harness == "" || len(exclusion.Branches) == 0 {
			t.Fatalf("mounted flow case %q has an incomplete branch-exclusion fixture", caseName)
		}
		requireMountedPathReference(t, caseName, pathKeys, exclusion.PathKey)
	}
	for _, sessions := range selection.SessionExclusions {
		if sessions.Harness == "" || len(sessions.IDs) == 0 {
			t.Fatalf("mounted flow case %q has an incomplete session-exclusion fixture", caseName)
		}
	}
}

func validMountedMultiplicity(value string) bool {
	return value == settings.MetaMultiplicityUnique || value == settings.MetaMultiplicityAmbiguous
}

type mountedRecordingTreeSource struct {
	inner kit.TreeSource
	mu    sync.Mutex
	loads [][]*kit.TreeNode
}

func (s *mountedRecordingTreeSource) Load(ctx context.Context) ([]*kit.TreeNode, error) {
	roots, err := s.inner.Load(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.loads = append(s.loads, append([]*kit.TreeNode(nil), roots...))
	}
	return roots, err
}

func (s *mountedRecordingTreeSource) latest() []*kit.TreeNode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.loads) == 0 {
		return nil
	}
	return append([]*kit.TreeNode(nil), s.loads[len(s.loads)-1]...)
}

func (s *mountedRecordingTreeSource) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.loads)
}

var _ kit.TreeSource = (*mountedRecordingTreeSource)(nil)

// TestMountedKickstartFlowUsesPhysicalSelectionEvidence drives Program through
// its real settings.Flow and asynchronous kit.Tree. The recording source only
// observes the forest returned by the production scanner, which receives the
// production physical-path resolver and real temporary filesystem state.
func TestMountedKickstartFlowUsesPhysicalSelectionEvidence(t *testing.T) {
	document := loadMountedFlowDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			paths := materializeMountedPaths(t, testCase.Paths)
			baseline := mountedSelection(testCase.Saved, paths)
			configured := config.BaseConfig()
			configured.Selection = baseline
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := config.SaveAtomic(configPath, configured); err != nil {
				t.Fatalf("save mounted-flow baseline: %v", err)
			}
			draft, err := settings.NewDraft(configPath, configured)
			if err != nil {
				t.Fatalf("open mounted-flow draft: %v", err)
			}

			listings := mountedListings(testCase.Listings, paths)
			realScanner := kickstart.NewScannerTreeSource(
				listings,
				kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
			)
			source := &mountedRecordingTreeSource{inner: realScanner}
			program := kickstart.NewProgram(kickstart.ProgramDeps{
				Theme:  theme.New(theme.ModeDark),
				Draft:  draft,
				Source: source,
			})
			program.SetSize(120, 28)
			program = declineOAuth(t, program)

			if source.loadCount() != 1 {
				t.Fatalf("initial mounted flow completed %d scanner loads, want 1", source.loadCount())
			}
			roots := source.latest()
			assertMountedEditorForest(t, roots, paths, testCase.Expect)
			assertMountedSelection(t, draft.Working().Selection, baseline, "first successful load")

			switch testCase.Operation {
			case mountedFlowEditRefreshSave:
				program = editMountedRoot(t, program, roots, paths[testCase.Expect.EditRootPathKey])
				wantSaved := mountedSelection(testCase.Expect.Saved, paths)
				assertMountedSelection(t, draft.Working().Selection, wantSaved, "single on-screen edit")

				beforeRefresh := cloneMountedSelection(draft.Working().Selection)
				program = drainProgram(program, program.Init())
				if source.loadCount() != testCase.Expect.LoadCount {
					t.Fatalf("mounted refresh completed %d scanner loads, want %d", source.loadCount(), testCase.Expect.LoadCount)
				}
				assertMountedSelection(t, draft.Working().Selection, beforeRefresh, "refresh")
				assertMountedSessionStates(
					t,
					source.latest(),
					testCase.Expect.AfterRefreshCheckedSessions,
					testCase.Expect.AfterRefreshUncheckedSessions,
					"after refresh",
				)

				var commitCmd tea.Cmd
				program, commitCmd = advanceToCommit(program)
				program = drainProgram(program, commitCmd)
				if !program.Committed() {
					t.Fatalf("mounted flow did not commit the single edit: phase=%s", program.Phase())
				}
				reloaded, err := config.Parse(mustReadFile(t, configPath))
				if err != nil {
					t.Fatalf("parse mounted-flow commit: %v", err)
				}
				assertMountedSelection(t, reloaded.Selection, wantSaved, "committed config")
			case mountedFlowLoadOnly:
				if source.loadCount() != testCase.Expect.LoadCount {
					t.Fatalf("mounted load-only flow completed %d scanner loads, want %d", source.loadCount(), testCase.Expect.LoadCount)
				}
				if draft.Working().Selection.Mode != config.SelectionModeAll {
					t.Fatalf("mode all was routed through selected restoration: %#v", draft.Working().Selection)
				}
			default:
				t.Fatalf("unhandled mounted flow operation %q", testCase.Operation)
			}
		})
	}
}

func materializeMountedPaths(t *testing.T, fixtures []mountedPathFixture) map[string]string {
	t.Helper()
	root := t.TempDir()
	paths := make(map[string]string, len(fixtures)*2)
	for _, fixture := range fixtures {
		paths[fixture.Key] = filepath.Join(root, fixture.Key)
		if fixture.Target != "" {
			paths[fixture.Target] = filepath.Join(root, fixture.Target)
		}
	}
	for _, fixture := range fixtures {
		if fixture.State == mountedPathDirectory || fixture.State == mountedPathMoved {
			if err := os.MkdirAll(paths[fixture.Key], 0o755); err != nil {
				t.Fatalf("create real path %q: %v", fixture.Key, err)
			}
		}
	}
	for _, fixture := range fixtures {
		if fixture.State != mountedPathMoved {
			continue
		}
		if err := os.Rename(paths[fixture.Key], paths[fixture.Target]); err != nil {
			t.Fatalf("move real path %q to %q: %v", fixture.Key, fixture.Target, err)
		}
	}
	for _, fixture := range fixtures {
		if fixture.State != mountedPathSymlink {
			continue
		}
		if err := os.Symlink(paths[fixture.Target], paths[fixture.Key]); err != nil {
			t.Fatalf("symlink real path %q to %q: %v", fixture.Key, fixture.Target, err)
		}
	}
	return paths
}

func mountedListings(fixtures []mountedListingFixture, paths map[string]string) []ftue.SessionListing {
	listings := make([]ftue.SessionListing, 0, len(fixtures))
	for _, fixture := range fixtures {
		listings = append(listings, ftue.SessionListing{
			Harness:     fixture.Harness,
			ProjectName: fixture.ProjectName,
			GitRemote:   fixture.GitRemote,
			WorkingDir:  paths[fixture.PathKey],
			Branch:      fixture.Branch,
			SessionID:   fixture.SessionID,
			Title:       fixture.Title,
		})
	}
	return listings
}

func mountedSelection(fixture mountedSelectionFixture, paths map[string]string) config.SelectionConfig {
	selection := config.SelectionConfig{
		Mode:                  fixture.Mode,
		AutoIngestNewBranches: fixture.AutoIngestNewBranches,
	}
	if len(fixture.Projects) == 0 && len(fixture.Sessions) == 0 && len(fixture.BranchExclusions) == 0 && len(fixture.SessionExclusions) == 0 {
		return selection
	}
	selection.Harnesses = map[string]config.SelectionHarnessConfig{}
	for _, project := range fixture.Projects {
		harness := selection.Harnesses[project.Harness]
		configured := config.ProjectSelection{
			GitRemote: project.GitRemote,
			Name:      project.Name,
			Branches:  append([]string(nil), project.Branches...),
		}
		for _, pathKey := range project.PathKeys {
			configured.ClonePaths = append(configured.ClonePaths, paths[pathKey])
		}
		harness.Projects = append(harness.Projects, configured)
		selection.Harnesses[project.Harness] = harness
	}
	for _, sessions := range fixture.Sessions {
		harness := selection.Harnesses[sessions.Harness]
		harness.Sessions = append(harness.Sessions, sessions.IDs...)
		selection.Harnesses[sessions.Harness] = harness
	}
	for _, exclusion := range fixture.BranchExclusions {
		harness := selection.Harnesses[exclusion.Harness]
		harness.Exclusions.Branches = append(harness.Exclusions.Branches, config.BranchExclusion{
			ClonePath: paths[exclusion.PathKey],
			Branches:  append([]string(nil), exclusion.Branches...),
		})
		selection.Harnesses[exclusion.Harness] = harness
	}
	for _, sessions := range fixture.SessionExclusions {
		harness := selection.Harnesses[sessions.Harness]
		harness.Exclusions.Sessions = append(harness.Exclusions.Sessions, sessions.IDs...)
		selection.Harnesses[sessions.Harness] = harness
	}
	return selection
}

func assertMountedEditorForest(t *testing.T, roots []*kit.TreeNode, paths map[string]string, expect mountedFlowExpectation) {
	t.Helper()
	if len(roots) != len(expect.Roots) {
		t.Fatalf("mounted editor roots = %d, want %d", len(roots), len(expect.Roots))
	}
	rootsByPath := map[string]*kit.TreeNode{}
	for _, root := range roots {
		clonePath := root.Meta[settings.MetaClonePath]
		if clonePath == "" {
			t.Fatalf("mounted editor root %q has no physical clone path", root.Label)
		}
		if _, duplicate := rootsByPath[clonePath]; duplicate {
			t.Fatalf("mounted editor merged two roots at physical path %q", clonePath)
		}
		rootsByPath[clonePath] = root
		if len(root.Children) == 0 || len(root.Children[0].Children) == 0 {
			t.Fatalf("mounted editor root %q did not retain branch and session descendants", root.Label)
		}
	}
	for _, expectedRoot := range expect.Roots {
		physicalPath := paths[expectedRoot.PathKey]
		root := rootsByPath[physicalPath]
		if root == nil {
			t.Fatalf("mounted editor has no distinct root for physical path key %q (%s)", expectedRoot.PathKey, physicalPath)
		}
		assertMountedMultiplicity(t, root, expectedRoot.RemoteMultiplicity, expectedRoot.NameMultiplicity)
		walkMountedNodes(root, func(node *kit.TreeNode) {
			if node.Meta[settings.MetaHarness] != "" {
				assertMountedMultiplicity(t, node, expectedRoot.RemoteMultiplicity, expectedRoot.NameMultiplicity)
			}
		})
	}
	assertSameRemoteRootsStayDistinct(t, rootsByPath, paths, expect.SameRemotePathKeys)
	assertMountedSessionStates(t, roots, expect.InitiallyCheckedSessions, expect.InitiallyUncheckedSessions, "initial restoration")

	// Assert the editor input itself, not parity with a viewer list. Available
	// selected and unselected descendants stay editable, while a listing whose
	// physical path is unavailable does not invent an editor row. Its saved
	// identity is checked separately through the draft and committed config.
	sessions := mountedSessionNodes(t, roots)
	if got := sortedMountedKeys(sessions); !reflect.DeepEqual(got, sortedMountedStrings(expect.EditorSessions)) {
		t.Fatalf("mounted editor session inputs = %v, want %v", got, sortedMountedStrings(expect.EditorSessions))
	}
	for _, unavailable := range expect.UnavailableListings {
		if _, present := sessions[unavailable]; present {
			t.Fatalf("unavailable filesystem listing %q became an editor row", unavailable)
		}
	}
}

func assertMountedMultiplicity(t *testing.T, node *kit.TreeNode, remote, name string) {
	t.Helper()
	gotRemote := node.Meta[settings.MetaRemoteMultiplicity]
	gotName := node.Meta[settings.MetaNameMultiplicity]
	if gotRemote == "" || gotName == "" || gotRemote == settings.MetaMultiplicityUnproven || gotName == settings.MetaMultiplicityUnproven {
		t.Fatalf("node %q has zero or unproven multiplicity: remote=%q name=%q", node.ID, gotRemote, gotName)
	}
	if gotRemote != remote || gotName != name {
		t.Fatalf("node %q multiplicity = remote:%q name:%q, want remote:%q name:%q", node.ID, gotRemote, gotName, remote, name)
	}
}

func assertSameRemoteRootsStayDistinct(t *testing.T, rootsByPath map[string]*kit.TreeNode, paths map[string]string, pathKeys []string) {
	t.Helper()
	if len(pathKeys) == 0 {
		return
	}
	if len(pathKeys) < 2 {
		t.Fatalf("same-remote assertion needs at least two physical paths")
	}
	var remote string
	identities := map[string]struct{}{}
	for _, pathKey := range pathKeys {
		root := rootsByPath[paths[pathKey]]
		if root == nil {
			t.Fatalf("same-remote assertion has no root for path key %q", pathKey)
		}
		if remote == "" {
			remote = root.Meta[settings.MetaRemote]
		}
		if remote == "" || root.Meta[settings.MetaRemote] != remote {
			t.Fatalf("same-remote roots do not carry one populated remote: first=%q current=%q", remote, root.Meta[settings.MetaRemote])
		}
		if _, duplicate := identities[root.ID]; duplicate {
			t.Fatalf("same-remote physical roots share project identity %q", root.ID)
		}
		identities[root.ID] = struct{}{}
	}
}

func assertMountedSessionStates(t *testing.T, roots []*kit.TreeNode, wantChecked, wantUnchecked []string, stage string) {
	t.Helper()
	sessions := mountedSessionNodes(t, roots)
	var checked, unchecked []string
	for id, node := range sessions {
		switch node.State {
		case kit.Checked:
			checked = append(checked, id)
		case kit.Unchecked:
			unchecked = append(unchecked, id)
		default:
			t.Fatalf("%s session %q has non-binary state %s", stage, id, node.State)
		}
	}
	sort.Strings(checked)
	sort.Strings(unchecked)
	if !reflect.DeepEqual(checked, sortedMountedStrings(wantChecked)) {
		t.Fatalf("%s checked sessions = %v, want %v", stage, checked, sortedMountedStrings(wantChecked))
	}
	if !reflect.DeepEqual(unchecked, sortedMountedStrings(wantUnchecked)) {
		t.Fatalf("%s unchecked sessions = %v, want %v", stage, unchecked, sortedMountedStrings(wantUnchecked))
	}
}

func mountedSessionNodes(t *testing.T, roots []*kit.TreeNode) map[string]*kit.TreeNode {
	t.Helper()
	sessions := map[string]*kit.TreeNode{}
	for _, root := range roots {
		walkMountedNodes(root, func(node *kit.TreeNode) {
			if node.Meta[settings.MetaHarness] == "" {
				return
			}
			if _, duplicate := sessions[node.ID]; duplicate {
				t.Fatalf("mounted editor repeats session ID %q", node.ID)
			}
			sessions[node.ID] = node
		})
	}
	return sessions
}

func walkMountedNodes(node *kit.TreeNode, visit func(*kit.TreeNode)) {
	visit(node)
	for _, child := range node.Children {
		walkMountedNodes(child, visit)
	}
}

func sortedMountedKeys(values map[string]*kit.TreeNode) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedMountedStrings(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	if len(copy) == 0 {
		return nil
	}
	return copy
}

func editMountedRoot(t *testing.T, program kickstart.Program, roots []*kit.TreeNode, physicalPath string) kickstart.Program {
	t.Helper()
	rootIndex := -1
	for index, root := range roots {
		if root.Meta[settings.MetaClonePath] == physicalPath {
			rootIndex = index
			break
		}
	}
	if rootIndex < 0 {
		t.Fatalf("cannot edit missing mounted root at %q", physicalPath)
	}
	for index := 0; index < rootIndex; index++ {
		program = pressAndDrain(program, 'j')
	}
	return pressAndDrain(program, ' ')
}

func assertMountedSelection(t *testing.T, got, want config.SelectionConfig, stage string) {
	t.Helper()
	got = normalizedSelection(got)
	want = normalizedSelection(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mounted selection after %s mismatch\n got: %#v\nwant: %#v", stage, got, want)
	}
}

func cloneMountedSelection(selection config.SelectionConfig) config.SelectionConfig {
	copy := selection
	copy.Harnesses = make(map[string]config.SelectionHarnessConfig, len(selection.Harnesses))
	for harness, configured := range selection.Harnesses {
		cloned := config.SelectionHarnessConfig{Sessions: append([]string(nil), configured.Sessions...)}
		for _, project := range configured.Projects {
			project.ClonePaths = append([]string(nil), project.ClonePaths...)
			project.Branches = append([]string(nil), project.Branches...)
			cloned.Projects = append(cloned.Projects, project)
		}
		cloned.Exclusions.Sessions = append([]string(nil), configured.Exclusions.Sessions...)
		for _, exclusion := range configured.Exclusions.Branches {
			cloned.Exclusions.Branches = append(cloned.Exclusions.Branches, config.BranchExclusion{
				ClonePath: exclusion.ClonePath,
				Branches:  append([]string(nil), exclusion.Branches...),
			})
		}
		copy.Harnesses[harness] = cloned
	}
	if len(copy.Harnesses) == 0 {
		copy.Harnesses = nil
	}
	return copy
}
