package kickstart_test

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
)

//go:embed testdata/repository_grouping.yaml
var repositoryGroupingData []byte

type repositoryGroupingAction string

const (
	repositoryGroupingNone    repositoryGroupingAction = "none"
	repositoryGroupingProject repositoryGroupingAction = "project"
	repositoryGroupingBranch  repositoryGroupingAction = "branch"
)

type repositoryGroupingDocument struct {
	ExpectedCaseCount int                      `yaml:"expectedCaseCount"`
	ExpectedNames     []string                 `yaml:"expectedNames"`
	Cases             []repositoryGroupingCase `yaml:"cases"`
}

type repositoryGroupingCase struct {
	Name                 string                      `yaml:"name"`
	Repositories         []repositoryPathFixture     `yaml:"repositories"`
	Listings             []ftue.SessionListing       `yaml:"listings"`
	ExpectedRoots        []repositoryRootExpectation `yaml:"expectedRoots"`
	Saved                config.SelectionConfig      `yaml:"saved"`
	Action               repositoryGroupingAction    `yaml:"action"`
	ActionRepositoryPath string                      `yaml:"actionRepositoryPath"`
	ActionBranch         string                      `yaml:"actionBranch"`
	ExpectedSelection    config.SelectionConfig      `yaml:"expectedSelection"`
	ExpectedChecked      []string                    `yaml:"expectedChecked"`
	ExpectedUnchecked    []string                    `yaml:"expectedUnchecked"`
}

type repositoryPathFixture struct {
	ClonePath      string `yaml:"clonePath"`
	RepositoryPath string `yaml:"repositoryPath"`
	Fail           bool   `yaml:"fail"`
}

type repositoryRootExpectation struct {
	RepositoryPath     string              `yaml:"repositoryPath"`
	ClonePaths         []string            `yaml:"clonePaths"`
	Branches           map[string][]string `yaml:"branches"`
	RemoteMultiplicity string              `yaml:"remoteMultiplicity"`
}

