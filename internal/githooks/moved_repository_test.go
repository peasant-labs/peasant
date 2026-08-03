package githooks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/githooks"
)

// TestScript_MovedRepositoryPointsAtTheResolvedWorktree covers the one common
// failure state that had no usable remedy on the surface it appeared on.
//
// A hook pins the repository it was installed for, so after the directory is
// renamed or moved every command the warning prints names a path that is not
// there: `where`, `fix`, and `stop` — the escape hatch — all fail with
// "No such file or directory" or "is not inside a Git worktree". The correct
// commands existed only in 'peasant village hooks status', which a user who
// just committed has no reason to run.
//
// The hook knows the root it was written for and git can say where the worktree
// is now, so both are compared and every command is rendered against the
// resolved root instead.
func TestScript_MovedRepositoryPointsAtTheResolvedWorktree(t *testing.T) {
	t.Parallel()
	original := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	binding := githooks.Binding{
		ConfigDir: filepath.Join(filepath.Dir(original), "bound config's directory"),
		DataDir:   filepath.Join(filepath.Dir(original), "bound data directory"),
	}
	if _, err := lifecycle.Install(t.Context(), githooks.Request{
		Dir: original, Events: []githooks.Event{githooks.EventPostCommit},
		Binding: binding,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	moved := filepath.Join(filepath.Dir(original), "moved-repo's copy")
	if err := os.Rename(original, moved); err != nil {
		t.Fatalf("move the repository: %v", err)
	}
	hook := filepath.Join(moved, ".git", "hooks", githooks.EventPostCommit.String())

	binDir, log := stubPeasant(t, 0)
	_, stderr, err := runHook(t, moved, hook, binDir, "")
	if err != nil {
		t.Fatalf("a moved repository must not break the commit: %v", err)
	}

	for _, want := range []string{
		"is not at " + original + " any more",
		"the upload was not attempted",
		githooks.InstallCommandWithBinding(githooks.EventPostCommit, moved, binding),
		githooks.UninstallCommandWithBinding(githooks.EventPostCommit, moved, binding),
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the moved-repository warning must state %q; got:\n%s", want, stderr)
		}
	}
	// Every command offered must be runnable from where the user is now. The
	// old root may only appear as the thing being reported as gone.
	for _, line := range strings.Split(stderr, "\n") {
		label, value, found := strings.Cut(line, ":")
		field := strings.TrimSpace(label)
		if !found || (field != "fix" && field != "stop") {
			continue
		}
		if strings.Contains(value, original+"'") || strings.Contains(value, original+" ") {
			t.Errorf("the %s line must not name the directory that no longer exists; got:%s", field, value)
		}
	}
	// The pinned upload can only fail against a path that is gone, and it costs
	// the whole time budget on every commit to find that out.
	if recorded := readFile(t, log); strings.TrimSpace(recorded) != "" {
		t.Errorf("a doomed upload against a missing repository must not be attempted; peasant was called with:\n%s", recorded)
	}
}

// TestLifecycle_InstallDisclosesLinkedWorktreesThatShareTheHook closes the
// asymmetry in what the two sides of the same fact are told.
//
// Installing FROM a linked worktree is refused with a full explanation.
// Installing from the main worktree writes a hook that every linked worktree
// also executes — and said nothing at all. It is not a leak, because the upload
// stays pinned to this repository's own sessions, but it is a fact consent
// depends on, so it belongs on the path that succeeds.
func TestLifecycle_InstallDisclosesLinkedWorktreesThatShareTheHook(t *testing.T) {
	t.Parallel()
	lifecycle := githooks.New(githooks.NewExecGit())

	t.Run("no linked worktrees, nothing to disclose", func(t *testing.T) {
		t.Parallel()
		repo := disposableRepo(t)
		report, err := lifecycle.Install(t.Context(), githooks.Request{
			Dir: repo, Events: []githooks.Event{githooks.EventPostCommit},
		})
		if err != nil {
			t.Fatal(err)
		}
		if warning, found := findWarning(report.Results[0].Warnings, githooks.WarningSharedWithLinkedWorktrees); found {
			t.Errorf("an ordinary repository must not be told about worktrees it does not have: %s", warning.Detail)
		}
	})

	t.Run("linked worktrees run the same hook", func(t *testing.T) {
		t.Parallel()
		main := disposableRepo(t)
		mustGit(t, main, "commit", "--quiet", "--allow-empty", "-m", "base")
		linked := filepath.Join(filepath.Dir(main), "linked")
		mustGit(t, main, "worktree", "add", "--quiet", "-b", "disclosure-test", linked)

		report, err := lifecycle.Install(t.Context(), githooks.Request{
			Dir: main, Events: []githooks.Event{githooks.EventPostCommit},
		})
		if err != nil {
			t.Fatal(err)
		}
		if report.Results[0].Outcome != githooks.OutcomeCreated {
			t.Fatalf("install outcome = %s: %s", report.Results[0].Outcome, report.Results[0].Reason)
		}
		warning, found := findWarning(report.Results[0].Warnings, githooks.WarningSharedWithLinkedWorktrees)
		if !found {
			t.Fatalf("installing from the main worktree must disclose that linked worktrees run it too; warnings: %v",
				report.Results[0].Warnings)
		}
		for _, want := range []string{filepath.Base(linked), "1 linked worktree(s)", "only " + main + "'s own recorded sessions"} {
			if !strings.Contains(warning.Detail, want) {
				t.Errorf("the disclosure must state %q; got: %s", want, warning.Detail)
			}
		}
		// Status has to keep saying it: the worktree can be added after the
		// install, and status is the surface that answers "what does this do".
		status, err := lifecycle.Status(t.Context(), githooks.Request{Dir: main})
		if err != nil {
			t.Fatal(err)
		}
		if _, found := findWarning(findStatus(t, status, githooks.EventPostCommit).Warnings,
			githooks.WarningSharedWithLinkedWorktrees); !found {
			t.Error("status must repeat the linked-worktree disclosure")
		}
	})
}
