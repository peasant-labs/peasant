package githooks_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
)

// TestLifecycle_AnUnwritableHooksDirectorySaysHowToFixIt covers a failure whose
// printed remedy pointed straight back at the command that had just failed.
//
// The cause — the hooks directory is not writable — was already in the
// explanation, but the Fix line said "run 'peasant village hooks status', then
// retry". status then reports "not installed — install one with <the install
// that just failed>", so the two surfaces hand the user back and forth and
// neither ever mentions the permission. The sibling removal path already names
// it directly.
func TestLifecycle_AnUnwritableHooksDirectorySaysHowToFixIt(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions decide this case")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the write bit, so the failure cannot be produced")
	}
	repo := disposableRepo(t)
	hooksDir := filepath.Dir(hookPath(t, repo, githooks.EventPostCommit.String()))
	if err := os.Chmod(hooksDir, 0o555); err != nil {
		t.Fatalf("make the hooks directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(hooksDir, 0o755) })

	report, err := githooks.New(githooks.NewExecGit()).Install(t.Context(), githooks.Request{
		Dir: repo, Events: []githooks.Event{githooks.EventPostCommit},
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	result := report.Results[0]
	if result.Outcome != githooks.OutcomeFailed {
		t.Fatalf("install outcome = %q, want %q (reason: %s)", result.Outcome, githooks.OutcomeFailed, result.Reason)
	}
	fix := fixLine(t, result.Reason)
	if !strings.Contains(fix, "writable") || !strings.Contains(fix, hooksDir) {
		t.Errorf("the fix must name %s and say it has to be writable; got: %s", hooksDir, fix)
	}
	if strings.Contains(fix, "hooks status") {
		t.Errorf("the fix sends the user to status, which sends them back to this install; got: %s", fix)
	}
}

// TestLifecycle_AMovedAndInertHookDoesNotClaimGitRunsIt covers two disclosures
// printing together and contradicting each other.
//
// A hook that was moved AND lost its executable bit reported "Git runs the
// Peasant-generated hook here" and "The hook still runs, and its upload fails on
// every post-commit", directly above a warning stating that git skips the file
// and nothing is uploaded. Both cannot be true, and the one that is false is the
// one a user acts on.
func TestLifecycle_AMovedAndInertHookDoesNotClaimGitRunsIt(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("git for windows does not consult the executable bit")
	}
	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	if _, err := lifecycle.Install(t.Context(), githooks.Request{
		Dir: repo, Events: []githooks.Event{githooks.EventPostCommit},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	hook := hookPath(t, repo, githooks.EventPostCommit.String())
	rewriteEmbeddedRepository(t, hook, repo, filepath.Join(filepath.Dir(repo), "somewhere-else"))
	if err := os.Chmod(hook, 0o644); err != nil {
		t.Fatalf("drop the executable bit: %v", err)
	}

	status, err := lifecycle.Status(t.Context(), githooks.Request{Dir: repo})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	plan := findStatus(t, status, githooks.EventPostCommit)

	if _, found := findWarning(plan.Warnings, githooks.WarningHookNotExecutable); !found {
		t.Fatalf("the not-executable disclosure is missing; warnings: %v", plan.Warnings)
	}
	if strings.Contains(plan.Reason, "Git runs the Peasant-generated") {
		t.Errorf("the primary statement claims git runs a file git is skipping; got: %s", plan.Reason)
	}
	if !strings.Contains(plan.Reason, "git does not run it") {
		t.Errorf("the primary statement must say git is not running this file; got: %s", plan.Reason)
	}
	moved, found := findWarning(plan.Warnings, githooks.WarningRepositoryMoved)
	if !found {
		t.Fatalf("the moved-repository disclosure is missing; warnings: %v", plan.Warnings)
	}
	if strings.Contains(moved.Detail, "The hook still runs") {
		t.Errorf("the moved disclosure claims the hook runs beside a warning that git skips it; got: %s", moved.Detail)
	}
	if !strings.Contains(moved.Detail, "git is not running this file at all right now") {
		t.Errorf("the moved disclosure must agree with the mode warning; got: %s", moved.Detail)
	}

	// With the mode restored, the moved hook does run and does fail, and the
	// report has to say so again — the suppression is conditional, not a
	// deletion.
	if err := os.Chmod(hook, githooks.ScriptMode); err != nil {
		t.Fatalf("restore the executable bit: %v", err)
	}
	status, err = lifecycle.Status(t.Context(), githooks.Request{Dir: repo})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	plan = findStatus(t, status, githooks.EventPostCommit)
	if !strings.Contains(plan.Reason, "Git runs the Peasant-generated") {
		t.Errorf("an executable managed hook must be reported as running; got: %s", plan.Reason)
	}
	moved, found = findWarning(plan.Warnings, githooks.WarningRepositoryMoved)
	if !found {
		t.Fatalf("the moved-repository disclosure is missing after the mode was restored; warnings: %v", plan.Warnings)
	}
	if !strings.Contains(moved.Detail, "The hook still runs") {
		t.Errorf("an executable moved hook does run and does fail; got: %s", moved.Detail)
	}
}

// fixLine extracts the Fix line of a six-dimension explanation.
func fixLine(t *testing.T, reason string) string {
	t.Helper()
	for _, line := range strings.Split(reason, "\n") {
		if strings.HasPrefix(line, "Fix: ") {
			return line
		}
	}
	t.Fatalf("no Fix line in:\n%s", reason)
	return ""
}

// rewriteEmbeddedRepository repoints an installed hook at a repository root that
// is not the one git resolves, which is what a moved or renamed directory
// produces without needing to move anything.
func rewriteEmbeddedRepository(t *testing.T, hook, from, to string) {
	t.Helper()
	content, err := os.ReadFile(hook)
	if err != nil {
		t.Fatalf("read the installed hook: %v", err)
	}
	rewritten := strings.Replace(string(content),
		"peasant_hook_repository='"+from+"'", "peasant_hook_repository='"+to+"'", 1)
	if rewritten == string(content) {
		t.Fatalf("the installed hook does not pin %s, so the moved state cannot be produced", from)
	}
	if err := os.WriteFile(hook, []byte(rewritten), githooks.ScriptMode); err != nil {
		t.Fatalf("rewrite the installed hook: %v", err)
	}
	if !githooks.IsManaged([]byte(rewritten)) {
		t.Fatal("the rewritten hook is no longer recognizable Peasant content, so the case under test is not the one being driven")
	}
}

// TestLifecycle_EveryPrintedRemedyCarriesTheBoundPaths holds the whole package to
// the invariant that a printed remedy is runnable from the state that printed it.
//
// A hook installed with --config-dir/--data-dir resolves a different config and
// store than the defaults do. So a remedy rendered WITHOUT those paths is not a
// weaker version of the right command, it is a command that acts on a different
// installation: following it reports nothing to remove, or removes nothing,
// while the hook the user is trying to get rid of keeps firing.
//
// The trap this guards is specific. The package ships bound and unbound builders
// of the same command, and the unbound one silently produces a plausible string,
// so a call site that reaches for the wrong one is invisible in review and in any
// test whose binding is empty - the two renderings are identical there. Both
// sites below are asserted separately, and each asserts the UNBOUND form is
// absent as well as the bound form present, so a fix at one site cannot mask an
// omission at the other and a partially-bound rendering cannot pass.
func TestLifecycle_EveryPrintedRemedyCarriesTheBoundPaths(t *testing.T) {
	t.Parallel()
	binding := githooks.Binding{
		ConfigDir: filepath.Join(t.TempDir(), "config dir with '$'"),
		DataDir:   filepath.Join(t.TempDir(), "data dir with '$'"),
	}
	event := githooks.EventPostCommit

	t.Run("the linked-worktree disclosure", func(t *testing.T) {
		t.Parallel()
		main := disposableRepo(t)
		mustGit(t, main, "commit", "--quiet", "--allow-empty", "-m", "base")
		mustGit(t, main, "worktree", "add", "--quiet", "-b", "bound-remedy-test",
			filepath.Join(filepath.Dir(main), "linked"))

		report, err := githooks.New(githooks.NewExecGit()).Install(t.Context(), githooks.Request{
			Dir: main, Events: []githooks.Event{event}, Binding: binding,
		})
		if err != nil {
			t.Fatal(err)
		}
		warning, found := findWarning(report.Results[0].Warnings, githooks.WarningSharedWithLinkedWorktrees)
		if !found {
			t.Fatalf("installing from the main worktree must disclose the linked worktrees; warnings: %v",
				report.Results[0].Warnings)
		}
		assertBoundRemedy(t, warning.Detail,
			githooks.UninstallCommandWithBinding(event, main, binding),
			githooks.UninstallCommand(event, main))
	})

	t.Run("the shared-path uninstall option", func(t *testing.T) {
		t.Parallel()
		main := disposableRepo(t)
		mustGit(t, main, "commit", "--quiet", "--allow-empty", "-m", "base")
		linked := filepath.Join(filepath.Dir(main), "linked")
		mustGit(t, main, "worktree", "add", "--quiet", "-b", "shared-bound-test", linked)
		lifecycle := githooks.New(githooks.NewExecGit())
		// The hook has to be seeded from the repository that owns the hooks
		// directory: that is the only way a managed hook legitimately occupies a
		// path the linked worktree shares.
		if _, err := lifecycle.Install(t.Context(), githooks.Request{
			Dir: main, Events: []githooks.Event{event}, Binding: binding,
		}); err != nil {
			t.Fatal(err)
		}
		// Uninstalling FROM the linked worktree is refused, because that file
		// belongs to every worktree resolving to it - and the refusal offers to
		// do it from the owner instead.
		report, err := lifecycle.Uninstall(t.Context(), githooks.Request{
			Dir: linked, Events: []githooks.Event{event}, Binding: binding,
		})
		if err != nil {
			t.Fatal(err)
		}
		if outcome := report.Results[0].Outcome; outcome != githooks.OutcomeRefused {
			t.Fatalf("uninstall outcome = %q, want %q for a shared hook path", outcome, githooks.OutcomeRefused)
		}
		assertBoundRemedy(t, report.Results[0].Reason,
			githooks.UninstallCommandWithBinding(event, main, binding),
			githooks.UninstallCommand(event, main))
	})
}

// assertBoundRemedy requires the bound rendering and forbids the unbound one.
//
// The unbound form is not a substring of the bound form - the bound paths sit
// between the program and its subcommand - so forbidding it is a real assertion
// rather than one the bound form satisfies by accident.
func assertBoundRemedy(t *testing.T, text, bound, unbound string) {
	t.Helper()
	if !strings.Contains(text, bound) {
		t.Errorf("the printed remedy must carry the bound paths of the install that produced it.\nwant: %s\ngot:\n%s",
			bound, text)
	}
	if strings.Contains(text, unbound) {
		t.Errorf("the printed remedy renders the command WITHOUT its bound paths, so it resolves a different config and store than the hook it is about.\nunbound form present: %s\ngot:\n%s",
			unbound, text)
	}
}
