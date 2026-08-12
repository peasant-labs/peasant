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
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/selectionprojection"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"gopkg.in/yaml.v3"
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
	ExpectedCaseCount            int                      `yaml:"expectedCaseCount"`
	ExpectedTopologyFailureCount int                      `yaml:"expectedTopologyFailureCount"`
	ExpectedNames                []string                 `yaml:"expectedNames"`
	Cases                        []repositoryGroupingCase `yaml:"cases"`
	TopologyFailures             []topologyFailureFixture `yaml:"topologyFailures"`
}

type topologyFailureFixture struct {
	Name         string `yaml:"name"`
	FailurePoint string `yaml:"failurePoint"`
	ClonePath    string `yaml:"clonePath"`
	SiblingPath  string `yaml:"siblingPath"`
	GitRemote    string `yaml:"gitRemote"`
	ProjectName  string `yaml:"projectName"`
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
	Facet                *repositoryFacetExpectation `yaml:"facet"`
}

type repositoryPathFixture struct {
	ClonePath      string `yaml:"clonePath"`
	CohortKey      string `yaml:"cohortKey"`
	RepositoryPath string `yaml:"repositoryPath"`
	Fail           bool   `yaml:"fail"`
}

type repositoryRootExpectation struct {
	CohortKey           string              `yaml:"cohortKey"`
	RepositoryPath      string              `yaml:"repositoryPath"`
	ClonePaths          []string            `yaml:"clonePaths"`
	Remotes             []string            `yaml:"remotes"`
	CandidateClonePaths map[string][]string `yaml:"candidateClonePaths"`
	Branches            map[string][]string `yaml:"branches"`
	RemoteMultiplicity  string              `yaml:"remoteMultiplicity"`
}