func loadRepositoryGroupingDocument(t *testing.T) repositoryGroupingDocument {
	t.Helper()
	var document repositoryGroupingDocument
	if err := decodeStrictFixture(repositoryGroupingData, &document); err != nil {
		t.Fatalf("decode repository grouping fixture: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || document.ExpectedCaseCount != len(document.ExpectedNames) || len(document.Cases) == 0 {
		t.Fatalf("repository grouping manifest count=%d names=%d cases=%d", document.ExpectedCaseCount, len(document.ExpectedNames), len(document.Cases))
	}
	seen := map[string]bool{}
	for index, testCase := range document.Cases {
		testutil.RequireFixtureFields(t, "repository grouping", testCase.Name, []testutil.FixtureField{
			{Key: "name", Value: testCase.Name},
			{Key: "action", Value: string(testCase.Action)},
			{Key: "expectedSelection.mode", Value: testCase.ExpectedSelection.Mode.String()},
		})
		if testCase.Name != document.ExpectedNames[index] {
			t.Fatalf("case[%d] name=%q, manifest=%q", index, testCase.Name, document.ExpectedNames[index])
		}
		if seen[testCase.Name] {
			t.Fatalf("duplicate repository grouping case %q", testCase.Name)
		}
		seen[testCase.Name] = true
		if len(testCase.Repositories) == 0 || len(testCase.Listings) == 0 || len(testCase.ExpectedRoots) == 0 {
			t.Fatalf("repository grouping case %q requires repositories, listings, and roots", testCase.Name)
		}
		if testCase.Action != repositoryGroupingNone && testCase.Action != repositoryGroupingProject && testCase.Action != repositoryGroupingBranch {
			t.Fatalf("repository grouping case %q has unknown action %q", testCase.Name, testCase.Action)
		}
		if testCase.Action != repositoryGroupingNone && testCase.ActionRepositoryPath == "" {
			t.Fatalf("repository grouping case %q has no action repository path", testCase.Name)
		}
		if testCase.Action == repositoryGroupingBranch && testCase.ActionBranch == "" {
			t.Fatalf("repository grouping case %q has no action branch", testCase.Name)
		}
	}
	return document
}

type fixtureRepositoryPathResolver struct {
	paths map[ingest.ClonePath]repositoryPathFixture
}

func (r fixtureRepositoryPathResolver) ResolveRepositoryPath(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryPath, error) {
	fixture, ok := r.paths[clonePath]
	if !ok {
		return "", fmt.Errorf("fixture has no repository identity for clone %q", clonePath)
	}
	if fixture.Fail {
		return "", fmt.Errorf("fixture marks clone %q as non-Git", clonePath)
	}
	return ingest.RepositoryPath(fixture.RepositoryPath), nil
}

var _ ingest.RepositoryPathResolver = fixtureRepositoryPathResolver{}

func TestScannerRepositoryGroupingAndExactRoundTrip(t *testing.T) {
	t.Parallel()
	document := loadRepositoryGroupingDocument(t)
	for _, testCase := range document.Cases {
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			resolver := fixtureRepositoryResolver(testCase.Repositories)
			source := repositoryGroupingSource(testCase.Listings, resolver)
			roots := loadRepositoryGroupingRoots(t, source)
			assertRepositoryGroupingRoots(t, roots, testCase.ExpectedRoots)
			candidates, err := source.CommitGateCandidates(nil)
			if err != nil {
				t.Fatalf("build repository grouping commit-gate candidates: %v", err)
			}
			assertRepositoryGroupingCandidates(t, candidates, testCase.ExpectedRoots)

			selection := testCase.Saved
			if !selection.Mode.IsValid() {
				selection = config.SelectionConfig{Mode: config.SelectionModeSelected}
			}
			settings.PrepopulateSelection(roots, selection)
			switch testCase.Action {
			case repositoryGroupingProject:
				setNodeState(repositoryRootByPath(t, roots, testCase.ActionRepositoryPath), kit.Checked)
			case repositoryGroupingBranch:
				root := repositoryRootByPath(t, roots, testCase.ActionRepositoryPath)
				setNodeState(repositoryBranchByName(t, root, testCase.ActionBranch), kit.Checked)
			case repositoryGroupingNone:
				// Restoration-only row.
			}
			for _, root := range roots {
				rollup(root)
			}

			got := selection
			if testCase.Action != repositoryGroupingNone {
				got = settings.FromTreeNodes(roots).ToSelectionConfig(selection.AutoIngestNewBranches)
			}
			if got = normalizedSelection(got); !reflect.DeepEqual(got, normalizedSelection(testCase.ExpectedSelection)) {
				t.Fatalf("exact grouped selection mismatch\n got: %#v\nwant: %#v", got, normalizedSelection(testCase.ExpectedSelection))
			}

			reopened := loadRepositoryGroupingRoots(t, repositoryGroupingSource(testCase.Listings, resolver))
			settings.PrepopulateSelection(reopened, testCase.ExpectedSelection)
			if checked := checkedSessionIDs(reopened); !reflect.DeepEqual(checked, sortedCopy(testCase.ExpectedChecked)) {
				t.Fatalf("reopened checked sessions=%v, want %v", checked, sortedCopy(testCase.ExpectedChecked))
			}
			assertUncheckedSessionIDs(t, reopened, testCase.ExpectedUnchecked)
		})
	}
}

