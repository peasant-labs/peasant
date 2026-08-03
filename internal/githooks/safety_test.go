package githooks_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
)

// TestLifecycle_LeavesDeveloperStateUntouched is the evidence behind the promise
// that running this suite cannot damage a developer's machine. It fingerprints
// the hooks directory of the checkout the tests are running from, plus the git
// configuration files git would read for that checkout, drives a full
// install/status/uninstall cycle in a disposable repository, and requires every
// fingerprint to be identical afterwards.
func TestLifecycle_LeavesDeveloperStateUntouched(t *testing.T) {
	t.Parallel()
	workspaceHooks := developerHooksDir(t)
	configPaths := developerGitConfigPaths()

	before := developerStateFingerprint(t, workspaceHooks, configPaths)

	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	ctx := t.Context()
	both := []githooks.Event{githooks.EventPostCommit, githooks.EventPrePush}
	if _, err := lifecycle.Install(ctx, githooks.Request{Dir: repo, Events: both}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := lifecycle.Status(ctx, githooks.Request{Dir: repo}); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := lifecycle.Uninstall(ctx, githooks.Request{Dir: repo}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	if after := developerStateFingerprint(t, workspaceHooks, configPaths); after != before {
		t.Errorf("the hook lifecycle changed state outside its disposable repository\n  before:\n%s\n  after:\n%s", before, after)
	}
}

// TestLifecycle_NeverChangesTheRepositoryGitConfiguration proves Peasant reaches
// the effective hook path by asking git, and never by setting the configuration
// that selects the hooks directory.
func TestLifecycle_NeverChangesTheRepositoryGitConfiguration(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	before := mustGit(t, repo, "config", "--local", "--list")

	lifecycle := githooks.New(githooks.NewExecGit())
	ctx := t.Context()
	both := []githooks.Event{githooks.EventPostCommit, githooks.EventPrePush}
	if _, err := lifecycle.Install(ctx, githooks.Request{Dir: repo, Events: both}); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := lifecycle.Uninstall(ctx, githooks.Request{Dir: repo}); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	if after := mustGit(t, repo, "config", "--local", "--list"); after != before {
		t.Errorf("the repository git configuration changed\n  before:\n%s\n  after:\n%s", before, after)
	}
	if _, err := runGit(repo, os.Environ(), "config", "--get", "core.hooksPath"); err == nil {
		t.Error("core.hooksPath is set after the lifecycle ran; Peasant must never write it")
	}
}

// TestLifecycle_SymlinkedSlotIsForeign covers a slot pointing somewhere else
// entirely. Peasant must never follow it, rewrite through it, or delete it —
// and it must say WHY correctly.
//
// A symlink in an ordinary .git/hooks is foreign CONTENT, not a shared hooks
// directory. Diagnosing it as a shared directory claims a file lexically inside
// the repository is outside it, which hides the real cause.
//
// Both targets here live OUTSIDE the hooks directory, so the by-hand section is
// withheld and the refusal has to say so and name the file git really runs.
// Offering it would address a machine-pinned upload section to a file the
// repository does not own — the same leak the shared-path refusal prevents — and
// would be decided without ever reading what interpreter that file needs. The
// case where the section IS correct, a target inside the hooks directory that
// git runs with a shell, is driven by the linked-slot corpus.
func TestLifecycle_SymlinkedSlotIsForeign(t *testing.T) {
	t.Parallel()
	for _, targetOutsideRepo := range []bool{false, true} {
		name := "target inside the worktree"
		if targetOutsideRepo {
			name = "target outside the worktree"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := disposableRepo(t)
			targetDir := repo
			if targetOutsideRepo {
				targetDir = t.TempDir()
			}
			target := filepath.Join(targetDir, "shared-hook.sh")
			if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			hook := hookPath(t, repo, githooks.EventPostCommit.String())
			if err := os.Symlink(target, hook); err != nil {
				t.Fatalf("create symlinked hook: %v", err)
			}
			targetBefore := fileFingerprint(t, target)

			lifecycle := githooks.New(githooks.NewExecGit())
			ctx := t.Context()
			request := githooks.Request{Dir: repo, Events: []githooks.Event{githooks.EventPostCommit}}

			installed, err := lifecycle.Install(ctx, request)
			if err != nil {
				t.Fatalf("install: %v", err)
			}
			result := installed.Results[0]
			if result.Outcome != githooks.OutcomeRefused {
				t.Errorf("install outcome = %q, want %q for a symlinked slot", result.Outcome, githooks.OutcomeRefused)
			}
			if result.Refusal != githooks.RefusalForeignFile {
				t.Errorf("refusal = %q, want %q: the hooks directory is inside the worktree; only the slot's CONTENT is foreign.\nreason: %s",
					result.Refusal, githooks.RefusalForeignFile, result.Reason)
			}
			if strings.TrimSpace(result.Manual) != "" {
				t.Errorf("the by-hand section was offered for a link that leaves the hooks directory (%s -> %s): pasting it would put a machine-pinned upload into a file this repository does not own",
					hook, target)
			}
			if !strings.Contains(result.Reason, target) {
				t.Errorf("the refusal does not name %s, the file git really runs and the only one the user can change:\n%s", target, result.Reason)
			}

			removed, err := lifecycle.Uninstall(ctx, request)
			if err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			if got := removed.Results[0].Outcome; got == githooks.OutcomeRemoved {
				t.Error("uninstall must never delete a symlinked slot Peasant did not create")
			}

			link, err := os.Lstat(hook)
			if err != nil || link.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the symlink at %s must be left exactly as it was (lstat err=%v)", hook, err)
			}
			if got := fileFingerprint(t, target); got != targetBefore {
				t.Errorf("the symlink target was modified: %s became %s", targetBefore, got)
			}
		})
	}
}