type repositoryFacetExpectation struct {
	Harness             string   `yaml:"harness"`
	Project             string   `yaml:"project"`
	Branch              string   `yaml:"branch"`
	WantSessions        []string `yaml:"wantSessions"`
	WantMissingSessions []string `yaml:"wantMissingSessions"`
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
	if document.ExpectedTopologyFailureCount != len(document.TopologyFailures) || len(document.TopologyFailures) == 0 {
		t.Fatalf("repository topology failure manifest count=%d cases=%d", document.ExpectedTopologyFailureCount, len(document.TopologyFailures))
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
		for _, root := range testCase.ExpectedRoots {
			if root.RepositoryPath == "" || len(root.ClonePaths) == 0 || len(root.CandidateClonePaths) == 0 {
				t.Fatalf("repository grouping case %q root %q requires repository, root paths, and harness candidate paths", testCase.Name, root.RepositoryPath)
			}
		}
		fixtureByClonePath := make(map[string]repositoryPathFixture, len(testCase.Repositories))
		for _, repository := range testCase.Repositories {
			if repository.ClonePath == "" {
				t.Fatalf("repository grouping case %q has an empty fixture clone path", testCase.Name)
			}
			if _, duplicate := fixtureByClonePath[repository.ClonePath]; duplicate {
				t.Fatalf("repository grouping case %q repeats fixture clone path %q", testCase.Name, repository.ClonePath)
			}
			fixtureByClonePath[repository.ClonePath] = repository
		}
		for _, listing := range testCase.Listings {
			if _, ok := fixtureByClonePath[listing.WorkingDir]; !ok {
				t.Fatalf("repository grouping case %q listing %q has no repository fixture for %q", testCase.Name, listing.SessionID, listing.WorkingDir)
			}
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
		if testCase.Facet != nil {
			testutil.RequireFixtureFields(t, "repository grouping facet", testCase.Name, []testutil.FixtureField{
				{Key: "facet.harness", Value: testCase.Facet.Harness},
				{Key: "facet.project", Value: testCase.Facet.Project},
				{Key: "facet.branch", Value: testCase.Facet.Branch},
			})
			if len(testCase.Facet.WantSessions) == 0 || len(testCase.Facet.WantMissingSessions) == 0 {
				t.Fatalf("repository grouping facet case %q needs visible and hidden session rows", testCase.Name)
			}
		}
	}
	failureNames := make(map[string]bool, len(document.TopologyFailures))
	for _, failure := range document.TopologyFailures {
		testutil.RequireFixtureFields(t, "repository topology failure", failure.Name, []testutil.FixtureField{
			{Key: "name", Value: failure.Name},
			{Key: "failurePoint", Value: failure.FailurePoint},
			{Key: "clonePath", Value: failure.ClonePath},
			{Key: "siblingPath", Value: failure.SiblingPath},
			{Key: "gitRemote", Value: failure.GitRemote},
			{Key: "projectName", Value: failure.ProjectName},
		})
		if failureNames[failure.Name] {
			t.Fatalf("duplicate repository topology failure case %q", failure.Name)
		}
		failureNames[failure.Name] = true
	}
	return document
}

type fixtureRepositoryIdentityResolver struct {
	paths map[ingest.ClonePath]repositoryPathFixture
}

func (r fixtureRepositoryIdentityResolver) ResolveRepositoryIdentity(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	fixture, ok := r.paths[clonePath]
	if !ok {
		return ingest.RepositoryIdentity{}, fmt.Errorf("fixture has no repository identity for clone %q", clonePath)
	}
	if fixture.Fail {
		return ingest.RepositoryIdentity{}, fmt.Errorf("fixture marks clone %q as non-Git", clonePath)
	}
	cohortKey := fixture.CohortKey
	if cohortKey == "" {
		cohortKey = "fixture:" + fixture.RepositoryPath
	}
	return ingest.RepositoryIdentity{
		CohortKey:    ingest.RepositoryCohortKey(cohortKey),
		GitDirectory: ingest.RepositoryPath(fixture.RepositoryPath),
	}, nil
}

var _ ingest.RepositoryIdentityResolver = fixtureRepositoryIdentityResolver{}

type failingRepositoryIdentityResolver struct{}

func (failingRepositoryIdentityResolver) ResolveRepositoryIdentity(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	return ingest.RepositoryIdentity{}, fmt.Errorf("fixture topology failure for %q", clonePath)
}

var _ ingest.RepositoryIdentityResolver = failingRepositoryIdentityResolver{}

type failingTopologyWithGitDirectoryResolver map[ingest.ClonePath]ingest.RepositoryPath

func (r failingTopologyWithGitDirectoryResolver) ResolveRepositoryIdentity(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	gitDirectory, ok := r[clonePath]
	if !ok {
		return ingest.RepositoryIdentity{}, fmt.Errorf("fixture has no physical Git directory for %q", clonePath)
	}
	return ingest.RepositoryIdentity{GitDirectory: gitDirectory}, fmt.Errorf("fixture topology failure after resolving Git directory %q", gitDirectory)
}

var _ ingest.RepositoryIdentityResolver = failingTopologyWithGitDirectoryResolver{}

func TestScannerRepositoryTopologyFailuresFallBackToExactClonePaths(t *testing.T) {
	t.Parallel()
	document := loadRepositoryGroupingDocument(t)
	for _, failure := range document.TopologyFailures {
		failure := failure
		t.Run(failure.Name, func(t *testing.T) {
			t.Parallel()
			listings := []ftue.SessionListing{
				{Harness: string(defaults.HarnessClaudeCode), ProjectName: failure.ProjectName, GitRemote: failure.GitRemote, WorkingDir: failure.ClonePath, Branch: "main", SessionID: "12121212-1212-4212-8212-121212121212"},
				{Harness: string(defaults.HarnessClaudeCode), ProjectName: failure.ProjectName, GitRemote: failure.GitRemote, WorkingDir: failure.SiblingPath, Branch: "main", SessionID: "34343434-3434-4434-8434-343434343434"},
			}
			source := kickstart.NewScannerTreeSource(
				listings,
				withFixturePathResolver(),
				kickstart.WithRepositoryIdentityResolver(failingRepositoryIdentityResolver{}),
			)
			roots := loadRepositoryGroupingRoots(t, source)
			if len(roots) != 2 {
				t.Fatalf("failure point %q roots=%d, want 2 exact-path roots", failure.FailurePoint, len(roots))
			}
			candidates, err := source.CommitGateCandidates(nil)
			if err != nil {
				t.Fatalf("failure point %q commit-gate candidates: %v", failure.FailurePoint, err)
			}
			if len(candidates) != 2 {
				t.Fatalf("failure point %q commit-gate candidates=%d, want 2", failure.FailurePoint, len(candidates))
			}
			byPath := make(map[string]selectionprojection.ProjectCandidate, len(candidates))
			for _, candidate := range candidates {
				byPath[candidate.RepositoryCohortKey.String()] = candidate
			}
			for index, wantPath := range []string{failure.ClonePath, failure.SiblingPath} {
				root := repositoryRootByCohortKey(t, roots, wantPath)
				if got := repositoryRootGitDirectories(root); !reflect.DeepEqual(got, []string{wantPath}) {
					t.Fatalf("failure point %q row %d Git directories=%v, want exact path %q", failure.FailurePoint, index, got, wantPath)
				}
				candidate, ok := byPath[wantPath]
				if !ok {
					t.Fatalf("failure point %q lacks exact candidate path %q", failure.FailurePoint, wantPath)
				}
				if candidate.GitRemote != "" || candidate.ProjectName != "" {
					t.Fatalf("failure point %q row %d exposed remote/name fallback: %#v", failure.FailurePoint, index, candidate)
				}
			}
		})
	}
}

func TestScannerRepositoryTopologyFailureUsesPhysicalGitDirectoryCohort(t *testing.T) {
	t.Parallel()
	cloneA := ingest.ClonePath("/fixtures/failures/worktree-a")
	cloneB := ingest.ClonePath("/fixtures/failures/worktree-b")
	gitDirectory := ingest.RepositoryPath("/fixtures/failures/shared.git")
	resolver := failingTopologyWithGitDirectoryResolver{cloneA: gitDirectory, cloneB: gitDirectory}
	listings := []ftue.SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "tool", GitRemote: "git@github.com:acme/tool.git", WorkingDir: cloneA.String(), Branch: "main", SessionID: "56565656-5656-4656-8656-565656565656"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "tool", GitRemote: "git@github.com:acme/tool.git", WorkingDir: cloneB.String(), Branch: "review", SessionID: "78787878-7878-4878-8878-787878787878"},
	}
	source := kickstart.NewScannerTreeSource(listings, withFixturePathResolver(), kickstart.WithRepositoryIdentityResolver(resolver))
	roots := loadRepositoryGroupingRoots(t, source)
	if len(roots) != 1 {
		t.Fatalf("physical Git-directory fallback roots=%d, want 1", len(roots))
	}
	root := roots[0]
	if root.Meta[settings.MetaProjectIdentity] != gitDirectory.String() || !reflect.DeepEqual(repositoryRootGitDirectories(root), []string{gitDirectory.String()}) {
		t.Fatalf("physical Git-directory fallback root=%#v directories=%v", root.Meta, repositoryRootGitDirectories(root))
	}
	paths, _, _ := repositoryRootEvidence(root)
	if want := []string{cloneA.String(), cloneB.String()}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("physical Git-directory fallback exact paths=%v, want %v", paths, want)
	}
}

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
			assertRepositoryGroupingPreviewContexts(t, source, roots, testCase.ExpectedRoots)
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