func TestProductionRepositoryResolverGroupsRealLinkedWorktreeButNotIndependentClone(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	main := filepath.Join(root, "main")
	linked := filepath.Join(root, "linked")
	independent := filepath.Join(root, "independent")

	runRepositoryGit(t, root, "init", "--bare", origin)
	runRepositoryGit(t, root, "init", "--initial-branch=main", main)
	runRepositoryGit(t, main, "config", "user.email", "fixture@example.invalid")
	runRepositoryGit(t, main, "config", "user.name", "Fixture User")
	runRepositoryGit(t, main, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(main, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatalf("write main repository fixture: %v", err)
	}
	runRepositoryGit(t, main, "add", "README.md")
	runRepositoryGit(t, main, "commit", "-m", "initial fixture")
	runRepositoryGit(t, main, "remote", "add", "origin", origin)
	runRepositoryGit(t, main, "push", "-u", "origin", "main")
	runRepositoryGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	runRepositoryGit(t, main, "worktree", "add", "-b", "linked", linked)
	runRepositoryGit(t, root, "clone", origin, independent)

	pathResolver := ingest.NewPhysicalPathResolver()
	repositoryResolver := ingest.NewGitRepositoryPathResolver()
	mainClone, err := pathResolver.Resolve(main)
	if err != nil {
		t.Fatalf("resolve main clone path: %v", err)
	}
	linkedClone, err := pathResolver.Resolve(linked)
	if err != nil {
		t.Fatalf("resolve linked clone path: %v", err)
	}
	independentClone, err := pathResolver.Resolve(independent)
	if err != nil {
		t.Fatalf("resolve independent clone path: %v", err)
	}
	mainRepository, err := repositoryResolver.ResolveRepositoryPath(context.Background(), mainClone)
	if err != nil {
		t.Fatalf("resolve main repository path: %v", err)
	}
	linkedRepository, err := repositoryResolver.ResolveRepositoryPath(context.Background(), linkedClone)
	if err != nil {
		t.Fatalf("resolve linked repository path: %v", err)
	}
	independentRepository, err := repositoryResolver.ResolveRepositoryPath(context.Background(), independentClone)
	if err != nil {
		t.Fatalf("resolve independent repository path: %v", err)
	}
	if mainClone == linkedClone || mainClone == independentClone || linkedClone == independentClone {
		t.Fatalf("exact worktree paths are not distinct: main=%q linked=%q independent=%q", mainClone, linkedClone, independentClone)
	}
	if mainRepository != linkedRepository {
		t.Fatalf("main and linked repository paths differ: %q != %q", mainRepository, linkedRepository)
	}
	if independentRepository == mainRepository {
		t.Fatalf("independent clone reused repository path %q", independentRepository)
	}

	listings := []ftue.SessionListing{
		{Harness: "claude-code", ProjectName: "tool", GitRemote: origin, WorkingDir: main, Branch: "main", SessionID: "11111111-1111-4111-8111-111111111111"},
		{Harness: "claude-code", ProjectName: "tool", GitRemote: origin, WorkingDir: linked, Branch: "linked", SessionID: "22222222-2222-4222-8222-222222222222"},
		{Harness: "claude-code", ProjectName: "tool", GitRemote: origin, WorkingDir: independent, Branch: "main", SessionID: "33333333-3333-4333-8333-333333333333"},
	}
	prepared := kickstart.PrepareSessionListings(listings, pathResolver)
	if len(prepared) != len(listings) {
		t.Fatalf("prepared listings=%d, want %d", len(prepared), len(listings))
	}
	if prepared[0].Candidate.ClonePath != mainClone || prepared[1].Candidate.ClonePath != linkedClone || prepared[2].Candidate.ClonePath != independentClone {
		t.Fatalf("prepared exact paths changed: %#v", prepared)
	}
	if prepared[0].RepositoryIdentity.RepositoryPath != mainRepository || prepared[1].RepositoryIdentity.RepositoryPath != mainRepository {
		t.Fatalf("prepared linked repository identities differ: %#v", prepared)
	}
	if prepared[2].RepositoryIdentity.RepositoryPath != independentRepository {
		t.Fatalf("prepared independent repository identity=%q, want %q", prepared[2].RepositoryIdentity.RepositoryPath, independentRepository)
	}

	roots, err := kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(pathResolver),
		kickstart.WithRepositoryPathResolver(repositoryResolver),
	).Load(context.Background())
	if err != nil {
		t.Fatalf("load real Git repository forest: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("real Git repository roots=%d, want 2", len(roots))
	}
	grouped := repositoryRootByPath(t, roots, mainRepository.String())
	paths, _, _ := repositoryRootEvidence(grouped)
	if want := sortedCopy([]string{mainClone.String(), linkedClone.String()}); !reflect.DeepEqual(paths, want) {
		t.Fatalf("grouped real worktree paths=%v, want %v", paths, want)
	}
	independentRoot := repositoryRootByPath(t, roots, independentRepository.String())
	paths, _, _ = repositoryRootEvidence(independentRoot)
	if want := []string{independentClone.String()}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("independent real clone paths=%v, want %v", paths, want)
	}
}

func runRepositoryGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %q: %v\n%s", args, dir, err, output)
	}
}