// TestLifecycle_EditedManagedHookStillUploads proves that a generated hook whose
// framing was broken is reported as still uploading, and that the refusal hands
// back the three remediation steps required for an ownership mismatch: inspect,
// remove by hand, reinstall.
//
// Appending to an existing hook is ordinary tooling behaviour. Before this, the
// appended file was reported as "nothing of Peasant's to remove ... no upload
// runs from it" with exit 0, while the upload kept running on every commit.
func TestLifecycle_EditedManagedHookStillUploads(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	ctx := t.Context()
	request := githooks.Request{Dir: repo, Events: []githooks.Event{githooks.EventPostCommit}}

	if _, err := lifecycle.Install(ctx, request); err != nil {
		t.Fatalf("install: %v", err)
	}
	hook := hookPath(t, repo, githooks.EventPostCommit.String())
	file, err := os.OpenFile(hook, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("append to the installed hook: %v", err)
	}
	if _, err := file.WriteString("\n# another tool appended this line\n"); err != nil {
		t.Fatalf("append to the installed hook: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("append to the installed hook: %v", err)
	}
	editedBefore := fileFingerprint(t, hook)

	status, err := lifecycle.Status(ctx, request)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(status.Plans[0].Reason, "no upload hook is active") {
		t.Errorf("status must not claim no upload is active while the upload line is still in the file:\n%s", status.Plans[0].Reason)
	}

	report, err := lifecycle.Uninstall(ctx, request)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	result := report.Results[0]
	if result.Outcome != githooks.OutcomeRefused {
		t.Fatalf("uninstall outcome = %q, want %q; the upload still runs and only the user can stop it.\nreason: %s",
			result.Outcome, githooks.OutcomeRefused, result.Reason)
	}
	if !report.Blocked() {
		t.Error("a refusal must make the command exit non-zero, or a script believes the hook is gone")
	}
	for _, want := range []string{
		"peasant village hooks status",          // inspect
		githooks.ScriptMarkerBegin,              // exact lines to remove by hand
		githooks.ScriptMarkerEnd,                //
		"peasant village hooks install --event", // reinstall
		"still uploads on every post-commit",    // the impact
	} {
		if !strings.Contains(result.Reason, want) {
			t.Errorf("the refusal must state %q; got:\n%s", want, result.Reason)
		}
	}
	if got := fileFingerprint(t, hook); got != editedBefore {
		t.Errorf("the edited hook was modified: %s became %s", editedBefore, got)
	}
}

// TestLifecycle_RefusesOutsideAGitRepository proves the failure is actionable
// and that nothing is written when there is no repository to write into.
func TestLifecycle_RefusesOutsideAGitRepository(t *testing.T) {
	t.Parallel()
	plain := t.TempDir()
	lifecycle := githooks.New(githooks.NewExecGit())
	_, err := lifecycle.Install(t.Context(), githooks.Request{
		Dir:    plain,
		Events: []githooks.Event{githooks.EventPostCommit},
	})
	if err == nil {
		t.Fatal("installing outside a git worktree must fail")
	}
	for _, want := range []string{"not inside a Git worktree", "Fix:", plain} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must name %q; got: %v", want, err)
		}
	}
	entries, readErr := os.ReadDir(plain)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Errorf("nothing must be created outside a repository; found %d entries", len(entries))
	}
}

