package githooks_test

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
)

//go:embed testdata/shared_path.yaml
var sharedPathFixtureData []byte

const sharedPathFixturePath = "internal/githooks/testdata/shared_path.yaml"

// sharedLayout is how a repository ends up running a hook file that is not its
// own. Both cases are real: a linked worktree runs its main worktree's hooks
// directory, and an outside core.hooksPath is run by every repository pointed at
// it.
type sharedLayout string

const (
	sharedLayoutLinkedWorktree   sharedLayout = "linked-worktree"
	sharedLayoutOutsideHooksPath sharedLayout = "outside-hooks-path"
)

var allSharedLayouts = [...]sharedLayout{sharedLayoutLinkedWorktree, sharedLayoutOutsideHooksPath}

// hookOperation is the lifecycle call under test. The wording a user reads has
// to come from the operation they ran.
type hookOperation string

const (
	hookOperationInstall   hookOperation = "install"
	hookOperationStatus    hookOperation = "status"
	hookOperationUninstall hookOperation = "uninstall"
)

var allHookOperations = [...]hookOperation{hookOperationInstall, hookOperationStatus, hookOperationUninstall}

type sharedPathDocument struct {
	ExpectedCaseCount int              `yaml:"expectedCaseCount"`
	Cases             []sharedPathCase `yaml:"cases"`
}

type sharedPathCase struct {
	Name                string           `yaml:"name"`
	Layout              sharedLayout     `yaml:"layout"`
	Seed                seedKind         `yaml:"seed"`
	Operation           hookOperation    `yaml:"operation"`
	ExpectedOutcome     githooks.Outcome `yaml:"expectedOutcome"`
	ExpectManualSnippet bool             `yaml:"expectManualSnippet"`
	// ExpectOwnerCommand states whether the refusal may offer "do it from the
	// repository that owns the hooks directory". That is only an option when
	// such a repository exists: a linked worktree has one, and a core.hooksPath
	// pointing at a plain directory — the ordinary way people share hooks —
	// does not, where the offer resolves to "is not inside a Git worktree".
	ExpectOwnerCommand bool     `yaml:"expectOwnerCommand"`
	MustContain        []string `yaml:"mustContain"`
	MustNotContain     []string `yaml:"mustNotContain"`
}

// loadSharedPathFixture decodes and fully validates the corpus.
func loadSharedPathFixture(data []byte) (sharedPathDocument, error) {
	document, err := decodeFixtureDocument[sharedPathDocument](data, sharedPathFixturePath)
	if err != nil {
		return document, err
	}
	if err := fixtureCountGuard(sharedPathFixturePath, document.ExpectedCaseCount, len(document.Cases)); err != nil {
		return document, err
	}
	names := make([]string, 0, len(document.Cases))
	for _, testCase := range document.Cases {
		names = append(names, testCase.Name)
	}
	if err := fixtureUniqueNames(sharedPathFixturePath, names); err != nil {
		return document, err
	}
	for index, testCase := range document.Cases {
		if !containsValue(allSharedLayouts[:], testCase.Layout) {
			return document, fixtureCaseError(sharedPathFixturePath, index,
				fmt.Sprintf("unsupported layout %q", testCase.Layout),
				"fix=use linked-worktree or outside-hooks-path")
		}
		if !containsValue(allHookOperations[:], testCase.Operation) {
			return document, fixtureCaseError(sharedPathFixturePath, index,
				fmt.Sprintf("unsupported operation %q", testCase.Operation),
				"fix=use install, status, or uninstall")
		}
		if testCase.Seed != seedAbsent && testCase.Seed != seedManaged {
			return document, fixtureCaseError(sharedPathFixturePath, index,
				fmt.Sprintf("unsupported seed %q", testCase.Seed),
				"fix=use absent or managed; a shared slot is seeded through Peasant or not at all")
		}
		if testCase.Seed == seedManaged && testCase.Layout != sharedLayoutLinkedWorktree {
			return document, fixtureCaseError(sharedPathFixturePath, index,
				"only the linked-worktree layout can be seeded with a managed hook",
				"fix=use seed absent; Peasant refuses to write into an outside core.hooksPath from anywhere, so there is no honest way to seed one")
		}
		if !containsValue(githooks.AllOutcomes[:], testCase.ExpectedOutcome) {
			return document, fixtureCaseError(sharedPathFixturePath, index,
				fmt.Sprintf("unsupported expectedOutcome %q", testCase.ExpectedOutcome), "fix=use a known outcome")
		}
		if len(testCase.MustContain) == 0 {
			return document, fixtureCaseError(sharedPathFixturePath, index, "mustContain is empty",
				"fix=state the facts this operation's wording has to carry, or the case asserts nothing about the message")
		}
		for _, forbidden := range testCase.MustNotContain {
			if containsValue(testCase.MustContain, forbidden) {
				return document, fixtureCaseError(sharedPathFixturePath, index,
					fmt.Sprintf("%q is both required and forbidden", forbidden),
					"fix=decide which one it is; a phrase in both lists can never pass")
			}
		}
	}
	// Both sides of the ownership question have to be covered. A corpus that
	// only ever runs the layout WITH an owner cannot catch the option being
	// offered where no repository owns the directory at all.
	var withOwner, withoutOwner bool
	for _, testCase := range document.Cases {
		if testCase.ExpectOwnerCommand {
			withOwner = true
		} else {
			withoutOwner = true
		}
	}
	if !withOwner || !withoutOwner {
		return document, fixtureCaseError(sharedPathFixturePath, len(document.Cases),
			fmt.Sprintf("the owner option needs a case on each side, got expectOwnerCommand=true:%v false:%v", withOwner, withoutOwner),
			"fix=add the missing case; an option that names a repository which does not exist is a wrong entry in a menu of three")
	}
	return document, nil
}

