package githooks_test

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
)

//go:embed testdata/topology.yaml
var topologyFixtureData []byte

const topologyFixturePath = "internal/githooks/testdata/topology.yaml"

// repositoryTopology is the shape of the repository being acted on. Each one
// puts the hook file git runs somewhere different relative to the worktree and
// to the repository's own git directory, which is the whole question.
type repositoryTopology string

const (
	topologyPlain          repositoryTopology = "plain"
	topologySubmodule      repositoryTopology = "submodule"
	topologyWorktreeHooks  repositoryTopology = "worktree-hooks"
	topologyLinkedWorktree repositoryTopology = "linked-worktree"
	topologyOutsideHooks   repositoryTopology = "outside-hooks"
)

var allRepositoryTopologies = [...]repositoryTopology{
	topologyPlain, topologySubmodule, topologyWorktreeHooks, topologyLinkedWorktree, topologyOutsideHooks,
}

type topologyDocument struct {
	ExpectedCaseCount int            `yaml:"expectedCaseCount"`
	Cases             []topologyCase `yaml:"cases"`
}

type topologyCase struct {
	Name                     string             `yaml:"name"`
	Topology                 repositoryTopology `yaml:"topology"`
	ExpectedOutcome          githooks.Outcome   `yaml:"expectedOutcome"`
	ExpectCommittableWarning bool               `yaml:"expectCommittableWarning"`
}

// loadTopologyFixture decodes and fully validates the corpus.
func loadTopologyFixture(data []byte) (topologyDocument, error) {
	document, err := decodeFixtureDocument[topologyDocument](data, topologyFixturePath)
	if err != nil {
		return document, err
	}
	if err := fixtureCountGuard(topologyFixturePath, document.ExpectedCaseCount, len(document.Cases)); err != nil {
		return document, err
	}
	names := make([]string, 0, len(document.Cases))
	for _, testCase := range document.Cases {
		names = append(names, testCase.Name)
	}
	if err := fixtureUniqueNames(topologyFixturePath, names); err != nil {
		return document, err
	}
	for index, testCase := range document.Cases {
		if !containsValue(allRepositoryTopologies[:], testCase.Topology) {
			return document, fixtureCaseError(topologyFixturePath, index,
				fmt.Sprintf("unsupported topology %q", testCase.Topology),
				"fix=use one of plain, submodule, worktree-hooks, linked-worktree, outside-hooks")
		}
		if testCase.ExpectedOutcome != githooks.OutcomeCreated && testCase.ExpectedOutcome != githooks.OutcomeRefused {
			return document, fixtureCaseError(topologyFixturePath, index,
				fmt.Sprintf("unsupported expectedOutcome %q", testCase.ExpectedOutcome),
				"fix=a topology case either writes the hook (created) or declines to (refused)")
		}
		if testCase.ExpectCommittableWarning && testCase.ExpectedOutcome != githooks.OutcomeCreated {
			return document, fixtureCaseError(topologyFixturePath, index,
				"a refused topology cannot disclose anything about a hook that was not written",
				"fix=set expectCommittableWarning to false, or expect it to be created")
		}
	}
	return document, nil
}

// --- loader guards ----------------------------------------------------------

