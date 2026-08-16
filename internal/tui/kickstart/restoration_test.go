package kickstart_test

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/restoration.yaml
var restorationData []byte

type restorationCase struct {
	Name                   string                 `yaml:"name"`
	Listings               []ftue.SessionListing  `yaml:"listings"`
	Saved                  config.SelectionConfig `yaml:"saved"`
	ExpectInitiallyChecked []string               `yaml:"expectInitiallyChecked"`
	ClearProjectPaths      []string               `yaml:"clearProjectPaths"`
	SelectProjectPaths     []string               `yaml:"selectProjectPaths"`
	ClearBranches          []restorationBranch    `yaml:"clearBranches"`
	SelectAll              bool                   `yaml:"selectAll"`
	LaterListings          []ftue.SessionListing  `yaml:"laterListings"`
	ExpectLaterChecked     []string               `yaml:"expectLaterChecked"`
	Expected               config.SelectionConfig `yaml:"expected"`
}

type restorationBranch struct {
	ClonePath string `yaml:"clonePath"`
	Branch    string `yaml:"branch"`
}

type restorationDocument struct {
	ExpectedCaseCount int               `yaml:"expectedCaseCount"`
	Cases             []restorationCase `yaml:"cases"`
}

func loadRestorationDocument(t *testing.T) restorationDocument {
	t.Helper()
	var document restorationDocument
	decoder := yaml.NewDecoder(bytes.NewReader(restorationData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode testdata/restoration.yaml: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		t.Fatalf("restoration.yaml must hold exactly one document: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || len(document.Cases) == 0 {
		t.Fatalf("expectedCaseCount=%d but %d cases present", document.ExpectedCaseCount, len(document.Cases))
	}
	for _, testCase := range document.Cases {
		testutil.RequireFixtureFields(t, "restoration", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "saved.mode", Value: testCase.Saved.Mode.String()},
			{Key: "expected.mode", Value: testCase.Expected.Mode.String()},
		})
		if len(testCase.Listings) == 0 {
			t.Fatalf("restoration case %q has no scanner listings", testCase.Name)
		}
		if (len(testCase.LaterListings) == 0) != (len(testCase.ExpectLaterChecked) == 0) {
			t.Fatalf("restoration case %q must set laterListings and expectLaterChecked together", testCase.Name)
		}
	}
	return document
}

func TestPrepopulateSelection_RestorationAndMergeMatrix(t *testing.T) {
	t.Parallel()
	document := loadRestorationDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			source := kickstart.NewScannerTreeSource(testCase.Listings, withFixturePathResolver())
			roots, err := source.Load(context.Background())
			if err != nil {
				t.Fatalf("scanner load: %v", err)
			}

			unmatched := settings.PrepopulateSelection(roots, testCase.Saved)
			if got := checkedSessionIDs(roots); !reflect.DeepEqual(got, sortedCopy(testCase.ExpectInitiallyChecked)) {
				t.Fatalf("initially checked sessions = %v, want %v", got, sortedCopy(testCase.ExpectInitiallyChecked))
			}

			for _, clonePath := range testCase.ClearProjectPaths {
				setProjectPathState(t, roots, clonePath, kit.Unchecked)
			}
			for _, clonePath := range testCase.SelectProjectPaths {
				setProjectPathState(t, roots, clonePath, kit.Checked)
			}
			for _, branch := range testCase.ClearBranches {
				setBranchState(t, roots, branch, kit.Unchecked)
			}
			if testCase.SelectAll {
				for _, root := range roots {
					setNodeState(root, kit.Checked)
				}
			}
			for _, root := range roots {
				rollup(root)
			}

			derived := settings.FromTreeNodes(roots)
			merged := settings.MergeSelection(derived, unmatched)
			got := merged.ToSelectionConfig(testCase.Saved.AutoIngestNewBranches)
			got = normalizedSelection(got)
			want := normalizedSelection(testCase.Expected)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("merged selection mismatch\n got: %#v\nwant: %#v", got, want)
			}

			if len(testCase.LaterListings) > 0 {
				laterRoots, err := kickstart.NewScannerTreeSource(testCase.LaterListings, withFixturePathResolver()).Load(context.Background())
				if err != nil {
					t.Fatalf("later scanner load: %v", err)
				}
				settings.PrepopulateSelection(laterRoots, got)
				if later := checkedSessionIDs(laterRoots); !reflect.DeepEqual(later, sortedCopy(testCase.ExpectLaterChecked)) {
					t.Fatalf("later checked sessions = %v, want %v", later, sortedCopy(testCase.ExpectLaterChecked))
				}
			}
		})
	}
}

