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

//go:embed testdata/containment.yaml
var containmentFixtureData []byte

const containmentFixturePath = "internal/githooks/testdata/containment.yaml"

// repositoryAddressing is how the test names the repository it acts on.
type repositoryAddressing string

const (
	// addressingDirect names the repository by its real path.
	addressingDirect repositoryAddressing = "direct"
	// addressingSymlinked names it through a symlinked parent directory.
	addressingSymlinked repositoryAddressing = "symlinked"
)

var allRepositoryAddressings = [...]repositoryAddressing{addressingDirect, addressingSymlinked}

// hooksDirectoryShape is what occupies the repository's hooks directory.
type hooksDirectoryShape string

const (
	hooksDirectoryDefault          hooksDirectoryShape = "default"
	hooksDirectorySymlinkedInside  hooksDirectoryShape = "symlinked-inside"
	hooksDirectorySymlinkedOutside hooksDirectoryShape = "symlinked-outside"
)

var allHooksDirectoryShapes = [...]hooksDirectoryShape{
	hooksDirectoryDefault, hooksDirectorySymlinkedInside, hooksDirectorySymlinkedOutside,
}

type containmentDocument struct {
	ExpectedCaseCount int               `yaml:"expectedCaseCount"`
	Cases             []containmentCase `yaml:"cases"`
}

type containmentCase struct {
	Name            string               `yaml:"name"`
	RepositoryPath  repositoryAddressing `yaml:"repositoryPath"`
	HooksDirectory  hooksDirectoryShape  `yaml:"hooksDirectory"`
	ExpectedOutcome githooks.Outcome     `yaml:"expectedOutcome"`
}

// loadContainmentFixture decodes and fully validates the corpus.
func loadContainmentFixture(data []byte) (containmentDocument, error) {
	document, err := decodeFixtureDocument[containmentDocument](data, containmentFixturePath)
	if err != nil {
		return document, err
	}
	if err := fixtureCountGuard(containmentFixturePath, document.ExpectedCaseCount, len(document.Cases)); err != nil {
		return document, err
	}
	names := make([]string, 0, len(document.Cases))
	for _, testCase := range document.Cases {
		names = append(names, testCase.Name)
	}
	if err := fixtureUniqueNames(containmentFixturePath, names); err != nil {
		return document, err
	}
	for index, testCase := range document.Cases {
		if !containsValue(allRepositoryAddressings[:], testCase.RepositoryPath) {
			return document, fixtureCaseError(containmentFixturePath, index,
				fmt.Sprintf("unsupported repositoryPath %q", testCase.RepositoryPath), "fix=use direct or symlinked")
		}
		if !containsValue(allHooksDirectoryShapes[:], testCase.HooksDirectory) {
			return document, fixtureCaseError(containmentFixturePath, index,
				fmt.Sprintf("unsupported hooksDirectory %q", testCase.HooksDirectory),
				"fix=use default, symlinked-inside, or symlinked-outside")
		}
		if testCase.ExpectedOutcome != githooks.OutcomeCreated && testCase.ExpectedOutcome != githooks.OutcomeRefused {
			return document, fixtureCaseError(containmentFixturePath, index,
				fmt.Sprintf("unsupported expectedOutcome %q", testCase.ExpectedOutcome),
				"fix=a containment case either writes the hook (created) or declines to (refused)")
		}
	}
	return document, nil
}

// --- loader guards ----------------------------------------------------------