func TestScannerRepositoryGroupingFacetKeepsSharedAncestors(t *testing.T) {
	t.Parallel()
	document := loadRepositoryGroupingDocument(t)
	for _, testCase := range document.Cases {
		if testCase.Facet == nil {
			continue
		}
		testCase := testCase
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			source := repositoryGroupingSource(testCase.Listings, fixtureRepositoryResolver(testCase.Repositories))
			program, _ := newTestProgram(t, kickstart.ProgramDeps{Source: source})
			program.SetSize(120, 30)
			program = declineOAuth(t, program)

			harnesses := repositoryListingHarnesses(testCase.Listings)
			facetIndex := -1
			for index, harness := range harnesses {
				if harness == testCase.Facet.Harness {
					facetIndex = index
					break
				}
			}
			if facetIndex < 0 {
				t.Fatalf("facet harness %q is not present in %v", testCase.Facet.Harness, harnesses)
			}
			for index := 0; index <= facetIndex; index++ {
				program, _ = program.Update(press('f'))
			}
			view := stripRender(program.View())
			for _, want := range append([]string{testCase.Facet.Project, testCase.Facet.Branch}, testCase.Facet.WantSessions...) {
				if !strings.Contains(view, want) {
					t.Fatalf("facet %q must retain %q; view:\n%s", testCase.Facet.Harness, want, view)
				}
			}
			for _, unwanted := range testCase.Facet.WantMissingSessions {
				if strings.Contains(view, unwanted) {
					t.Fatalf("facet %q retained hidden session %q; view:\n%s", testCase.Facet.Harness, unwanted, view)
				}
			}
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
	repositoryResolver := ingest.NewGitRepositoryIdentityResolver()
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
	mainRepository, err := repositoryResolver.ResolveRepositoryIdentity(context.Background(), mainClone)
	if err != nil {
		t.Fatalf("resolve main repository path: %v", err)
	}
	linkedRepository, err := repositoryResolver.ResolveRepositoryIdentity(context.Background(), linkedClone)
	if err != nil {
		t.Fatalf("resolve linked repository path: %v", err)
	}
	independentRepository, err := repositoryResolver.ResolveRepositoryIdentity(context.Background(), independentClone)
	if err != nil {
		t.Fatalf("resolve independent repository path: %v", err)
	}
	if mainClone == linkedClone || mainClone == independentClone || linkedClone == independentClone {
		t.Fatalf("exact worktree paths are not distinct: main=%q linked=%q independent=%q", mainClone, linkedClone, independentClone)
	}
	if mainRepository.CohortKey != linkedRepository.CohortKey || mainRepository.GitDirectory != linkedRepository.GitDirectory {
		t.Fatalf("main and linked repository identities differ: %#v != %#v", mainRepository, linkedRepository)
	}
	if independentRepository.CohortKey == mainRepository.CohortKey {
		t.Fatalf("independent clone reused repository cohort %q", independentRepository.CohortKey)
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
	if prepared[0].RepositoryIdentity != mainRepository || prepared[1].RepositoryIdentity != mainRepository {
		t.Fatalf("prepared linked repository identities differ: %#v", prepared)
	}
	if prepared[2].RepositoryIdentity != independentRepository {
		t.Fatalf("prepared independent repository identity=%#v, want %#v", prepared[2].RepositoryIdentity, independentRepository)
	}

	roots, err := kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(pathResolver),
		kickstart.WithRepositoryIdentityResolver(repositoryResolver),
	).Load(context.Background())
	if err != nil {
		t.Fatalf("load real Git repository forest: %v", err)
	}
	if len(roots) != 2 {
		t.Fatalf("real Git repository roots=%d, want 2", len(roots))
	}
	grouped := repositoryRootByPath(t, roots, mainRepository.GitDirectory.String())
	paths, _, _ := repositoryRootEvidence(grouped)
	if want := sortedCopy([]string{mainClone.String(), linkedClone.String()}); !reflect.DeepEqual(paths, want) {
		t.Fatalf("grouped real worktree paths=%v, want %v", paths, want)
	}
	independentRoot := repositoryRootByPath(t, roots, independentRepository.GitDirectory.String())
	paths, _, _ = repositoryRootEvidence(independentRoot)
	if want := []string{independentClone.String()}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("independent real clone paths=%v, want %v", paths, want)
	}
}