func TestMountedProgramRestoresSavedSelectionThroughProductionRegistry(t *testing.T) {
	t.Parallel()
	harness := string(defaults.HarnessClaudeCode)
	selectedPath := "/fixtures/team-a/tool"
	saved := config.SelectionConfig{
		Mode:                  config.SelectionModeSelected,
		AutoIngestNewBranches: false,
		Harnesses: map[string]config.SelectionHarnessConfig{
			harness: {
				Projects: []config.ProjectSelection{{
					GitRemote:  "git@github.com:acme/tool.git",
					ClonePaths: []string{selectedPath},
				}},
				Sessions: []string{"unavailable-session"},
			},
		},
	}
	configured := config.BaseConfig()
	configured.Selection = saved
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, configured); err != nil {
		t.Fatalf("save selected config: %v", err)
	}
	draft, err := settings.NewDraft(path, configured)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	listings := []ftue.SessionListing{
		{Harness: harness, ProjectName: "tool", GitRemote: "git@github.com:acme/tool.git", WorkingDir: selectedPath, Branch: "main", SessionID: "saved-session"},
		{Harness: harness, ProjectName: "new-tool", GitRemote: "git@github.com:acme/new-tool.git", WorkingDir: "/fixtures/team-a/new-tool", Branch: "main", SessionID: "new-session"},
	}
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:  theme.New(theme.ModeDark),
		Draft:  draft,
		Source: kickstart.NewScannerTreeSource(listings, withFixturePathResolver()),
	})
	program.SetSize(100, 24)
	program = declineOAuth(t, program)

	got := normalizedSelection(draft.Working().Selection)
	want := normalizedSelection(saved)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mounted selection restoration mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestMountedKickstartNoEditSaveKeepsExplicitSessionsScoped(t *testing.T) {
	testCase := restorationCaseNamed(t, loadRestorationDocument(t), "all-current-explicit-sessions-stay-session-scoped")
	configured := config.BaseConfig()
	configured.Selection = testCase.Saved
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, configured); err != nil {
		t.Fatalf("save explicit-session config: %v", err)
	}
	draft, err := settings.NewDraft(path, configured)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:  theme.New(theme.ModeDark),
		Draft:  draft,
		Source: kickstart.NewScannerTreeSource(testCase.Listings, withFixturePathResolver()),
	})
	program.SetSize(100, 24)
	program = declineOAuth(t, program)
	program, _ = advanceToCommit(program)
	if !program.Committed() {
		t.Fatalf("explicit-session no-edit save did not commit: phase=%s", program.Phase())
	}

	reloaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse explicit-session config: %v", err)
	}
	want := normalizedSelection(testCase.Expected)
	if got := normalizedSelection(reloaded.Selection); !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit-session no-edit save widened its grain\n got: %#v\nwant: %#v", got, want)
	}
	laterRoots, err := kickstart.NewScannerTreeSource(testCase.LaterListings, withFixturePathResolver()).Load(context.Background())
	if err != nil {
		t.Fatalf("load later scanner cohort: %v", err)
	}
	settings.PrepopulateSelection(laterRoots, reloaded.Selection)
	if got := checkedSessionIDs(laterRoots); !reflect.DeepEqual(got, sortedCopy(testCase.ExpectLaterChecked)) {
		t.Fatalf("later checked sessions = %v, want %v", got, sortedCopy(testCase.ExpectLaterChecked))
	}
}

