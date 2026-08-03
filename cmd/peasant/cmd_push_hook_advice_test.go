package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
)

// countingRootResolver answers exactly like the production resolver and records
// how often the subprocess behind it was actually needed.
type countingRootResolver struct {
	inner ingest.ExecGitResolver
	calls atomic.Int64
}

func (c *countingRootResolver) ResolveRepositoryRoot(ctx context.Context, dir string) (string, error) {
	c.calls.Add(1)
	return c.inner.ResolveRepositoryRoot(ctx, dir)
}

// TestWorktreeMembership_AnswersFromTheFilesystemWithoutASubprocess holds the
// per-commit cost of resolving a repository scope.
//
// The question "does this recorded directory belong to this worktree" was one
// `git rev-parse --show-toplevel` per recorded directory — about 1.24ms each,
// paid before any upload on every single commit: 0.28s at 150 recorded
// directories, 0.84s at 600. At that scale it can swallow a hook's whole time
// budget on local work while the failure message blames the village.
//
// Git decides it by walking up for a repository boundary, which is pure lstat,
// so the walk is done directly and memoized across shared ancestors. What the
// filesystem cannot settle still falls back to git, so the answer is never a
// guess — and the answers here are checked against git's own.
func TestWorktreeMembership_AnswersFromTheFilesystemWithoutASubprocess(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "monorepo")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksGit(t, root, "", "init", "--quiet", "--initial-branch=main")

	const recordedDirectories = 150
	inside := make([]string, 0, recordedDirectories)
	for i := range recordedDirectories {
		dir := filepath.Join(root, "services", fmt.Sprintf("svc-%03d", i), "src")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		inside = append(inside, dir)
	}
	nested := filepath.Join(root, "vendor", "lib")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksGit(t, nested, "", "init", "--quiet", "--initial-branch=main")
	missing := filepath.Join(root, "services", "deleted-long-ago")

	resolver := &countingRootResolver{}
	membership := newWorktreeMembership(root)
	ctx := t.Context()
	for _, dir := range inside {
		if !membership.belongs(ctx, resolver, dir) {
			t.Fatalf("%s is an ordinary subdirectory of %s and must be in scope", dir, root)
		}
	}
	if membership.belongs(ctx, resolver, nested) {
		t.Errorf("%s is its own repository and must never inherit %s's scope", nested, root)
	}
	// A recorded directory that no longer exists is not part of the worktree —
	// and is exactly the state in which re-ingesting would destroy the sessions,
	// so it must not be admitted or treated as evidence.
	if membership.belongs(ctx, resolver, missing) {
		t.Errorf("%s does not exist and must not be admitted", missing)
	}
	if calls := resolver.calls.Load(); calls != 0 {
		t.Errorf("resolving %d recorded directories cost %d git subprocess(es); the filesystem settles every one of these",
			len(inside)+2, calls)
	}

	// What the walk cannot settle is still asked of git rather than guessed.
	linkTarget := filepath.Join(root, "services", "svc-000")
	linked := filepath.Join(root, "services", "svc-via-symlink")
	if err := os.Symlink(linkTarget, linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !membership.belongs(ctx, resolver, linked) {
		t.Errorf("%s resolves inside %s and must be in scope", linked, root)
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Errorf("a symlinked component must fall back to git exactly once, got %d calls", calls)
	}
}

// TestPushCmd_ABudgetSpentLocallyDoesNotBlameTheVillage covers the state a hook
// with a short budget on a large machine actually reaches.
//
// With the village not running and a small cap, the whole budget is consumed by
// LOCAL work — resolving which recorded sessions belong to this repository —
// and zero HTTP requests are made. The message asserted "the village did not
// answer in time" regardless, which names the wrong culprit and sends the user
// to check a network that was never used.
func TestPushCmd_ABudgetSpentLocallyDoesNotBlameTheVillage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestCredentials(t, dir)
	seedPushableSession(t, dir)
	repo := filepath.Join(dir, "repository")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	hooksGit(t, repo, "", "init", "--quiet", "--initial-branch=main")

	_, _, err := executePushCmdSeparate(t, dir,
		[]string{"--non-interactive", "--quiet", "--timeout", "1ms", "--repository", repo})
	if err == nil {
		t.Fatal("a push whose budget expired must fail")
	}
	if strings.Contains(err.Error(), "the village did not answer in time") {
		t.Errorf("no village request was made; the budget went on local work: %v", err)
	}
	for _, want := range []string{
		"ran out of its 1ms budget",
		"before any village request was made",
		"The village was never contacted",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must state %q; got: %v", want, err)
		}
	}
	// Nothing was sent, so neither "let it finish over several runs" nor "the
	// remaining sessions do not fit" is true.
	if strings.Contains(err.Error(), "got through and are recorded as published") {
		t.Errorf("nothing was uploaded; the recovery must not claim progress: %v", err)
	}
	assertBudgetRecoveryKeepsContainment(t, err, repo, dir)
}