// TestLifecycle_RequiresAnExplicitEvent proves no hook is ever installed from a
// defaulted or implied choice, while status and uninstall still cover both.
func TestLifecycle_RequiresAnExplicitEvent(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	ctx := t.Context()

	if _, err := lifecycle.Install(ctx, githooks.Request{Dir: repo}); err == nil {
		t.Error("install with no event must fail rather than pick events for the user")
	}
	status, err := lifecycle.Status(ctx, githooks.Request{Dir: repo})
	if err != nil {
		t.Fatalf("status with no event must report every managed event: %v", err)
	}
	if len(status.Plans) != len(githooks.AllEvents) {
		t.Errorf("status reported %d events, want %d", len(status.Plans), len(githooks.AllEvents))
	}
	uninstalled, err := lifecycle.Uninstall(ctx, githooks.Request{Dir: repo})
	if err != nil {
		t.Fatalf("uninstall with no event must consider every managed event: %v", err)
	}
	if len(uninstalled.Results) != len(githooks.AllEvents) {
		t.Errorf("uninstall reported %d events, want %d", len(uninstalled.Results), len(githooks.AllEvents))
	}
}

// TestLifecycle_RejectsUnsupportedEvent keeps the event set closed.
func TestLifecycle_RejectsUnsupportedEvent(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	_, err := githooks.New(githooks.NewExecGit()).Install(t.Context(), githooks.Request{
		Dir:    repo,
		Events: []githooks.Event{githooks.Event("pre-commit")},
	})
	if err == nil {
		t.Fatal("an unmanaged event must be rejected")
	}
	if !strings.Contains(err.Error(), "unsupported hook event") {
		t.Errorf("error must explain the closed event set; got: %v", err)
	}
	if slotExists(filepath.Join(repo, ".git", "hooks", "pre-commit")) {
		t.Error("a rejected event must not create a file")
	}
}

// --- developer state helpers ------------------------------------------------

// developerHooksDir resolves the hooks directory of the checkout these tests run
// from, using the environment the binary started with rather than the redirected
// one, so the guard inspects real state.
func developerHooksDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	path, err := runGit(cwd, originalEnvironment, "rev-parse", "--git-path", "hooks")
	if err != nil {
		t.Skipf("tests are not running inside a git checkout, nothing to guard: %v", err)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path)
}

// developerGitConfigPaths lists the files git would read as global
// configuration for the developer, derived from the captured environment.
func developerGitConfigPaths() []string {
	home := environmentValue(originalEnvironment, envHome)
	configHome := environmentValue(originalEnvironment, defaults.EnvXDGConfigHome.String())
	if configHome == "" && home != "" {
		configHome = filepath.Join(home, ".config")
	}
	var paths []string
	if home != "" {
		paths = append(paths, filepath.Join(home, ".gitconfig"))
	}
	if configHome != "" {
		paths = append(paths, filepath.Join(configHome, "git", "config"))
	}
	return paths
}

func developerStateFingerprint(t *testing.T, hooksDir string, configPaths []string) string {
	t.Helper()
	lines := []string{"hooks(" + hooksDir + "):\n" + directoryFingerprint(t, hooksDir)}
	for _, path := range configPaths {
		lines = append(lines, path+" "+pathFingerprint(t, path))
	}
	return strings.Join(lines, "\n")
}

func pathFingerprint(t *testing.T, path string) string {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		return "absent"
	}
	return fileFingerprint(t, path)
}
