package discovery_test

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/repository_grouping.yaml
var repositoryGroupingData []byte

type groupingDocument struct {
	ExpectedCaseCount            int               `yaml:"expectedCaseCount"`
	ExpectedTopologyFailureCount int               `yaml:"expectedTopologyFailureCount"`
	ExpectedNames                []string          `yaml:"expectedNames"`
	Cases                        []groupingCase    `yaml:"cases"`
	TopologyFailures             []topologyFailure `yaml:"topologyFailures"`
}

type topologyFailure struct {
	Name         string `yaml:"name"`
	FailurePoint string `yaml:"failurePoint"`
	ClonePath    string `yaml:"clonePath"`
	SiblingPath  string `yaml:"siblingPath"`
	GitRemote    string `yaml:"gitRemote"`
	ProjectName  string `yaml:"projectName"`
}

type groupingCase struct {
	Name                 string                     `yaml:"name"`
	Repositories         []repositoryPath           `yaml:"repositories"`
	Listings             []discovery.SessionListing `yaml:"listings"`
	ExpectedRoots        []rootExpectation          `yaml:"expectedRoots"`
	Saved                any                        `yaml:"saved"`
	Action               string                     `yaml:"action"`
	ActionRepositoryPath string                     `yaml:"actionRepositoryPath"`
	ActionBranch         string                     `yaml:"actionBranch"`
	ExpectedSelection    any                        `yaml:"expectedSelection"`
	ExpectedChecked      []string                   `yaml:"expectedChecked"`
	ExpectedUnchecked    []string                   `yaml:"expectedUnchecked"`
	Facet                any                        `yaml:"facet"`
}

type repositoryPath struct {
	ClonePath      string `yaml:"clonePath"`
	CohortKey      string `yaml:"cohortKey"`
	RepositoryPath string `yaml:"repositoryPath"`
	Fail           bool   `yaml:"fail"`
}

type rootExpectation struct {
	CohortKey           string              `yaml:"cohortKey"`
	RepositoryPath      string              `yaml:"repositoryPath"`
	ClonePaths          []string            `yaml:"clonePaths"`
	Remotes             []string            `yaml:"remotes"`
	Branches            map[string][]string `yaml:"branches"`
	RemoteMultiplicity  string              `yaml:"remoteMultiplicity"`
	CandidateClonePaths map[string][]string `yaml:"candidateClonePaths"`
}

func TestBuildForestRepositoryGrouping(t *testing.T) {
	t.Parallel()
	var document groupingDocument
	if err := testutil.DecodeFixtureYAML(repositoryGroupingData, &document); err != nil {
		t.Fatalf("decode repository grouping fixture: %v", err)
	}
	if document.ExpectedCaseCount != len(document.Cases) || document.ExpectedCaseCount != len(document.ExpectedNames) {
		t.Fatalf("case count=%d names=%d expected=%d", len(document.Cases), len(document.ExpectedNames), document.ExpectedCaseCount)
	}
	if document.ExpectedTopologyFailureCount != len(document.TopologyFailures) {
		t.Fatalf("topology failures=%d expected=%d", len(document.TopologyFailures), document.ExpectedTopologyFailureCount)
	}
	names := map[string]bool{}
	for index, row := range document.Cases {
		if row.Name != document.ExpectedNames[index] {
			t.Fatalf("case[%d]=%q manifest=%q", index, row.Name, document.ExpectedNames[index])
		}
		if names[row.Name] {
			t.Fatalf("duplicate grouping case %q", row.Name)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("repository grouping", "case", document.ExpectedNames, names); err != nil {
		t.Fatal(err)
	}
	failureNames := make([]string, len(document.TopologyFailures))
	failurePresent := map[string]bool{}
	for index, failure := range document.TopologyFailures {
		failureNames[index] = failure.Name
		failurePresent[failure.Name] = true
	}
	if err := testutil.RequireFixtureNames("repository grouping", "topology failure", failureNames, failurePresent); err != nil {
		t.Fatal(err)
	}

	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			prepared := discovery.PrepareSessionListingsWith(context.Background(), row.Listings, preResolvedPathResolver{}, fixtureRepositoryResolver{paths: row.Repositories})
			forest := discovery.BuildForest(prepared, nil, nil)
			assertForestRoots(t, forest, row.ExpectedRoots)
			assertPreviewContexts(t, discovery.PreviewContexts(forest), forest, row.ExpectedRoots)
		})
	}
	for _, failure := range document.TopologyFailures {
		failure := failure
		t.Run(failure.Name, func(t *testing.T) {
			t.Parallel()
			listings := []discovery.SessionListing{
				{Harness: "claude-code", ProjectName: failure.ProjectName, GitRemote: failure.GitRemote, WorkingDir: failure.ClonePath, Branch: "main", SessionID: "12121212-1212-4212-8212-121212121212"},
				{Harness: "claude-code", ProjectName: failure.ProjectName, GitRemote: failure.GitRemote, WorkingDir: failure.SiblingPath, Branch: "main", SessionID: "34343434-3434-4434-8434-343434343434"},
			}
			prepared := discovery.PrepareSessionListingsWith(context.Background(), listings, preResolvedPathResolver{}, failingRepositoryResolver{})
			forest := discovery.BuildForest(prepared, nil, nil)
			roots := projectNodes(forest)
			if len(roots) != 2 {
				t.Fatalf("failure point %q roots=%d, want 2 exact-path roots", failure.FailurePoint, len(roots))
			}
			got := map[string]onboarding.Node{}
			for _, root := range roots {
				got[root.ID] = root
			}
			for _, want := range []string{failure.ClonePath, failure.SiblingPath} {
				root, ok := got[want]
				if !ok {
					t.Fatalf("failure point %q lacks exact-path root %q; got %v", failure.FailurePoint, want, rootIDs(roots))
				}
				sessions := sessionNodes(forest, root.ID)
				if len(sessions) != 1 || sessions[0].Labels.Location != want {
					t.Fatalf("failure point %q root %q sessions=%v, want one session at %q", failure.FailurePoint, want, sessions, want)
				}
			}
		})
	}
}