func TestMountedKickstartSelectAllNamesProjectScopeAndCommitsCurrentClones(t *testing.T) {
	testCase := restorationCaseNamed(t, loadRestorationDocument(t), "select-all-saves-exact-current-clones")

	configured := config.BaseConfig()
	configured.Selection = testCase.Saved
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(path, configured); err != nil {
		t.Fatalf("save selected config: %v", err)
	}
	draft, err := settings.NewDraft(path, configured)
	if err != nil {
		t.Fatalf("open draft: %v", err)
	}
	program := kickstart.NewProgram(kickstart.ProgramDeps{
		Theme:  theme.New(theme.ModeDark),
		Draft:  draft,
		Source: kickstart.NewScannerTreeSource(testCase.Listings, withFixturePathResolver()),
	})
	program.SetSize(120, 28)
	program = declineOAuth(t, program)

	program = pressAndDrain(program, '?')
	if view := program.View(); !strings.Contains(view, "toggle all") {
		t.Fatalf("kickstart selection help does not name the select-all cycle:\n%s", view)
	}
	program = pressAndDrain(program, '?')
	program = pressAndDrain(program, 'a')
	gotWorking := normalizedSelection(draft.Working().Selection)
	want := normalizedSelection(testCase.Expected)
	if !reflect.DeepEqual(gotWorking, want) {
		t.Fatalf("mounted select-all working selection mismatch\n got: %#v\nwant: %#v", gotWorking, want)
	}

	program, _ = advanceToCommit(program)
	if !program.Committed() {
		t.Fatalf("mounted select-all did not commit: phase=%s", program.Phase())
	}
	reloaded, err := config.Parse(mustReadFile(t, path))
	if err != nil {
		t.Fatalf("parse committed selection: %v", err)
	}
	if got := normalizedSelection(reloaded.Selection); !reflect.DeepEqual(got, want) {
		t.Fatalf("mounted select-all committed selection mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func restorationCaseNamed(t *testing.T, document restorationDocument, name string) restorationCase {
	t.Helper()
	for _, testCase := range document.Cases {
		if testCase.Name == name {
			return testCase
		}
	}
	t.Fatalf("restoration fixture has no case %q", name)
	return restorationCase{}
}

func setBranchState(t *testing.T, roots []*kit.TreeNode, target restorationBranch, state kit.TriState) {
	t.Helper()
	for _, root := range roots {
		if root.Meta == nil || root.Meta[settings.MetaClonePath] != target.ClonePath {
			continue
		}
		for _, branch := range root.Children {
			if branch.Meta != nil && branch.Meta[settings.MetaBranch] == target.Branch {
				setNodeState(branch, state)
				return
			}
		}
	}
	t.Fatalf("branch %q for project path %q is not available", target.Branch, target.ClonePath)
}

func checkedSessionIDs(roots []*kit.TreeNode) []string {
	var checked []string
	var walk func(*kit.TreeNode)
	walk = func(node *kit.TreeNode) {
		if node.Meta != nil && node.Meta[settings.MetaHarness] != "" && node.State == kit.Checked {
			checked = append(checked, node.ID)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	sort.Strings(checked)
	return checked
}

func setProjectPathState(t *testing.T, roots []*kit.TreeNode, clonePath string, state kit.TriState) {
	t.Helper()
	for _, root := range roots {
		if root.Meta != nil && root.Meta[settings.MetaClonePath] == clonePath {
			setNodeState(root, state)
			return
		}
	}
	t.Fatalf("project path %q is not available in the scanner forest", clonePath)
}

func setNodeState(node *kit.TreeNode, state kit.TriState) {
	node.State = state
	for _, child := range node.Children {
		setNodeState(child, state)
	}
}

func sortedCopy(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	if len(copy) == 0 {
		return nil
	}
	return copy
}

func normalizedSelection(selection config.SelectionConfig) config.SelectionConfig {
	selection.DeprecatedProviders = nil
	if len(selection.Harnesses) == 0 {
		selection.Harnesses = nil
		return selection
	}
	for harness, configured := range selection.Harnesses {
		configured.Sessions = sortedCopy(configured.Sessions)
		for index := range configured.Projects {
			configured.Projects[index].ClonePaths = sortedCopy(configured.Projects[index].ClonePaths)
			configured.Projects[index].Branches = sortedCopy(configured.Projects[index].Branches)
		}
		sort.Slice(configured.Projects, func(i, j int) bool {
			left, right := configured.Projects[i], configured.Projects[j]
			return fmt.Sprintf("%s|%s|%v|%v", left.GitRemote, left.Name, left.ClonePaths, left.Branches) <
				fmt.Sprintf("%s|%s|%v|%v", right.GitRemote, right.Name, right.ClonePaths, right.Branches)
		})
		if len(configured.Projects) == 0 {
			configured.Projects = nil
		}
		selection.Harnesses[harness] = configured
	}
	return selection
}