// assertBudgetRecoveryKeepsContainment holds every command a budget failure
// prints to the containment the run was given.
//
// Two of them lost it. The by-hand retry dropped --repository whenever the cap
// expired before the scope resolved, so following it pushes EVERY project on the
// machine rather than the one the hook was confined to — with public visibility,
// every session on the machine. And the reinstall command omitted --dir: run
// from outside a repository it errors, and run from an unrelated repository it
// silently installs an upload hook THERE while the repository that actually
// timed out keeps its old cap.
func assertBudgetRecoveryKeepsContainment(t *testing.T, err error, repo, boundDir string) {
	t.Helper()
	message := err.Error()
	quoted := githooks.ShellQuote(repo)

	if !strings.Contains(message, "--repository "+quoted) {
		t.Errorf("the budget failure does not confine its commands to %s; got: %v", repo, err)
	}
	if strings.Contains(message, "Run 'peasant village push' by hand") {
		t.Errorf("the by-hand retry dropped --repository, so following it pushes every project on the machine; got: %v", err)
	}
	if !strings.Contains(message, "hooks install") {
		t.Fatalf("the budget failure names no reinstall command; got: %v", err)
	}
	for _, event := range githooks.AllEvents {
		bare := "peasant village hooks install --event " + event.String() + " --timeout"
		if strings.Contains(message, bare) {
			t.Errorf("the reinstall command for %s omits --dir, so it acts on whichever repository the user is standing in; got: %v",
				event, err)
		}
	}
	if !strings.Contains(message, "hooks install --event "+githooks.EventPostCommit.String()+" --dir "+quoted+" --timeout") {
		t.Errorf("the reinstall command does not name %s, the repository whose cap expired; got: %v", repo, err)
	}
	for _, flag := range []string{"--config-dir", "--data-dir"} {
		if !strings.Contains(message, flag+" "+githooks.ShellQuote(boundDir)) {
			t.Errorf("the recovery dropped %s, so it would inspect or reinstall against a different Peasant state directory; got: %v", flag, err)
		}
	}
}