func TestLoadTopologyFixture_RejectsUnmodelledTopology(t *testing.T) {
	t.Parallel()
	_, err := loadTopologyFixture([]byte(`expectedCaseCount: 1
cases:
  - name: bare repository
    topology: bare
    expectedOutcome: created
    expectCommittableWarning: false
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported topology") {
		t.Fatalf("error = %v, want rejection of a topology this corpus does not build", err)
	}
}

func TestLoadTopologyFixture_RejectsADisclosureAboutAFileThatWasNotWritten(t *testing.T) {
	t.Parallel()
	_, err := loadTopologyFixture([]byte(`expectedCaseCount: 1
cases:
  - name: refused but discloses
    topology: linked-worktree
    expectedOutcome: refused
    expectCommittableWarning: true
`))
	if err == nil || !strings.Contains(err.Error(), "cannot disclose anything") {
		t.Fatalf("error = %v, want rejection of a disclosure about a hook that was never written", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestLifecycle_TopologyDecidesOwnership installs into real repositories of each
// shape and checks who ends up owning the file git runs.
//
// The submodule case is the one that was wrong: a submodule's hooks live in its
// own private git directory under the parent, so nothing else on the machine
// runs them, yet every submodule was refused as a shared hooks directory — with
// a remedy that named the repository the user was already in. The two genuinely
// shared shapes must stay refused, which is why they are in the same corpus.
func TestLifecycle_TopologyDecidesOwnership(t *testing.T) {
	t.Parallel()
	document, err := loadTopologyFixture(topologyFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runTopologyCase(t, testCase)
		})
	}
}

func runTopologyCase(t *testing.T, testCase topologyCase) {
	t.Helper()
	acting := topologyRepository(t, testCase.Topology)

	report, err := githooks.New(githooks.NewExecGit()).Install(t.Context(), githooks.Request{
		Dir: acting, Events: []githooks.Event{githooks.EventPostCommit},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	result := report.Results[0]
	if result.Outcome != testCase.ExpectedOutcome {
		t.Fatalf("install outcome = %q, want %q\nreason: %s", result.Outcome, testCase.ExpectedOutcome, result.Reason)
	}

	if testCase.ExpectedOutcome == githooks.OutcomeRefused {
		if result.Refusal != githooks.RefusalSharedPath {
			t.Errorf("refusal = %q, want %q", result.Refusal, githooks.RefusalSharedPath)
		}
		return
	}

	content, readErr := os.ReadFile(result.Path)
	if readErr != nil {
		t.Fatalf("read the installed hook: %v", readErr)
	}
	if !githooks.IsManaged(content) {
		t.Error("the installed file is not recognizable Peasant-owned content")
	}
	if root := githooks.EmbeddedRepository(content); root != report.Repository.Root {
		t.Errorf("the installed hook names repository %q, want the resolved root %q", root, report.Repository.Root)
	}
	// git only runs an executable hook, and a whole topology silently doing
	// nothing is exactly the failure the mode warning exists to surface.
	info, statErr := os.Stat(result.Path)
	if statErr != nil {
		t.Fatalf("stat the installed hook: %v", statErr)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("the installed hook has mode %04o, which git refuses to run", info.Mode().Perm())
	}
	if got := hasWarning(result.Warnings, githooks.WarningCommittableHook); got != testCase.ExpectCommittableWarning {
		t.Errorf("committable-hook disclosure = %v, want %v; warnings: %v",
			got, testCase.ExpectCommittableWarning, result.Warnings)
	}
	// A freshly written hook is executable by construction, so it must never be
	// reported as one git will skip.
	if hasWarning(result.Warnings, githooks.WarningHookNotExecutable) {
		t.Errorf("a hook Peasant just wrote must not be reported as non-executable; warnings: %v", result.Warnings)
	}
}

func hasWarning(warnings []githooks.Warning, kind githooks.WarningKind) bool {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return true
		}
	}
	return false
}

// topologyRepository builds the layout and returns the directory the lifecycle
// is asked about.
func topologyRepository(t *testing.T, topology repositoryTopology) string {
	t.Helper()
	repo := disposableRepo(t)
	switch topology {
	case topologySubmodule:
		// A real submodule, wired the way git does it: the child's git directory
		// moves under the parent's, and its hooks go with it.
		child := filepath.Join(filepath.Dir(repo), "lib")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatalf("create the submodule source: %v", err)
		}
		mustGit(t, child, "init", "--quiet", "--initial-branch=main")
		mustGit(t, child, "config", "user.email", "hooks-test@example.invalid")
		mustGit(t, child, "config", "user.name", "Hooks Test")
		mustGit(t, child, "commit", "--quiet", "--allow-empty", "-m", "base")
		mustGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", "base")
		mustGit(t, repo, "-c", "protocol.file.allow=always", "submodule", "add", "--quiet", child, "sub")
		return filepath.Join(repo, "sub")
	case topologyWorktreeHooks:
		mustGit(t, repo, "config", "core.hooksPath", ".githooks")
		return repo
	case topologyLinkedWorktree:
		mustGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", "base")
		linked := filepath.Join(filepath.Dir(repo), "linked")
		mustGit(t, repo, "worktree", "add", "--quiet", "-b", "topology-test", linked)
		return linked
	case topologyOutsideHooks:
		mustGit(t, repo, "config", "core.hooksPath", filepath.Join(filepath.Dir(repo), "shared-hooks"))
		return repo
	default:
		return repo
	}
}
