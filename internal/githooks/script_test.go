package githooks_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/githooks"
)

// uploadArgv is the argv every generated hook passes to the peasant binary,
// after any bound path overrides. It lives here once so a change to the single
// shared command builder is one fixture edit rather than N inline literals.
//
// --quiet is part of the contract: a hook fires on every commit or push, and the
// default summary would print several lines into an ordinary git command.
// --timeout is part of it for the same reason: the client's own timeout is per
// request and a push issues several in sequence, so without an overall budget a
// village that accepts a connection and never answers stalls git for minutes.
// The budget is read from the constant so the fixture cannot drift from it.
var uploadArgv = []string{
	"village", "push", "--non-interactive", "--quiet",
	"--timeout", githooks.DefaultUploadBudget.String(),
}

// wantRepositoryArgv builds the full argv the stub peasant must record for a
// hook installed in repo, with any bound flags the caller passes first.
func wantRepositoryArgv(repo string, bound ...string) []string {
	argv := append([]string{}, bound...)
	argv = append(argv, uploadArgv...)
	return append(argv, "--repository", repo)
}

// TestScript_IsPortableShell syntax-checks the generated hook for both events
// with the shell git will use to run it.
func TestScript_IsPortableShell(t *testing.T) {
	t.Parallel()
	for _, event := range githooks.AllEvents {
		script, err := githooks.Script(event, "/tmp/repo", "/tmp/repo/.git/hooks/"+event.String(), githooks.Binding{})
		if err != nil {
			t.Fatalf("render %s script: %v", event, err)
		}
		assertShellParses(t, event.String()+"-hook", script)
		if !isASCII(script) {
			t.Errorf("%s script must stay ASCII so it behaves the same under every locale git runs it in", event)
		}
	}
}