type preResolvedPathResolver struct{}

func (preResolvedPathResolver) Resolve(dir string) (ingest.ClonePath, error) {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return "", fmt.Errorf("fixture path %q is not a clean absolute path", dir)
	}
	return ingest.ClonePath(dir), nil
}

type fixtureRepositoryResolver struct {
	paths []repositoryPath
}

func (r fixtureRepositoryResolver) ResolveRepositoryIdentity(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	for _, path := range r.paths {
		if path.ClonePath == clonePath.String() {
			if path.Fail {
				return ingest.RepositoryIdentity{}, fmt.Errorf("fixture marks clone %q as non-Git", clonePath)
			}
			cohort := path.CohortKey
			if cohort == "" {
				cohort = "fixture:" + path.RepositoryPath
			}
			return ingest.RepositoryIdentity{
				CohortKey:    ingest.RepositoryCohortKey(cohort),
				GitDirectory: ingest.RepositoryPath(path.RepositoryPath),
			}, nil
		}
	}
	return ingest.RepositoryIdentity{}, fmt.Errorf("fixture has no repository identity for clone %q", clonePath)
}

type failingRepositoryResolver struct{}

func (failingRepositoryResolver) ResolveRepositoryIdentity(_ context.Context, clonePath ingest.ClonePath) (ingest.RepositoryIdentity, error) {
	return ingest.RepositoryIdentity{}, fmt.Errorf("fixture topology failure for %q", clonePath)
}

func assertForestRoots(t *testing.T, forest onboarding.Forest, expected []rootExpectation) {
	t.Helper()
	roots := projectNodes(forest)
	if len(roots) != len(expected) {
		t.Fatalf("repository roots=%d, want %d (%v)", len(roots), len(expected), rootIDs(roots))
	}
	byID := map[string]onboarding.Node{}
	for _, root := range roots {
		byID[root.ID] = root
	}
	for _, want := range expected {
		identity := expectedCohortKey(want)
		root, ok := byID[identity]
		if !ok {
			t.Fatalf("missing repository cohort %q; got %v", identity, rootIDs(roots))
		}
		if root.Kind != onboarding.NodeKindProject || root.Label == "" {
			t.Fatalf("root %q kind=%s label=%q", identity, root.Kind, root.Label)
		}
		if got := multiplicityToken(root.Candidate.RemoteMultiplicity); got != want.RemoteMultiplicity {
			t.Fatalf("root %q remote multiplicity=%q, want %q", identity, got, want.RemoteMultiplicity)
		}
		clonePaths, branches := forestEvidence(forest, root.ID)
		if !reflect.DeepEqual(clonePaths, sortedCopy(want.ClonePaths)) {
			t.Fatalf("root %q clone paths=%v, want %v", identity, clonePaths, sortedCopy(want.ClonePaths))
		}
		if !reflect.DeepEqual(branches, sortedPathMap(want.Branches)) {
			t.Fatalf("root %q branches=%v, want %v", identity, branches, sortedPathMap(want.Branches))
		}
		if len(want.ClonePaths) == 1 && root.Candidate.ClonePath.String() != want.ClonePaths[0] {
			t.Fatalf("single-worktree root clone path=%q, want %q", root.Candidate.ClonePath, want.ClonePaths[0])
		}
		if len(want.ClonePaths) > 1 && root.Candidate.ClonePath != "" {
			t.Fatalf("multi-worktree root retained clone path %q", root.Candidate.ClonePath)
		}
	}
}