// TestPushRun_AScopeTimeoutStaysInTheScopePhase proves the budget narrative
// written for a cap exhausted during scope resolution can actually be reached.
//
// The phase was reset to local BEFORE the scope error was checked, so the scope
// branch was unreachable code: a run that genuinely spent its whole cap
// resolving which recorded sessions belong to a repository — the one local step
// whose cost grows with the number of recorded projects — reported that the cap
// expired while loading credentials and config, and sent the user to look at the
// wrong thing.
func TestPushRun_AScopeTimeoutStaysInTheScopePhase(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	seedPushableSession(t, dir)
	db, err := store.Open(string(defaults.ResolveDBFilePathWith(dir)))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repo := hooksTestRepo(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	run := pushRun{requestedRepository: repo}
	if _, err := run.resolveScope(ctx, db, repo); err == nil {
		t.Fatal("resolving a repository scope after its deadline must fail")
	}
	if run.phase != pushPhaseScope {
		t.Errorf("phase after a failed scope resolution = %v, want the scope phase: a budget error built from any other phase blames local work that had already finished", run.phase)
	}
	if run.repository() != repo {
		t.Errorf("repository() = %q, want the requested %q: a failure during resolution still has to name what the caller asked to be confined to",
			run.repository(), repo)
	}

	// And the narrative that phase selects has to be the scope one.
	budgetErr := uploadBudgetExceededError(time.Second, run)
	if !strings.Contains(budgetErr.Error(), "resolving which recorded sessions belong to this repository") {
		t.Errorf("the budget error does not use the scope-phase narrative; got: %v", budgetErr)
	}
	if !strings.Contains(budgetErr.Error(), "--repository "+githooks.ShellQuote(repo)) {
		t.Errorf("the budget error drops the repository it was confined to; got: %v", budgetErr)
	}
}

// TestApplyUploadBudgetError_DoesNotRetroactivelyFailSuccess covers the narrow
// race between completing the work and inspecting the budget context. The
// deadline may fire in that gap, but all requested work already succeeded.
func TestApplyUploadBudgetError_DoesNotRetroactivelyFailSuccess(t *testing.T) {
	t.Parallel()
	if err := applyUploadBudgetError(time.Second, context.DeadlineExceeded, nil, pushRun{}); err != nil {
		t.Fatalf("a successful run became a timeout failure after its work completed: %v", err)
	}

	operationErr := errors.New("operation was interrupted")
	err := applyUploadBudgetError(time.Second, context.DeadlineExceeded, operationErr, pushRun{})
	if err == nil || !strings.Contains(err.Error(), "ran out of its 1s budget") {
		t.Fatalf("an operation actually ended by its deadline was not rewritten as the actionable budget error: %v", err)
	}
	if strings.Contains(err.Error(), "--repository ''") {
		t.Fatalf("an unscoped recovery invented an empty repository flag: %v", err)
	}

	requested := &atomic.Bool{}
	run := pushRun{
		villageRequested: requested,
		annotationSummary: &push.AnnotationPushSummary{
			Created: 1,
		},
	}
	run.markVillageRequest()
	err = uploadBudgetExceededError(time.Second, run)
	if !strings.Contains(err.Error(), "0 transcript(s) and 1 annotation change(s) got through") || strings.Contains(err.Error(), "no upload progress") {
		t.Fatalf("annotation-only progress was reported as no progress: %v", err)
	}
}

// TestPushCmd_FailureBeforeAnyVillageRequestExitsAsNothingAttempted gives a
// generated hook the one fact it cannot read out of prose.
//
// A hook sees an exit status and nothing else, and it prints "whatever the
// upload finished before it stopped is on the village and is recorded as
// published" whenever the command failed. After an expired login nothing was
// ever sent, so that sentence describes an upload that never started.
func TestPushCmd_FailureBeforeAnyVillageRequestExitsAsNothingAttempted(t *testing.T) {
	t.Parallel()

	t.Run("an expired login never reached the village", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir() // no credentials written
		_, _, err := executePushCmdSeparate(t, dir, []string{"--non-interactive", "--quiet"})
		if err == nil {
			t.Fatal("a push without credentials must fail")
		}
		if got := exitCodeFor(err); got != defaults.ExitNothingAttempted {
			t.Errorf("exit code = %s, want %s so a hook can say nothing was published",
				got, defaults.ExitNothingAttempted)
		}
	})

	t.Run("a failure at the village keeps the ordinary status", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// Port 1 is not listening, so every upload is a connection failure and
		// the run aborts after three of them. It got as far as trying, so
		// anything it had managed to publish first would be real.
		writeTestCredentialsFor(t, dir, "http://127.0.0.1:1")
		var cfgPath string
		for _, id := range []string{
			"dddd4444-dddd-4ddd-8ddd-dddddddddddd",
			"eeee5555-eeee-4eee-8eee-eeeeeeeeeeee",
			"ffff6666-ffff-4fff-8fff-ffffffffffff",
		} {
			cfgPath = seedUploadableSession(t, dir, id)
		}
		_, _, err := executePushCmdSeparate(t, dir,
			[]string{"--non-interactive", "--quiet", "--config=" + cfgPath})
		if err == nil {
			t.Fatal("a push to a village that is not there must fail")
		}
		if got := exitCodeFor(err); got != defaults.ExitFailure {
			t.Errorf("exit code = %s, want %s: the run did reach the village (%v)", got, defaults.ExitFailure, err)
		}
	})
}