// --- loader guards ----------------------------------------------------------

func TestLoadSharedPathFixture_RejectsUnsatisfiablePhrase(t *testing.T) {
	t.Parallel()
	_, err := loadSharedPathFixture([]byte(`expectedCaseCount: 1
cases:
  - name: contradictory-phrase
    layout: linked-worktree
    seed: absent
    operation: install
    expectedOutcome: refused
    expectManualSnippet: false
    mustContain: ["shared"]
    mustNotContain: ["shared"]
`))
	if err == nil || !strings.Contains(err.Error(), "both required and forbidden") {
		t.Fatalf("error = %v, want rejection of a phrase that can never pass", err)
	}
}

func TestLoadSharedPathFixture_RejectsUnseedableManagedSlot(t *testing.T) {
	t.Parallel()
	_, err := loadSharedPathFixture([]byte(`expectedCaseCount: 1
cases:
  - name: managed-outside-hooks-path
    layout: outside-hooks-path
    seed: managed
    operation: status
    expectedOutcome: refused
    expectManualSnippet: false
    mustContain: ["shared"]
`))
	if err == nil || !strings.Contains(err.Error(), "only the linked-worktree layout") {
		t.Fatalf("error = %v, want rejection of a slot Peasant could never have seeded", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestLifecycle_SharedPathMessaging drives install, status, and uninstall
// against hook paths that are shared beyond the repository being acted on, and
// checks what the user is told.
//
// The structural assertion is the load-bearing one: no by-hand snippet is
// offered for a shared file. Recommending a paste into a file every worktree and
// repository runs would produce exactly the cross-repository leak the refusal
// exists to prevent.
func TestLifecycle_SharedPathMessaging(t *testing.T) {
	t.Parallel()
	document, err := loadSharedPathFixture(sharedPathFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runSharedPathCase(t, testCase)
		})
	}
}

func runSharedPathCase(t *testing.T, testCase sharedPathCase) {
	t.Helper()
	lifecycle := githooks.New(githooks.NewExecGit())
	ctx := t.Context()
	acting, owner := sharedPathRepositories(t, testCase.Layout)

	if testCase.Seed == seedManaged {
		// Seed through the repository that owns the hooks directory: that is the
		// only way a managed hook legitimately ends up at a path the acting
		// worktree shares.
		seeded, seedErr := lifecycle.Install(ctx, githooks.Request{
			Dir: owner, Events: []githooks.Event{githooks.EventPostCommit},
		})
		if seedErr != nil {
			t.Fatalf("seed managed hook from %s: %v", owner, seedErr)
		}
		if seeded.Results[0].Outcome != githooks.OutcomeCreated {
			t.Fatalf("seed managed hook: outcome=%s reason=%s", seeded.Results[0].Outcome, seeded.Results[0].Reason)
		}
	}

	request := githooks.Request{Dir: acting, Events: []githooks.Event{githooks.EventPostCommit}}
	var outcome githooks.Outcome
	var reason, manual string
	switch testCase.Operation {
	case hookOperationStatus:
		report, statusErr := lifecycle.Status(ctx, githooks.Request{Dir: acting})
		if statusErr != nil {
			t.Fatalf("status: %v", statusErr)
		}
		plan := findStatus(t, report, githooks.EventPostCommit)
		// status has no outcome of its own; the refusal it reports is the plan's.
		outcome = githooks.OutcomeRefused
		if plan.Action != githooks.ActionRefuse {
			t.Fatalf("status action = %q, want refuse for a shared path", plan.Action)
		}
		reason, manual = plan.Reason, plan.Manual
	case hookOperationUninstall:
		report, uninstallErr := lifecycle.Uninstall(ctx, request)
		if uninstallErr != nil {
			t.Fatalf("uninstall: %v", uninstallErr)
		}
		outcome, reason, manual = report.Results[0].Outcome, report.Results[0].Reason, report.Results[0].Manual
	default:
		report, installErr := lifecycle.Install(ctx, request)
		if installErr != nil {
			t.Fatalf("install: %v", installErr)
		}
		outcome, reason, manual = report.Results[0].Outcome, report.Results[0].Reason, report.Results[0].Manual
	}

	if outcome != testCase.ExpectedOutcome {
		t.Errorf("%s outcome = %q, want %q (reason: %s)", testCase.Operation, outcome, testCase.ExpectedOutcome, reason)
	}
	if hasManual := manual != ""; hasManual != testCase.ExpectManualSnippet {
		t.Errorf("%s offered a by-hand snippet = %v, want %v; a shared file is run by every worktree and repository "+
			"that resolves to it, so a section pinned to one repository must not be recommended for it\n%s",
			testCase.Operation, hasManual, testCase.ExpectManualSnippet, manual)
	}
	for _, want := range testCase.MustContain {
		if !strings.Contains(reason, want) {
			t.Errorf("%s explanation must state %q; got:\n%s", testCase.Operation, want, reason)
		}
	}
	for _, forbidden := range testCase.MustNotContain {
		if strings.Contains(reason, forbidden) {
			t.Errorf("%s explanation must not say %q; got:\n%s", testCase.Operation, forbidden, reason)
		}
	}
	assertOwnerOption(t, testCase, owner, reason)
}

// assertOwnerOption holds the "run it from the repository that owns the hooks
// directory" option to the existence of such a repository.
//
// With a global core.hooksPath the directory belongs to nobody, and the option
// resolves to "githooks: <dir> is not inside a Git worktree" — a wrong entry
// standing beside two that work. For a linked worktree the owner is real, and
// the command must name it so the user can paste it.
func assertOwnerOption(t *testing.T, testCase sharedPathCase, owner, reason string) {
	t.Helper()
	var ownerCommand string
	switch testCase.Operation {
	case hookOperationStatus:
		ownerCommand = githooks.StatusCommand(owner)
	case hookOperationUninstall:
		ownerCommand = githooks.UninstallCommand(githooks.EventPostCommit, owner)
	default:
		ownerCommand = githooks.InstallCommand(githooks.EventPostCommit, owner)
	}
	if offered := strings.Contains(reason, ownerCommand); offered != testCase.ExpectOwnerCommand {
		t.Errorf("%s offered %q = %v, want %v; the option only exists when a repository really runs that file\n%s",
			testCase.Operation, ownerCommand, offered, testCase.ExpectOwnerCommand, reason)
	}
}

// sharedPathRepositories builds the layout and returns the directory the test
// acts from and the directory that owns the hooks file git would run.
func sharedPathRepositories(t *testing.T, layout sharedLayout) (acting, owner string) {
	t.Helper()
	main := disposableRepo(t)
	if layout == sharedLayoutOutsideHooksPath {
		shared := filepath.Join(filepath.Dir(main), "shared-hooks")
		mustGit(t, main, "config", "core.hooksPath", shared)
		return main, main
	}
	mustGit(t, main, "commit", "--quiet", "--allow-empty", "-m", "base")
	linked := filepath.Join(filepath.Dir(main), "linked")
	mustGit(t, main, "worktree", "add", "--quiet", "-b", "shared-path-test", linked)
	return linked, main
}