func fixtureRepositoryResolver(fixtures []repositoryPathFixture) fixtureRepositoryPathResolver {
	paths := make(map[ingest.ClonePath]repositoryPathFixture, len(fixtures))
	for _, fixture := range fixtures {
		paths[ingest.ClonePath(fixture.ClonePath)] = fixture
	}
	return fixtureRepositoryPathResolver{paths: paths}
}

func repositoryGroupingSource(
	listings []ftue.SessionListing,
	repositoryResolver ingest.RepositoryPathResolver,
) *kickstart.ScannerTreeSource {
	return kickstart.NewScannerTreeSource(
		listings,
		withFixturePathResolver(),
		kickstart.WithRepositoryPathResolver(repositoryResolver),
	)
}

func loadRepositoryGroupingRoots(t *testing.T, source *kickstart.ScannerTreeSource) []*kit.TreeNode {
	t.Helper()
	roots, err := source.Load(context.Background())
	if err != nil {
		t.Fatalf("load repository grouping forest: %v", err)
	}
	return roots
}

func assertRepositoryGroupingCandidates(
	t *testing.T,
	candidates []selectionprojection.ProjectCandidate,
	expected []repositoryRootExpectation,
) {
	t.Helper()
	if len(candidates) != len(expected) {
		t.Fatalf("repository commit-gate candidates=%d, want %d", len(candidates), len(expected))
	}
	byRepository := make(map[string]selectionprojection.ProjectCandidate, len(candidates))
	for _, candidate := range candidates {
		path := candidate.RepositoryPath.String()
		if _, duplicate := byRepository[path]; duplicate {
			t.Fatalf("repository path %q appears in more than one commit-gate candidate", path)
		}
		byRepository[path] = candidate
	}
	for _, want := range expected {
		candidate, ok := byRepository[want.RepositoryPath]
		if !ok {
			t.Fatalf("missing commit-gate candidate for repository %q", want.RepositoryPath)
		}
		pathSet := map[string]struct{}{}
		for _, descendant := range candidate.Descendants {
			pathSet[descendant.ClonePath.String()] = struct{}{}
			if descendant.RepositoryPath != candidate.RepositoryPath {
				t.Fatalf("repository %q descendant %q carries repository path %q", want.RepositoryPath, descendant.SessionID, descendant.RepositoryPath)
			}
		}
		paths := make([]string, 0, len(pathSet))
		for path := range pathSet {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		if !reflect.DeepEqual(paths, sortedCopy(want.ClonePaths)) {
			t.Fatalf("repository %q commit-gate descendant paths=%v, want %v", want.RepositoryPath, paths, sortedCopy(want.ClonePaths))
		}
		if len(want.ClonePaths) == 1 && candidate.ClonePath.String() != want.ClonePaths[0] {
			t.Fatalf("single-worktree repository %q candidate path=%q, want %q", want.RepositoryPath, candidate.ClonePath, want.ClonePaths[0])
		}
		if len(want.ClonePaths) > 1 && candidate.ClonePath != "" {
			t.Fatalf("multi-worktree repository %q retained misleading candidate path %q", want.RepositoryPath, candidate.ClonePath)
		}
	}
}

func assertRepositoryGroupingRoots(t *testing.T, roots []*kit.TreeNode, expected []repositoryRootExpectation) {
	t.Helper()
	if len(roots) != len(expected) {
		t.Fatalf("repository roots=%d, want %d", len(roots), len(expected))
	}
	byRepository := make(map[string]*kit.TreeNode, len(roots))
	for _, root := range roots {
		path := root.Meta[settings.MetaRepositoryPath]
		if _, duplicate := byRepository[path]; duplicate {
			t.Fatalf("repository path %q appears in more than one root", path)
		}
		byRepository[path] = root
	}
	for _, want := range expected {
		root := byRepository[want.RepositoryPath]
		if root == nil {
			t.Fatalf("missing repository root %q; got %v", want.RepositoryPath, repositoryRootPaths(roots))
		}
		wantIdentity := (kickstart.RepositoryIdentity{
			Harness:        ingest.Harness(root.Meta[settings.MetaProjectHarness]),
			RepositoryPath: ingest.RepositoryPath(want.RepositoryPath),
		}).String()
		if root.ID != wantIdentity || root.Meta[settings.MetaProjectIdentity] != wantIdentity {
			t.Fatalf("root identity id=%q meta=%q, want %q", root.ID, root.Meta[settings.MetaProjectIdentity], wantIdentity)
		}
		clonePaths, branches, sessionCount := repositoryRootEvidence(root)
		if !reflect.DeepEqual(clonePaths, sortedCopy(want.ClonePaths)) {
			t.Fatalf("root %q clone paths=%v, want %v", want.RepositoryPath, clonePaths, sortedCopy(want.ClonePaths))
		}
		if !reflect.DeepEqual(branches, sortedPathMap(want.Branches)) {
			t.Fatalf("root %q branch paths=%v, want %v", want.RepositoryPath, branches, sortedPathMap(want.Branches))
		}
		if sessionCount == 0 {
			t.Fatalf("root %q has no exact session evidence", want.RepositoryPath)
		}
		if root.Meta[settings.MetaRemoteMultiplicity] != want.RemoteMultiplicity {
			t.Fatalf("root %q remote multiplicity=%q, want %q", want.RepositoryPath, root.Meta[settings.MetaRemoteMultiplicity], want.RemoteMultiplicity)
		}
		if len(want.ClonePaths) == 1 && root.Meta[settings.MetaClonePath] != want.ClonePaths[0] {
			t.Fatalf("single-worktree root clone path=%q, want %q", root.Meta[settings.MetaClonePath], want.ClonePaths[0])
		}
		if len(want.ClonePaths) > 1 && root.Meta[settings.MetaClonePath] != "" {
			t.Fatalf("multi-worktree root retained misleading clone path %q", root.Meta[settings.MetaClonePath])
		}
	}
}

func repositoryRootEvidence(root *kit.TreeNode) ([]string, map[string][]string, int) {
	pathSet := map[string]struct{}{}
	branches := map[string][]string{}
	sessions := 0
	for _, branch := range root.Children {
		branchName := branch.Meta[settings.MetaBranch]
		for _, session := range branch.Children {
			clonePath := session.Meta[settings.MetaClonePath]
			pathSet[clonePath] = struct{}{}
			if !containsFixtureString(branches[branchName], clonePath) {
				branches[branchName] = append(branches[branchName], clonePath)
			}
			sessions++
		}
	}
	paths := make([]string, 0, len(pathSet))
	for path := range pathSet {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, sortedPathMap(branches), sessions
}

func sortedPathMap(source map[string][]string) map[string][]string {
	result := make(map[string][]string, len(source))
	for branch, paths := range source {
		result[branch] = sortedCopy(paths)
	}
	return result
}

func containsFixtureString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func repositoryRootPaths(roots []*kit.TreeNode) []string {
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Meta[settings.MetaRepositoryPath])
	}
	sort.Strings(paths)
	return paths
}

func repositoryRootByPath(t *testing.T, roots []*kit.TreeNode, repositoryPath string) *kit.TreeNode {
	t.Helper()
	for _, root := range roots {
		if root.Meta[settings.MetaRepositoryPath] == repositoryPath {
			return root
		}
	}
	t.Fatalf("repository root %q is unavailable", repositoryPath)
	return nil
}

func repositoryBranchByName(t *testing.T, root *kit.TreeNode, branchName string) *kit.TreeNode {
	t.Helper()
	for _, branch := range root.Children {
		if branch.Meta[settings.MetaBranch] == branchName {
			return branch
		}
	}
	t.Fatalf("branch %q is unavailable under repository %q", branchName, root.Meta[settings.MetaRepositoryPath])
	return nil
}

func assertUncheckedSessionIDs(t *testing.T, roots []*kit.TreeNode, expected []string) {
	t.Helper()
	want := make(map[string]bool, len(expected))
	for _, sessionID := range expected {
		want[sessionID] = true
	}
	for _, root := range roots {
		for _, branch := range root.Children {
			for _, session := range branch.Children {
				if want[session.ID] && session.State != kit.Unchecked {
					t.Fatalf("session %q state=%v, want unchecked", session.ID, session.State)
				}
			}
		}
	}
}