func assertPreviewContexts(t *testing.T, contexts map[string]discovery.PreviewContext, forest onboarding.Forest, expected []rootExpectation) {
	t.Helper()
	for _, want := range expected {
		identity := expectedCohortKey(want)
		context, ok := contexts[identity]
		if !ok {
			t.Fatalf("repository %q has no project preview context", identity)
		}
		clonePaths, branches := forestEvidence(forest, identity)
		if context.Kind != discovery.PreviewProject || context.SessionCount == 0 {
			t.Fatalf("repository %q preview=%#v", identity, context)
		}
		if !reflect.DeepEqual(context.ClonePaths, clonePaths) || !reflect.DeepEqual(context.Branches, sortedKeys(branches)) {
			t.Fatalf("repository %q preview paths=%v branches=%v, forest paths=%v branches=%v", identity, context.ClonePaths, context.Branches, clonePaths, sortedKeys(branches))
		}
		if !reflect.DeepEqual(sortedCopy(context.Remotes), sortedCopy(want.Remotes)) {
			t.Fatalf("repository %q preview remotes=%v, want %v", identity, context.Remotes, want.Remotes)
		}
	}
}

func forestEvidence(forest onboarding.Forest, projectID string) ([]string, map[string][]string) {
	children := map[string][]onboarding.Node{}
	for _, node := range forest.Nodes {
		if node.ParentID != "" {
			children[node.ParentID] = append(children[node.ParentID], node)
		}
	}
	pathSet := map[string]struct{}{}
	branches := map[string][]string{}
	for _, child := range children[projectID] {
		if child.Kind == onboarding.NodeKindSession {
			if path := child.Candidate.ClonePath.String(); path != "" {
				pathSet[path] = struct{}{}
			}
			continue
		}
		var paths []string
		for _, session := range children[child.ID] {
			if path := session.Candidate.ClonePath.String(); path != "" {
				pathSet[path] = struct{}{}
				paths = append(paths, path)
			}
		}
		sort.Strings(paths)
		branches[child.Label] = paths
	}
	return sortedSet(pathSet), branches
}

func projectNodes(forest onboarding.Forest) []onboarding.Node {
	var roots []onboarding.Node
	for _, node := range forest.Nodes {
		if node.Kind == onboarding.NodeKindProject {
			roots = append(roots, node)
		}
	}
	return roots
}

func rootIDs(roots []onboarding.Node) []string {
	ids := make([]string, len(roots))
	for i, root := range roots {
		ids[i] = root.ID
	}
	return ids
}

func expectedCohortKey(want rootExpectation) string {
	if want.CohortKey != "" {
		return want.CohortKey
	}
	if strings.HasPrefix(want.RepositoryPath, "/fixtures/team-") {
		return want.RepositoryPath
	}
	return "fixture:" + want.RepositoryPath
}

func sessionNodes(forest onboarding.Forest, projectID string) []onboarding.Node {
	children := map[string][]onboarding.Node{}
	for _, node := range forest.Nodes {
		if node.ParentID != "" {
			children[node.ParentID] = append(children[node.ParentID], node)
		}
	}
	var sessions []onboarding.Node
	var walk func(id string)
	walk = func(id string) {
		for _, child := range children[id] {
			if child.Kind == onboarding.NodeKindSession {
				sessions = append(sessions, child)
				continue
			}
			walk(child.ID)
		}
	}
	walk(projectID)
	return sessions
}

func multiplicityToken(value ingest.DiscoveryIdentityMultiplicity) string {
	switch value {
	case ingest.DiscoveryIdentityUnique:
		return "unique"
	case ingest.DiscoveryIdentityAmbiguous:
		return "ambiguous"
	default:
		return "unproven"
	}
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func sortedSet(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func sortedKeys(values map[string][]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedPathMap(source map[string][]string) map[string][]string {
	out := make(map[string][]string, len(source))
	for key, values := range source {
		out[key] = sortedCopy(values)
	}
	return out
}