func TestProductionRepositoryResolverGroupsLinkedWorktreeSubmodulesByTopology(t *testing.T) {
	root := t.TempDir()
	submoduleOrigin := filepath.Join(root, "submodule-origin.git")
	submoduleSeed := filepath.Join(root, "submodule-seed")
	superOrigin := filepath.Join(root, "super-origin.git")
	superMain := filepath.Join(root, "super-main")
	superLinkedA := filepath.Join(root, "super-linked-a")
	superLinkedB := filepath.Join(root, "super-linked-b")
	superIndependent := filepath.Join(root, "super-independent")
	standaloneNested := filepath.Join(superLinkedA, "standalone")

	runRepositoryGit(t, root, "init", "--bare", submoduleOrigin)
	runRepositoryGit(t, root, "init", "--initial-branch=main", submoduleSeed)
	configureRepositoryFixture(t, submoduleSeed)
	if err := os.WriteFile(filepath.Join(submoduleSeed, "README.md"), []byte("submodule fixture\n"), 0o600); err != nil {
		t.Fatalf("write submodule fixture: %v", err)
	}
	runRepositoryGit(t, submoduleSeed, "add", "README.md")
	runRepositoryGit(t, submoduleSeed, "commit", "-m", "initial submodule fixture")
	runRepositoryGit(t, submoduleSeed, "remote", "add", "origin", submoduleOrigin)
	runRepositoryGit(t, submoduleSeed, "push", "-u", "origin", "main")
	runRepositoryGit(t, submoduleOrigin, "symbolic-ref", "HEAD", "refs/heads/main")

	runRepositoryGit(t, root, "init", "--bare", superOrigin)
	runRepositoryGit(t, root, "init", "--initial-branch=main", superMain)
	configureRepositoryFixture(t, superMain)
	runRepositoryGit(t, superMain, "-c", "protocol.file.allow=always", "submodule", "add", submoduleOrigin, "pasture")
	runRepositoryGit(t, superMain, "-c", "protocol.file.allow=always", "submodule", "add", submoduleOrigin, "pasture-copy")
	runRepositoryGit(t, superMain, "commit", "-am", "declare submodule fixtures")
	runRepositoryGit(t, superMain, "remote", "add", "origin", superOrigin)
	runRepositoryGit(t, superMain, "push", "-u", "origin", "main")
	runRepositoryGit(t, superOrigin, "symbolic-ref", "HEAD", "refs/heads/main")

	runRepositoryGit(t, superMain, "worktree", "add", "-b", "linked-a", superLinkedA)
	runRepositoryGit(t, superMain, "worktree", "add", "-b", "linked-b", superLinkedB)
	for _, worktree := range []string{superLinkedA, superLinkedB} {
		runRepositoryGit(t, worktree, "-c", "protocol.file.allow=always", "submodule", "update", "--init", "pasture", "pasture-copy")
	}
	runRepositoryGit(t, root, "-c", "protocol.file.allow=always", "clone", "--recurse-submodules", superOrigin, superIndependent)
	runRepositoryGit(t, superLinkedA, "-c", "protocol.file.allow=always", "clone", submoduleOrigin, standaloneNested)

	resolver := ingest.NewPhysicalPathResolver()
	repositoryResolver := ingest.NewGitRepositoryIdentityResolver()
	linkedAPath := mustResolveClonePath(t, resolver, filepath.Join(superLinkedA, "pasture"))
	linkedBPath := mustResolveClonePath(t, resolver, filepath.Join(superLinkedB, "pasture"))
	independentPath := mustResolveClonePath(t, resolver, filepath.Join(superIndependent, "pasture"))
	distinctPath := mustResolveClonePath(t, resolver, filepath.Join(superLinkedA, "pasture-copy"))
	standalonePath := mustResolveClonePath(t, resolver, standaloneNested)
	linkedAIdentity := mustResolveRepositoryIdentity(t, repositoryResolver, linkedAPath)
	linkedBIdentity := mustResolveRepositoryIdentity(t, repositoryResolver, linkedBPath)
	independentIdentity := mustResolveRepositoryIdentity(t, repositoryResolver, independentPath)
	distinctIdentity := mustResolveRepositoryIdentity(t, repositoryResolver, distinctPath)
	standaloneIdentity := mustResolveRepositoryIdentity(t, repositoryResolver, standalonePath)

	if linkedAIdentity.CohortKey != linkedBIdentity.CohortKey {
		t.Fatalf("same-path linked-worktree submodules differ: %q != %q", linkedAIdentity.CohortKey, linkedBIdentity.CohortKey)
	}
	if linkedAIdentity.GitDirectory == linkedBIdentity.GitDirectory {
		t.Fatalf("linked-worktree submodule Git directories unexpectedly match: %q", linkedAIdentity.GitDirectory)
	}
	if independentIdentity.CohortKey == linkedAIdentity.CohortKey {
		t.Fatalf("independent superproject clone reused linked cohort %q", independentIdentity.CohortKey)
	}
	if distinctIdentity.CohortKey == linkedAIdentity.CohortKey {
		t.Fatalf("distinct declared submodule path reused cohort %q", distinctIdentity.CohortKey)
	}
	if standaloneIdentity.CohortKey == linkedAIdentity.CohortKey || !strings.HasPrefix(standaloneIdentity.CohortKey.String(), "repo:") {
		t.Fatalf("undeclared nested clone was treated as the declared submodule cohort: standalone=%q declared=%q", standaloneIdentity.CohortKey, linkedAIdentity.CohortKey)
	}
	if !strings.Contains(linkedAIdentity.CohortKey.String(), "pasture") || !strings.Contains(distinctIdentity.CohortKey.String(), "pasture-copy") {
		t.Fatalf("submodule cohort keys do not retain length-prefixed path components: pasture=%q copy=%q", linkedAIdentity.CohortKey, distinctIdentity.CohortKey)
	}

	listings := []ftue.SessionListing{
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "pasture", GitRemote: submoduleOrigin, WorkingDir: linkedAPath.String(), Branch: "main", SessionID: "51515151-5151-4151-8151-515151515151"},
		{Harness: string(defaults.HarnessCodex), ProjectName: "pasture", GitRemote: submoduleOrigin, WorkingDir: linkedBPath.String(), Branch: "main", SessionID: "61616161-6161-4161-8161-616161616161"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "pasture", GitRemote: submoduleOrigin, WorkingDir: independentPath.String(), Branch: "main", SessionID: "71717171-7171-4171-8171-717171717171"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "pasture", GitRemote: submoduleOrigin, WorkingDir: distinctPath.String(), Branch: "main", SessionID: "81818181-8181-4181-8181-818181818181"},
		{Harness: string(defaults.HarnessClaudeCode), ProjectName: "pasture", GitRemote: submoduleOrigin, WorkingDir: standalonePath.String(), Branch: "main", SessionID: "91919191-9191-4191-8191-919191919191"},
	}
	source := kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(resolver),
		kickstart.WithRepositoryIdentityResolver(repositoryResolver),
	)
	roots := loadRepositoryGroupingRoots(t, source)
	if len(roots) != 4 {
		t.Fatalf("real submodule topology roots=%d, want 4", len(roots))
	}
	grouped := repositoryRootByCohortKey(t, roots, linkedAIdentity.CohortKey.String())
	if len(grouped.Children) != 1 || grouped.Children[0].Meta[settings.MetaBranch] != "main" || len(grouped.Children[0].Children) != 2 {
		t.Fatalf("same-path linked submodules did not share one main branch: %#v", grouped.Children)
	}
	context, ok := source.ListingPreviewContext(grouped.ID)
	if !ok {
		t.Fatal("grouped submodule project has no preview context")
	}
	wantGitDirectories := sortedCopy([]string{linkedAIdentity.GitDirectory.String(), linkedBIdentity.GitDirectory.String()})
	if !reflect.DeepEqual(context.GitDirectories, wantGitDirectories) {
		t.Fatalf("grouped submodule preview Git directories=%v, want %v", context.GitDirectories, wantGitDirectories)
	}

	setNodeState(grouped, kit.Checked)
	rollup(grouped)
	selection := settings.FromTreeNodes(roots).ToSelectionConfig(false)
	for harness, clonePath := range map[string]string{string(defaults.HarnessClaudeCode): linkedAPath.String(), string(defaults.HarnessCodex): linkedBPath.String()} {
		configured, ok := selection.Harnesses[harness]
		if !ok || len(configured.Projects) != 1 || !reflect.DeepEqual(configured.Projects[0].ClonePaths, []string{clonePath}) {
			t.Fatalf("grouped submodule selection for %s=%#v, want exact path %q", harness, configured, clonePath)
		}
	}
	encoded, err := yaml.Marshal(selection)
	if err != nil {
		t.Fatalf("marshal grouped submodule selection: %v", err)
	}
	if strings.Contains(string(encoded), linkedAIdentity.CohortKey.String()) || strings.Contains(string(encoded), linkedAIdentity.GitDirectory.String()) || strings.Contains(string(encoded), linkedBIdentity.GitDirectory.String()) {
		t.Fatalf("grouped submodule selection persisted transient repository identity:\n%s", encoded)
	}
}