func TestLoadContainmentFixture_RejectsUnsupportedShape(t *testing.T) {
	t.Parallel()
	_, err := loadContainmentFixture([]byte(`expectedCaseCount: 1
cases:
  - name: unknown-hooks-directory
    repositoryPath: direct
    hooksDirectory: bind-mounted
    expectedOutcome: created
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported hooksDirectory") {
		t.Fatalf("error = %v, want rejection of an unmodelled hooks-directory shape", err)
	}
}

func TestLoadContainmentFixture_RejectsUndecidableOutcome(t *testing.T) {
	t.Parallel()
	_, err := loadContainmentFixture([]byte(`expectedCaseCount: 1
cases:
  - name: neither-written-nor-declined
    repositoryPath: direct
    hooksDirectory: default
    expectedOutcome: not-present
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported expectedOutcome") {
		t.Fatalf("error = %v, want rejection of an outcome install cannot produce here", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestLifecycle_ContainmentResolvesSymlinks installs through real repositories
// whose paths and hooks directories involve symlinks.
//
// These repositories are deliberately NOT resolved with filepath.EvalSymlinks
// first: doing that is what hid this whole class from the rest of the suite,
// because it normalised away the exact difference the containment check gets
// wrong.
func TestLifecycle_ContainmentResolvesSymlinks(t *testing.T) {
	t.Parallel()
	document, err := loadContainmentFixture(containmentFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runContainmentCase(t, testCase)
		})
	}
}

func runContainmentCase(t *testing.T, testCase containmentCase) {
	t.Helper()
	addressed, physicalRoot, outside := containmentRepository(t, testCase)

	report, err := githooks.New(githooks.NewExecGit()).Install(t.Context(), githooks.Request{
		Dir: addressed, Events: []githooks.Event{githooks.EventPostCommit},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	result := report.Results[0]
	if result.Outcome != testCase.ExpectedOutcome {
		t.Fatalf("install outcome = %q, want %q (reason: %s)", result.Outcome, testCase.ExpectedOutcome, result.Reason)
	}

	if testCase.ExpectedOutcome == githooks.OutcomeRefused {
		if result.Refusal != githooks.RefusalSharedPath {
			t.Errorf("refusal = %q, want %q: a hooks directory that resolves outside the repository is not this repository's to write",
				result.Refusal, githooks.RefusalSharedPath)
		}
		if outside != "" {
			if entries, readErr := os.ReadDir(outside); readErr == nil && len(entries) != 0 {
				t.Errorf("%d entr(y/ies) were written outside the repository at %s", len(entries), outside)
			}
		}
		return
	}

	// The write must land physically inside the repository, not merely appear to.
	written, evalErr := filepath.EvalSymlinks(result.Path)
	if evalErr != nil {
		t.Fatalf("resolve the written hook %s: %v", result.Path, evalErr)
	}
	if !strings.HasPrefix(written, physicalRoot+string(filepath.Separator)) {
		t.Errorf("the hook was written to %s, which is outside the repository %s", written, physicalRoot)
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
}

// containmentRepository builds the layout and returns the path the lifecycle is
// asked about, the repository's physical root, and the outside directory a
// symlinked hooks directory points at (empty when there is none).
func containmentRepository(t *testing.T, testCase containmentCase) (addressed, physicalRoot, outside string) {
	t.Helper()
	// Deliberately unresolved: t.TempDir() is itself reached through a symlink on
	// some platforms, and resolving it here would erase the difference under test.
	base := t.TempDir()
	physicalParent := filepath.Join(base, "physical")
	physicalRoot = filepath.Join(physicalParent, "repo")
	if err := os.MkdirAll(physicalRoot, 0o755); err != nil {
		t.Fatalf("create repository directory: %v", err)
	}
	mustGit(t, physicalRoot, "init", "--quiet", "--initial-branch=main")

	switch testCase.HooksDirectory {
	case hooksDirectorySymlinkedInside:
		target := filepath.Join(physicalRoot, ".repo-hooks")
		outside = ""
		linkHooksDirectory(t, physicalRoot, target)
	case hooksDirectorySymlinkedOutside:
		outside = filepath.Join(base, "outside-hooks")
		linkHooksDirectory(t, physicalRoot, outside)
	}

	addressed = physicalRoot
	if testCase.RepositoryPath == addressingSymlinked {
		link := filepath.Join(base, "link")
		if err := os.Symlink(physicalParent, link); err != nil {
			t.Fatalf("symlink the repository's parent directory: %v", err)
		}
		addressed = filepath.Join(link, "repo")
	}

	// Resolve only for the assertion, never for the path handed to the lifecycle.
	resolvedRoot, err := filepath.EvalSymlinks(physicalRoot)
	if err != nil {
		t.Fatalf("resolve the physical repository root: %v", err)
	}
	return addressed, resolvedRoot, outside
}

// linkHooksDirectory replaces git's own hooks directory with a symlink to target.
func linkHooksDirectory(t *testing.T, repo, target string) {
	t.Helper()
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("create hooks target %s: %v", target, err)
	}
	hooks := filepath.Join(repo, ".git", "hooks")
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatalf("remove the default hooks directory: %v", err)
	}
	if err := os.Symlink(target, hooks); err != nil {
		t.Fatalf("symlink the hooks directory to %s: %v", target, err)
	}
}