// TestScript_PrePushDrainsStdinAndPostCommitDoesNot pins the one behavioral
// difference between the two generated hooks. Git streams the refs being pushed
// into a pre-push hook, and a post-commit hook inherits whatever stdin git has,
// which may be the user's terminal.
func TestScript_PrePushDrainsStdinAndPostCommitDoesNot(t *testing.T) {
	t.Parallel()
	prePush, err := githooks.Script(githooks.EventPrePush, "/tmp/repo", "/tmp/repo/.git/hooks/pre-push", githooks.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	// The drain is asserted as a builtin read loop, not as any command that
	// consumes stdin: a drain that has to be found on PATH is one the
	// environment can remove, which puts git back to writing into a pipe
	// nobody reads - the state this section exists to prevent.
	if !strings.Contains(prePush, "while read -r ") {
		t.Errorf("the pre-push hook must drain the refs git streams on stdin, using only the shell; got:\n%s", prePush)
	}
	postCommit, err := githooks.Script(githooks.EventPostCommit, "/tmp/repo", "/tmp/repo/.git/hooks/post-commit", githooks.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(postCommit, "while read -r ") {
		t.Error("the post-commit hook must not read stdin: git may have left the user's terminal there")
	}
}

// TestScript_DescribesPrePushHonestly guards the wording that must never drift
// into claiming a push succeeded.
func TestScript_DescribesPrePushHonestly(t *testing.T) {
	t.Parallel()
	script, err := githooks.Script(githooks.EventPrePush, "/tmp/repo", "/tmp/repo/.git/hooks/pre-push", githooks.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "has not contacted the remote") {
		t.Errorf("the pre-push hook must say git has not contacted the remote yet; got:\n%s", script)
	}
}

// TestScript_BindsOnlyExplicitPaths proves an install without overrides leaves
// Peasant's normal resolution in force, and an install with them pins exactly
// those paths into the hook.
func TestScript_BindsOnlyExplicitPaths(t *testing.T) {
	t.Parallel()
	unbound := githooks.CommandLine(githooks.Binding{})
	if want := "peasant " + strings.Join(uploadArgv, " "); unbound != want {
		t.Errorf("unbound command = %q, want %q", unbound, want)
	}
	bound := githooks.CommandLine(githooks.Binding{
		ConfigPath: "/x/config.yaml",
		DataDir:    "/y/data",
	})
	for _, want := range []string{"--config '/x/config.yaml'", "--data-dir '/y/data'", strings.Join(uploadArgv, " ")} {
		if !strings.Contains(bound, want) {
			t.Errorf("bound command %q must contain %q", bound, want)
		}
	}
	if strings.Contains(bound, "--config-dir") || strings.Contains(bound, "--state-dir") {
		t.Errorf("bound command %q must not invent overrides the user did not set", bound)
	}
}

func TestScript_DisplaysExactScopedCommand(t *testing.T) {
	t.Parallel()
	root := "/tmp/repository"
	binding := githooks.Binding{ConfigPath: "/tmp/config.yaml", DataDir: "/tmp/data"}
	script, err := githooks.Script(githooks.EventPostCommit, root, root+"/.git/hooks/post-commit", binding)
	if err != nil {
		t.Fatal(err)
	}
	want := "#   command    : " + githooks.RepositoryCommand(root, binding)
	if !strings.Contains(script, want) {
		t.Fatalf("installed hook header must display its exact scoped command %q; got:\n%s", want, script)
	}
}

// TestScript_ByHandLineCarriesTheBoundPaths pins the one command in a generated
// hook that a person is meant to type.
//
// It used to be a hardcoded `peasant village push --repository <root>`, so a
// hook installed with an explicit config or data directory advertised a command
// that reads a DIFFERENT store than the hook itself — it would report nothing to
// push, or push from the wrong place. It is now derived from the same builder,
// minus the three flags that only make sense inside a hook.
func TestScript_ByHandLineCarriesTheBoundPaths(t *testing.T) {
	t.Parallel()
	root := "/tmp/repository"
	binding := githooks.Binding{ConfigDir: "/tmp/bound config", DataDir: "/tmp/bound data"}
	script, err := githooks.Script(githooks.EventPostCommit, root, root+"/.git/hooks/post-commit", binding)
	if err != nil {
		t.Fatal(err)
	}
	manual := githooks.ManualCommand(root, binding)
	if want := "cd " + shellSingleQuote(root) + " && " + manual; !strings.Contains(script, want) {
		t.Fatalf("the hook's run-it-by-hand line must be %q; got:\n%s", want, script)
	}
	for _, want := range []string{"--config-dir '/tmp/bound config'", "--data-dir '/tmp/bound data'"} {
		if !strings.Contains(manual, want) {
			t.Errorf("the by-hand command must pin the bound path %q; got: %s", want, manual)
		}
	}
	if status := githooks.StatusCommandWithBinding(root, binding); !strings.Contains(script, status) {
		t.Errorf("the generated hook's status command dropped its bound paths; want %q in:\n%s", status, script)
	}
	// The flags that exist only because a hook runs the command have no place in
	// one a person types: --timeout is the very budget they are working around,
	// and --quiet would suppress the output they ran it to see.
	for _, unwanted := range []string{"--quiet", "--timeout", "--non-interactive"} {
		if strings.Contains(manual, unwanted) {
			t.Errorf("the by-hand command must not carry the hook-only flag %q; got: %s", unwanted, manual)
		}
	}
}

// TestScript_RefusesPathsItCannotEmbed proves Peasant fails instead of writing a
// hook whose quoting or framing comments would be corrupted.
func TestScript_RefusesPathsItCannotEmbed(t *testing.T) {
	t.Parallel()
	if _, err := githooks.Script(githooks.EventPostCommit, "/tmp/re\npo", "/tmp/repo/.git/hooks/post-commit", githooks.Binding{}); err == nil {
		t.Error("a repository path containing a line break must be refused, not written into a hook")
	}
	if _, err := githooks.Script(githooks.EventPostCommit, "/tmp/repo", "/tmp/repo/.git/hooks/post-commit", githooks.Binding{DataDir: "/a\nb"}); err == nil {
		t.Error("a bound path containing a line break must be refused, not written into a hook")
	}
	if _, err := githooks.Script(githooks.Event("post-merge"), "/tmp/repo", "/tmp/repo/.git/hooks/post-merge", githooks.Binding{}); err == nil {
		t.Error("an unmanaged event must be refused")
	}
}

// TestIsManaged_OnlyClaimsPeasantGeneratedFiles is the ownership contract. The
// third case is the important one: a user who pastes the offered snippet into
// their own hook must never end up with a file Peasant later rewrites or
// deletes.
func TestIsManaged_OnlyClaimsPeasantGeneratedFiles(t *testing.T) {
	t.Parallel()
	script, err := githooks.Script(githooks.EventPostCommit, "/tmp/repo", "/tmp/repo/.git/hooks/post-commit", githooks.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	if !githooks.IsManaged([]byte(script)) {
		t.Fatal("a freshly generated hook must be recognized as Peasant's")
	}
	if !githooks.IsManaged([]byte(script + "\n\n")) {
		t.Error("trailing blank lines must not break ownership recognition")
	}
	if githooks.IsManaged([]byte("#!/bin/sh\nmake lint\n")) {
		t.Error("an unrelated hook must never be claimed")
	}
	if githooks.IsManaged(nil) {
		t.Error("an empty file must never be claimed")
	}

	snippet, err := githooks.ManualSnippet(githooks.EventPostCommit, "/tmp/repo", "/tmp/repo/.git/hooks/post-commit", githooks.Binding{})
	if err != nil {
		t.Fatal(err)
	}
	composed := "#!/bin/sh\nmake lint || exit 1\n" + snippet + "exit 0\n"
	if githooks.IsManaged([]byte(composed)) {
		t.Error("a user hook carrying the by-hand snippet must never be claimed as Peasant's")
	}
	if !githooks.ContainsManualSection([]byte(composed)) {
		t.Error("Peasant must still recognize that its by-hand section is running from that file")
	}
	if githooks.ContainsManualSection([]byte("#!/bin/sh\nmake lint\n")) {
		t.Error("an unrelated hook must not be reported as carrying the by-hand section")
	}
	assertShellParses(t, "composed-hook", composed)
}

// TestScript_RunsUploadAndNeverBlocksGit executes the generated hook the way git
// does - as an executable file in the repository - with a stub peasant on PATH.
// A failing upload must warn and still exit successfully.
func TestScript_RunsUploadAndNeverBlocksGit(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	if _, err := lifecycle.Install(t.Context(), githooks.Request{
		Dir:    repo,
		Events: []githooks.Event{githooks.EventPostCommit},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	hook := hookPath(t, repo, githooks.EventPostCommit.String())

	binDir, log := stubPeasant(t, 0)
	stdout, stderr, err := runHook(t, repo, hook, binDir, "")
	if err != nil {
		t.Fatalf("hook exited non-zero on a successful upload: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	if recorded, want := recordedArgs(t, log), wantRepositoryArgv(repo); !slices.Equal(recorded, want) {
		t.Errorf("hook invoked peasant with %#v, want %#v", recorded, want)
	}

	failingBin, _ := stubPeasant(t, 9)
	_, stderr, err = runHook(t, repo, hook, failingBin, "")
	if err != nil {
		t.Fatalf("hook must exit successfully when the upload fails, got: %v", err)
	}
	for _, want := range []string{
		"village upload did not complete",
		"exited 9",
		repo,
		hook,
		githooks.EventPostCommit.String(),
		"the commit is already recorded and is not changed",
		"peasant village hooks uninstall --event post-commit",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the failure warning must name %q; got:\n%s", want, stderr)
		}
	}
	// A non-zero exit does NOT mean nothing was published: an upload can succeed
	// and the run still fail afterwards, and the budget message peasant prints
	// says exactly that. Claiming otherwise contradicts peasant's own output.
	if strings.Contains(stderr, "nothing reached the village") {
		t.Errorf("a failed upload must not assert that nothing was published; got:\n%s", stderr)
	}
}

// TestScript_WarnsWhenPeasantIsMissing covers the environment git hooks are
// most often run in: a PATH without the peasant binary.
func TestScript_WarnsWhenPeasantIsMissing(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	if _, err := lifecycle.Install(t.Context(), githooks.Request{
		Dir:    repo,
		Events: []githooks.Event{githooks.EventPrePush},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	hook := hookPath(t, repo, githooks.EventPrePush.String())

	emptyBin := t.TempDir()
	_, stderr, err := runHook(t, repo, hook, emptyBin, "refs/heads/main abc refs/heads/main def\n")
	if err != nil {
		t.Fatalf("hook must exit successfully when peasant is missing, got: %v", err)
	}
	for _, want := range []string{"the peasant command was not found", "the push itself is unaffected"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the missing-binary warning must name %q; got:\n%s", want, stderr)
		}
	}
}

// TestScript_QuotesAwkwardRepositoryPaths installs into a repository whose path
// contains a space and a single quote, then runs the hook. A quoting mistake
// would show up as a shell error instead of a recorded upload.
func TestScript_QuotesAwkwardRepositoryPaths(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "it's a repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "--quiet", "--initial-branch=main")

	lifecycle := githooks.New(githooks.NewExecGit())
	if _, err := lifecycle.Install(t.Context(), githooks.Request{
		Dir:     repo,
		Events:  []githooks.Event{githooks.EventPostCommit},
		Binding: githooks.Binding{DataDir: filepath.Join(base, "data dir with 'quote' and $dollar")},
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	hook := hookPath(t, repo, githooks.EventPostCommit.String())
	assertShellParses(t, "awkward-path-hook", readFile(t, hook))

	binDir, log := stubPeasant(t, 0)
	stdout, stderr, err := runHook(t, repo, hook, binDir, "")
	if err != nil {
		t.Fatalf("hook failed in an awkwardly named repository: %v (stdout=%q stderr=%q)", err, stdout, stderr)
	}
	wantDataDir := filepath.Join(base, "data dir with 'quote' and $dollar")
	recorded := recordedArgs(t, log)
	want := wantRepositoryArgv(repo, "--data-dir", wantDataDir)
	if !slices.Equal(recorded, want) {
		t.Errorf("the bound data directory did not survive quoting; peasant saw %#v, want %#v", recorded, want)
	}
}

// TestScript_FailurePrintsExactBoundRecovery proves the retry a failed hook
// prints is a command that behaves the way the same sentence says it will.
//
// The advice said "drop --quiet and add --verbose for per-session detail" and
// then printed the hook's OWN capped, quiet command, so following it reproduced
// the identical failure with the identical cap and no extra detail. The by-hand
// builder that drops exactly those flags already existed two lines above in the
// same generated file.
func TestScript_FailurePrintsExactBoundRecovery(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	binding := githooks.Binding{
		ConfigDir: filepath.Join(t.TempDir(), "config dir with '$'"),
		DataDir:   filepath.Join(t.TempDir(), "data dir with '$'"),
		StateDir:  filepath.Join(t.TempDir(), "state dir with '$'"),
	}
	lifecycle := githooks.New(githooks.NewExecGit())
	if _, err := lifecycle.Install(t.Context(), githooks.Request{Dir: repo, Events: []githooks.Event{githooks.EventPostCommit}, Binding: binding}); err != nil {
		t.Fatalf("install: %v", err)
	}
	hook := hookPath(t, repo, githooks.EventPostCommit.String())
	binDir, _ := stubPeasant(t, 9)
	_, stderr, err := runHook(t, repo, hook, binDir, "")
	if err != nil {
		t.Fatalf("failed upload must not block git: %v", err)
	}
	want := githooks.ManualRecovery(repo, binding)
	if !strings.Contains(stderr, want) {
		t.Fatalf("warning omitted exact bound recovery command\nwant: %s\ngot:\n%s", want, stderr)
	}
	for _, bound := range []string{binding.ConfigDir, binding.DataDir, binding.StateDir} {
		if !strings.Contains(stderr, shellSingleQuote(bound)) {
			t.Errorf("the recovery command must pin the bound path %q; got:\n%s", bound, stderr)
		}
	}
	// The line the user is told to run must differ from the one that just
	// failed, in exactly the two ways the sentence promises.
	fix := warningField(t, stderr, "fix")
	for _, contradiction := range []string{"--quiet", "--timeout"} {
		if strings.Contains(fix, contradiction) {
			t.Errorf("the retry command must not carry %s, which the same sentence says it drops; got:\n%s",
				contradiction, fix)
		}
	}
}

// TestScript_NeedsNoUtilityBeyondGitAndPeasant runs a generated hook with
// NOTHING on PATH except the two programs it is permitted to call, and proves it
// still produces a remedy a user can run.
//
// It drives the moved-repository branch on purpose: that is the only place a
// generated hook quotes a value it discovered at runtime, and it is the branch a
// user reaches AFTER renaming their repository - so a remedy that comes out
// malformed there is malformed at the one moment it is needed. The quoting used
// to pipe through sed. With sed absent the substitution yielded nothing, every
// command printed '--dir ' with an empty argument, and the hook still exited 0:
// broken advice, delivered confidently, with no failure anywhere to notice.
//
// Two things make this test the guard rather than a restatement. The PATH is
// built from the permitted list, so the absence of sed - and of every other
// utility - is the same on a hermetic sandbox and on a machine where git shares
// /usr/bin with half of userland. And it asserts the SHELL reported nothing
// missing, which fails for any new external dependency the generated script
// picks up, not only for the one that caused this.
func TestScript_NeedsNoUtilityBeyondGitAndPeasant(t *testing.T) {
	t.Parallel()
	for _, event := range githooks.AllEvents {
		t.Run(event.String(), func(t *testing.T) {
			t.Parallel()
			original := disposableRepo(t)
			binding := githooks.Binding{
				ConfigDir: filepath.Join(filepath.Dir(original), "bound config's directory"),
				DataDir:   filepath.Join(filepath.Dir(original), "bound data directory"),
			}
			lifecycle := githooks.New(githooks.NewExecGit())
			if _, err := lifecycle.Install(t.Context(), githooks.Request{
				Dir: original, Events: []githooks.Event{event}, Binding: binding,
			}); err != nil {
				t.Fatalf("install: %v", err)
			}

			// The new name carries a single quote, which is the whole reason the
			// hook quotes at all: an unquoted or half-quoted path ends the
			// command early and the remainder is read as separate words.
			moved := filepath.Join(filepath.Dir(original), "moved-repo's copy")
			if err := os.Rename(original, moved); err != nil {
				t.Fatalf("move the repository: %v", err)
			}

			binDir, _ := stubPeasant(t, 0)
			_, stderr, err := runHook(t, moved, filepath.Join(moved, ".git", "hooks", event.String()),
				binDir, "refs/heads/main abc refs/heads/main def\n")
			if err != nil {
				t.Fatalf("the hook must not fail git when its PATH holds only %v: %v (stderr=%q)",
					hookUtilities, err, stderr)
			}

			for _, want := range []string{
				githooks.InstallCommandWithBinding(event, moved, binding),
				githooks.UninstallCommandWithBinding(event, moved, binding),
			} {
				if !strings.Contains(stderr, want) {
					t.Errorf("the remedy must name the repository git resolves now, quoted exactly as %q; got:\n%s", want, stderr)
				}
			}
			// The failure mode being guarded is not a wrong path but an EMPTY
			// one: whatever goes wrong in the quoting, a command with no
			// repository argument must be impossible to print.
			if strings.Contains(stderr, "--dir ''") {
				t.Errorf("a remedy with an empty --dir cannot work and must be unprintable; got:\n%s", stderr)
			}
			// The shell says "not found" for a command it could not resolve. On
			// this PATH nothing but the permitted utilities can resolve, so any
			// such line is the generated script reaching outside what it is
			// allowed to need.
			if strings.Contains(stderr, "not found") {
				t.Errorf("the generated hook reached for a program outside %v; its PATH holds only those, so the shell could not find it:\n%s",
					hookUtilities, stderr)
			}
		})
	}
}

// warningField returns one labelled line of the hook's warning block, so an
// assertion about the retry command cannot be satisfied by the flags appearing
// somewhere else in the output.
func warningField(t *testing.T, stderr, field string) string {
	t.Helper()
	for _, line := range strings.Split(stderr, "\n") {
		label, value, found := strings.Cut(line, ":")
		if found && strings.TrimSpace(label) == field {
			return value
		}
	}
	t.Fatalf("the warning carries no %q line; got:\n%s", field, stderr)
	return ""
}

// TestScript_EmbeddedFactsRoundTrip proves the two facts status reads back out
// of an installed hook are exactly the ones the template wrote. They are read by
// prefix, so a template edit that moved or renamed either line would otherwise
// make status silently report nothing instead of a moved repository.
func TestScript_EmbeddedFactsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, event := range githooks.AllEvents {
		root := "/tmp/repo with 'quote'"
		binding := githooks.Binding{DataDir: "/tmp/data dir"}
		script, err := githooks.Script(event, root, root+"/.git/hooks/"+event.String(), binding)
		if err != nil {
			t.Fatalf("render %s script: %v", event, err)
		}
		if got := githooks.EmbeddedRepository([]byte(script)); got != root {
			t.Errorf("%s embedded repository = %q, want %q", event, got, root)
		}
		if got, want := githooks.EmbeddedCommand([]byte(script)), githooks.RepositoryCommand(root, binding); got != want {
			t.Errorf("%s embedded command = %q, want %q", event, got, want)
		}
	}
	if got := githooks.EmbeddedRepository([]byte("#!/bin/sh\nmake lint\n")); got != "" {
		t.Errorf("a hook Peasant did not write must yield no embedded repository, got %q", got)
	}
	if got := githooks.EmbeddedCommand([]byte("#!/bin/sh\nmake lint\n")); got != "" {
		t.Errorf("a hook Peasant did not write must yield no embedded command, got %q", got)
	}
}

// --- helpers ----------------------------------------------------------------

// stubPeasant writes an executable `peasant` that records the arguments it was
// given and exits with exitCode. It stands in for the real binary so the hook
// itself, not the upload, is what the test exercises.
//
// The stub is then exercised ONCE from the parent, retrying the busy-file
// condition, before the log is created. That is where the race actually lands:
// a sibling test forking between this package's write and close of the stub
// leaves the child holding a write descriptor to it, and the kernel then
// refuses to exec it. The hook cannot ride that out - it always exits 0 by
// design, so a stub that would not start is reported as an ordinary failed
// upload and surfaces only as an empty invocation log. The parent can, so it
// does it here. The warm-up runs before the log file is created, so its own
// arguments are never recorded.
func stubPeasant(t *testing.T, exitCode int) (binDir, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(binDir, "invocations.log")
	stubPath := filepath.Join(binDir, "peasant")
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + shellSingleQuote(logPath) + "\nexit " +
		strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(stubPath, []byte(stub), 0o755); err != nil {
		t.Fatalf("write peasant stub: %v", err)
	}
	awaitExecutable(t, stubPath)
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create stub log: %v", err)
	}
	return binDir, logPath
}

// awaitExecutable runs path until the kernel stops reporting it as busy, so the
// caller hands the hook a binary that is known to start.
func awaitExecutable(t *testing.T, path string) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		err := exec.Command(path).Run()
		var exitErr *exec.ExitError
		if err == nil || errors.As(err, &exitErr) {
			// It started. A non-zero status is the stub doing its job.
			return
		}
		if !errors.Is(err, syscall.ETXTBSY) || attempt >= hookExecAttempts {
			t.Fatalf("the peasant stub at %s could not be started after %d attempt(s): %v", path, attempt, err)
		}
		time.Sleep(time.Duration(attempt) * 5 * time.Millisecond)
	}
}

func recordedArgs(t *testing.T, path string) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(readFile(t, path), "\n"), "\n")
}

// hookExecAttempts bounds the ETXTBSY retry in runHook. Eight attempts with the
// backoff below span roughly 180ms, which is far longer than the window a
// concurrently-forking test can hold a just-written file open, while still
// failing fast if something is genuinely wrong.
const hookExecAttempts = 8

// runHook executes the hook file exactly as git would: the executable itself,
// with the repository as the working directory. PATH is supplied per invocation
// so the test never mutates process-global state.
//
// The exec is retried on ETXTBSY. These tests write a hook and then immediately
// execute it while sibling tests run in parallel; if one of those forks between
// this package's create-write-close of the hook file, the child inherits a
// write-open descriptor to it and the kernel refuses the exec with "text file
// busy". That is a property of the test harness, not of the hook, but it lands
// as an intermittent failure in `make check` on many-core machines and reads
// exactly like a real regression. Stdin is taken by value so each attempt gets a
// fresh reader.
func runHook(t *testing.T, repo, hook, binDir, stdin string) (string, string, error) {
	t.Helper()
	for attempt := 1; ; attempt++ {
		cmd := exec.Command(hook)
		cmd.Dir = repo
		cmd.Env = []string{
			"PATH=" + hookPATH(t, binDir),
			envHome + "=" + t.TempDir(),
		}
		cmd.Stdin = strings.NewReader(stdin)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if errors.Is(err, syscall.ETXTBSY) && attempt < hookExecAttempts {
			time.Sleep(time.Duration(attempt) * 5 * time.Millisecond)
			continue
		}
		return stdout.String(), stderr.String(), err
	}
}

// hookUtilities is every external command a generated hook is PERMITTED to run.
// It is a closed list of one, and that is the property under test: a generated
// hook needs the shell its first line names, git, and peasant, and nothing else.
//
// git is on it because a hook asks git where the worktree is when the repository
// it was installed for has moved, and git runs its hooks with its own exec path
// on PATH. Leaving git out would exercise only the fallback branch and silently
// stop covering the answer a user actually gets.
var hookUtilities = [...]string{"git"}

// hookPATH builds the PATH a hook is run with: the stub directory holding
// peasant, then a directory holding exactly one symlink per permitted utility.
//
// It deliberately names no system directory. It used to carry /usr/bin and /bin,
// which quietly made the suite untrustworthy in the one direction that matters:
// a generated hook that reaches for a utility it should not need is caught only
// where that utility is absent. On a machine where git and, say, sed share
// /usr/bin - Debian, Ubuntu, macOS - a PATH derived from either would supply
// BOTH, so the same regression passes for most contributors and fails only in a
// hermetic sandbox. Building the directory from the permitted list instead makes
// the absence of everything else identical on every machine.
func hookPATH(t *testing.T, binDir string) string {
	t.Helper()
	utilities := t.TempDir()
	for _, utility := range hookUtilities {
		resolved, err := exec.LookPath(utility)
		if err != nil {
			t.Fatalf("%s must be available to run a hook the way git does: %v", utility, err)
		}
		if err := os.Symlink(resolved, filepath.Join(utilities, utility)); err != nil {
			t.Fatalf("link the permitted utility %s into the hook PATH: %v", utility, err)
		}
	}
	return strings.Join([]string{binDir, utilities}, string(os.PathListSeparator))
}

// assertShellParses runs the shell's own syntax check over text.
func assertShellParses(t *testing.T, name, text string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	out, err := exec.Command("sh", "-n", path).CombinedOutput()
	if err != nil {
		t.Errorf("%s is not valid POSIX shell: %v\n%s\n--- text ---\n%s", name, err, out, text)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func isASCII(text string) bool {
	for _, r := range text {
		if r > 0x7f {
			return false
		}
	}
	return true
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