func configureRepositoryFixture(t *testing.T, directory string) {
	t.Helper()
	runRepositoryGit(t, directory, "config", "user.email", "fixture@example.invalid")
	runRepositoryGit(t, directory, "config", "user.name", "Fixture User")
	runRepositoryGit(t, directory, "config", "commit.gpgsign", "false")
}

func mustResolveClonePath(t *testing.T, resolver ingest.PathIdentityResolver, path string) ingest.ClonePath {
	t.Helper()
	resolved, err := resolver.Resolve(path)
	if err != nil {
		t.Fatalf("resolve clone path %q: %v", path, err)
	}
	return resolved
}

func mustResolveRepositoryIdentity(t *testing.T, resolver ingest.RepositoryIdentityResolver, path ingest.ClonePath) ingest.RepositoryIdentity {
	t.Helper()
	identity, err := resolver.ResolveRepositoryIdentity(context.Background(), path)
	if err != nil {
		t.Fatalf("resolve repository identity for %q: %v", path, err)
	}
	return identity
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

func fixtureRepositoryResolver(fixtures []repositoryPathFixture) fixtureRepositoryIdentityResolver {
	paths := make(map[ingest.ClonePath]repositoryPathFixture, len(fixtures))
	for _, fixture := range fixtures {
		paths[ingest.ClonePath(fixture.ClonePath)] = fixture
	}
	return fixtureRepositoryIdentityResolver{paths: paths}
}

func repositoryGroupingSource(
	listings []ftue.SessionListing,
	repositoryResolver ingest.RepositoryIdentityResolver,
) *kickstart.ScannerTreeSource {
	return kickstart.NewScannerTreeSource(
		listings,
		withFixturePathResolver(),
		kickstart.WithRepositoryIdentityResolver(repositoryResolver),
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
	expectedCandidateCount := 0
	for _, root := range expected {
		expectedCandidateCount += len(root.CandidateClonePaths)
	}
	if len(candidates) != expectedCandidateCount {
		t.Fatalf("repository commit-gate candidates=%d, want %d", len(candidates), expectedCandidateCount)
	}
	byRepositoryHarness := make(map[string]selectionprojection.ProjectCandidate, len(candidates))
	for _, candidate := range candidates {
		key := candidate.RepositoryCohortKey.String() + "\x00" + candidate.Harness.String()
		if _, duplicate := byRepositoryHarness[key]; duplicate {
			t.Fatalf("repository/harness %q/%q appears in more than one commit-gate candidate", candidate.RepositoryCohortKey, candidate.Harness)
		}
		byRepositoryHarness[key] = candidate
	}
	for _, want := range expected {
		wantKey := expectedRepositoryCohortKey(want)
		for harness, wantPaths := range want.CandidateClonePaths {
			candidate, ok := byRepositoryHarness[wantKey+"\x00"+harness]
			if !ok {
				t.Fatalf("missing commit-gate candidate for repository %q harness %q", want.RepositoryPath, harness)
			}
			pathSet := map[string]struct{}{}
			for _, descendant := range candidate.Descendants {
				pathSet[descendant.ClonePath.String()] = struct{}{}
				if descendant.RepositoryCohortKey != candidate.RepositoryCohortKey {
					t.Fatalf("repository %q descendant %q carries repository cohort %q", want.RepositoryPath, descendant.SessionID, descendant.RepositoryCohortKey)
				}
			}
			paths := make([]string, 0, len(pathSet))
			for path := range pathSet {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			wantPaths = sortedCopy(wantPaths)
			if !reflect.DeepEqual(paths, wantPaths) {
				t.Fatalf("repository %q harness %q commit-gate descendant paths=%v, want %v", want.RepositoryPath, harness, paths, wantPaths)
			}
			if len(wantPaths) == 1 && candidate.ClonePath.String() != wantPaths[0] {
				t.Fatalf("single-worktree repository %q harness %q candidate path=%q, want %q", want.RepositoryPath, harness, candidate.ClonePath, wantPaths[0])
			}
			if len(wantPaths) > 1 && candidate.ClonePath != "" {
				t.Fatalf("multi-worktree repository %q harness %q retained misleading candidate path %q", want.RepositoryPath, harness, candidate.ClonePath)
			}
		}
	}
}

func assertRepositoryGroupingRoots(t *testing.T, roots []*kit.TreeNode, expected []repositoryRootExpectation) {
	t.Helper()
	if len(roots) != len(expected) {
		t.Fatalf("repository roots=%d, want %d", len(roots), len(expected))
	}
	byCohort := make(map[string]*kit.TreeNode, len(roots))
	rowIDs := make(map[string]string)
	for _, root := range roots {
		cohortKey := root.Meta[settings.MetaProjectIdentity]
		if _, duplicate := byCohort[cohortKey]; duplicate {
			t.Fatalf("repository cohort %q appears in more than one root", cohortKey)
		}
		byCohort[cohortKey] = root
		assertUniqueRepositoryRowID(t, rowIDs, root.ID, "project "+cohortKey)
		for _, branch := range root.Children {
			assertUniqueRepositoryRowID(t, rowIDs, branch.ID, "branch "+cohortKey+"/"+branch.Meta[settings.MetaBranch])
		}
	}
	for _, want := range expected {
		wantIdentity := expectedRepositoryCohortKey(want)
		root := byCohort[wantIdentity]
		if root == nil {
			t.Fatalf("missing repository cohort %q; got %v", wantIdentity, repositoryRootCohortKeys(roots))
		}
		if root.ID != wantIdentity || root.Meta[settings.MetaProjectIdentity] != wantIdentity {
			t.Fatalf("root identity id=%q meta=%q, want %q", root.ID, root.Meta[settings.MetaProjectIdentity], wantIdentity)
		}
		if root.Meta[settings.MetaProjectHarness] != "" {
			t.Fatalf("repository root %q carries one misleading harness %q", want.RepositoryPath, root.Meta[settings.MetaProjectHarness])
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

func assertRepositoryGroupingPreviewContexts(
	t *testing.T,
	source *kickstart.ScannerTreeSource,
	roots []*kit.TreeNode,
	expected []repositoryRootExpectation,
) {
	t.Helper()
	for _, want := range expected {
		root := repositoryRootByCohortKey(t, roots, expectedRepositoryCohortKey(want))
		context, ok := source.ListingPreviewContext(root.ID)
		if !ok {
			t.Fatalf("repository %q has no project preview context", want.RepositoryPath)
		}
		clonePaths, branches, sessionCount := repositoryRootEvidence(root)
		wantGitDirectories := repositoryRootGitDirectories(root)
		if context.Kind != kickstart.ListingPreviewProject || context.Project != root.Label || !reflect.DeepEqual(context.GitDirectories, wantGitDirectories) {
			t.Fatalf("repository %q project preview identity=%#v", want.RepositoryPath, context)
		}
		if !reflect.DeepEqual(context.Harnesses, sortedMapKeys(want.CandidateClonePaths)) ||
			!reflect.DeepEqual(sortedCopy(context.Remotes), sortedCopy(want.Remotes)) ||
			!reflect.DeepEqual(context.ClonePaths, clonePaths) ||
			!reflect.DeepEqual(context.Branches, sortedMapKeys(branches)) || context.SessionCount != sessionCount {
			t.Fatalf("repository %q project preview evidence=%#v", want.RepositoryPath, context)
		}

		for _, branch := range root.Children {
			branchName := branch.Meta[settings.MetaBranch]
			branchContext, found := source.ListingPreviewContext(branch.ID)
			if !found {
				t.Fatalf("repository %q branch %q has no preview context", want.RepositoryPath, branchName)
			}
			wantHarnesses := repositoryBranchHarnesses(branch)
			wantPaths := sortedCopy(branches[branchName])
			if branchContext.Kind != kickstart.ListingPreviewBranch || branchContext.Project != root.Label ||
				branchContext.Branch != branchName || !reflect.DeepEqual(branchContext.GitDirectories, repositoryRootGitDirectoriesForBranches([]*kit.TreeNode{branch})) ||
				!reflect.DeepEqual(branchContext.Harnesses, wantHarnesses) ||
				!reflect.DeepEqual(sortedCopy(branchContext.Remotes), sortedCopy(want.Remotes)) ||
				!reflect.DeepEqual(branchContext.ClonePaths, wantPaths) || branchContext.SessionCount != len(branch.Children) {
				t.Fatalf("repository %q branch %q preview evidence=%#v", want.RepositoryPath, branchName, branchContext)
			}
		}
	}
}

func repositoryRootGitDirectories(root *kit.TreeNode) []string {
	return repositoryRootGitDirectoriesForBranches(root.Children)
}

func repositoryRootGitDirectoriesForBranches(branches []*kit.TreeNode) []string {
	set := make(map[string]struct{})
	for _, branch := range branches {
		for _, session := range branch.Children {
			if gitDirectory := session.Meta[settings.MetaGitDirectory]; gitDirectory != "" {
				set[gitDirectory] = struct{}{}
			}
		}
	}
	return sortedMapKeys(set)
}

func assertUniqueRepositoryRowID(t *testing.T, seen map[string]string, id, description string) {
	t.Helper()
	if prior, duplicate := seen[id]; duplicate {
		t.Fatalf("tree row ID %q identifies both %s and %s", id, prior, description)
	}
	seen[id] = description
}

func repositoryBranchHarnesses(branch *kit.TreeNode) []string {
	set := make(map[string]struct{})
	for _, session := range branch.Children {
		if harness := session.Meta[settings.MetaHarness]; harness != "" {
			set[harness] = struct{}{}
		}
	}
	return sortedMapKeys(set)
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func repositoryListingHarnesses(listings []ftue.SessionListing) []string {
	set := make(map[string]struct{})
	for _, listing := range listings {
		if listing.Harness != "" {
			set[listing.Harness] = struct{}{}
		}
	}
	return sortedMapKeys(set)
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

func repositoryRootCohortKeys(roots []*kit.TreeNode) []string {
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Meta[settings.MetaProjectIdentity])
	}
	sort.Strings(paths)
	return paths
}

func repositoryRootByPath(t *testing.T, roots []*kit.TreeNode, repositoryPath string) *kit.TreeNode {
	t.Helper()
	for _, root := range roots {
		for _, gitDirectory := range repositoryRootGitDirectories(root) {
			if gitDirectory == repositoryPath {
				return root
			}
		}
	}
	t.Fatalf("repository root %q is unavailable", repositoryPath)
	return nil
}

func repositoryRootByCohortKey(t *testing.T, roots []*kit.TreeNode, cohortKey string) *kit.TreeNode {
	t.Helper()
	for _, root := range roots {
		if root.Meta[settings.MetaProjectIdentity] == cohortKey {
			return root
		}
	}
	t.Fatalf("repository cohort %q is unavailable", cohortKey)
	return nil
}

func repositoryBranchByName(t *testing.T, root *kit.TreeNode, branchName string) *kit.TreeNode {
	t.Helper()
	for _, branch := range root.Children {
		if branch.Meta[settings.MetaBranch] == branchName {
			return branch
		}
	}
	t.Fatalf("branch %q is unavailable under repository cohort %q", branchName, root.Meta[settings.MetaProjectIdentity])
	return nil
}

func fixtureRepositoryCohortKey(repositoryPath string) string {
	return "fixture:" + repositoryPath
}

func expectedRepositoryCohortKey(expected repositoryRootExpectation) string {
	if expected.CohortKey != "" {
		return expected.CohortKey
	}
	if strings.HasPrefix(expected.RepositoryPath, "/fixtures/team-") {
		return expected.RepositoryPath
	}
	return fixtureRepositoryCohortKey(expected.RepositoryPath)
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
