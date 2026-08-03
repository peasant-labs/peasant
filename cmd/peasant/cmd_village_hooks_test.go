package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/village_hooks.yaml
var villageHooksFixtureData []byte

//go:embed testdata/village_hooks_unknown_field.yaml
var villageHooksUnknownFieldFixtureData []byte

//go:embed testdata/village_hooks_second_document.yaml
var villageHooksSecondDocumentFixtureData []byte

//go:embed testdata/village_hooks_count_mismatch.yaml
var villageHooksCountMismatchFixtureData []byte

//go:embed testdata/village_hooks_mismatched_operation.yaml
var villageHooksMismatchedOperationFixtureData []byte

//go:embed testdata/village_hooks_warning_without_failure.yaml
var villageHooksWarningWithoutFailureFixtureData []byte

const villageHooksFixturePath = "cmd/peasant/testdata/village_hooks.yaml"

// --- fixture schema ---------------------------------------------------------

// gitOperation is the git command that must fire a given hook.
type gitOperation string

const (
	gitOperationCommit gitOperation = "commit"
	gitOperationPush   gitOperation = "push"
)

// operationForEvent pins which git command each managed event belongs to, so a
// fixture cannot claim a post-commit hook is exercised by a push.
var operationForEvent = map[githooks.Event]gitOperation{
	githooks.EventPostCommit: gitOperationCommit,
	githooks.EventPrePush:    gitOperationPush,
}

type villageHooksFixtureDocument struct {
	ExpectedCaseCount int                       `yaml:"expectedCaseCount"`
	Cases             []villageHooksFixtureCase `yaml:"cases"`
}

type villageHooksFixtureCase struct {
	Name           string         `yaml:"name"`
	Event          githooks.Event `yaml:"event"`
	GitOperation   gitOperation   `yaml:"gitOperation"`
	UploadExitCode int            `yaml:"uploadExitCode"`
	ExpectWarning  bool           `yaml:"expectWarning"`
}

// loadVillageHooksFixture decodes and validates the git-execution corpus.
func loadVillageHooksFixture(data []byte) (villageHooksFixtureDocument, error) {
	var document villageHooksFixtureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf(
			"village hooks fixture rule failed: typed YAML fields must match the document schema; unknown or malformed data "+
				"invalidates the mounted git-execution evidence; where=%s loader=first-document decode; when=test fixture loading; "+
				"impact=hook execution coverage cannot be trusted; fix=remove unknown fields and provide expectedCaseCount plus typed cases: %w",
			villageHooksFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, fmt.Errorf(
			"village hooks fixture rule failed: exactly one YAML document is allowed; trailing data is silently ignored and "+
				"invalidates the mounted git-execution evidence; where=%s loader=end-of-document check; when=test fixture loading; "+
				"impact=hook execution coverage cannot be trusted; fix=remove the second document so the next decode returns EOF: %w",
			villageHooksFixturePath, err)
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, fmt.Errorf(
			"village hooks fixture rule failed: declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d; "+
				"a silently shrinking corpus invalidates the mounted git-execution evidence; where=%s loader=case-count validation; "+
				"when=test fixture loading; impact=hook execution coverage cannot be trusted; fix=set expectedCaseCount to the number of cases present",
			document.ExpectedCaseCount, len(document.Cases), villageHooksFixturePath)
	}
	seen := make(map[string]bool, len(document.Cases))
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, villageHooksRuleError(index, fmt.Sprintf("case name %q is missing or duplicated", testCase.Name),
				"fix=give every case a unique, behavior-naming name")
		}
		seen[testCase.Name] = true
		if err := testCase.Event.Validate(); err != nil {
			return document, villageHooksRuleError(index, fmt.Sprintf("unsupported event %q", testCase.Event), "fix=use a managed event")
		}
		if want := operationForEvent[testCase.Event]; testCase.GitOperation != want {
			return document, villageHooksRuleError(index,
				fmt.Sprintf("gitOperation %q cannot fire the %q hook", testCase.GitOperation, testCase.Event),
				fmt.Sprintf("fix=use gitOperation %q for %q", want, testCase.Event))
		}
		if testCase.UploadExitCode < 0 || testCase.UploadExitCode > 125 {
			return document, villageHooksRuleError(index, fmt.Sprintf("uploadExitCode %d is not a usable exit status", testCase.UploadExitCode),
				"fix=use a status between 0 and 125")
		}
		if testCase.ExpectWarning != (testCase.UploadExitCode != 0) {
			return document, villageHooksRuleError(index,
				fmt.Sprintf("expectWarning=%v contradicts uploadExitCode=%d", testCase.ExpectWarning, testCase.UploadExitCode),
				"fix=expect a warning exactly when the upload fails")
		}
	}
	return document, nil
}

func villageHooksRuleError(index int, what, fix string) error {
	return fmt.Errorf(
		"village hooks fixture rule failed: %s; a malformed case invalidates the mounted git-execution evidence; "+
			"where=%s case index %d; when=test fixture loading; impact=hook execution coverage cannot be trusted; %s",
		what, villageHooksFixturePath, index, fix)
}

// --- loader guards ----------------------------------------------------------

func TestLoadVillageHooksFixture_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := loadVillageHooksFixture(villageHooksUnknownFieldFixtureData)
	if err == nil || !strings.Contains(err.Error(), "field unexpectedEvidence not found") {
		t.Fatalf("error = %v, want unknown-field rejection", err)
	}
}

func TestLoadVillageHooksFixture_RejectsSecondDocument(t *testing.T) {
	t.Parallel()
	_, err := loadVillageHooksFixture(villageHooksSecondDocumentFixtureData)
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("error = %v, want second-document rejection", err)
	}
}

func TestLoadVillageHooksFixture_RejectsCountMismatch(t *testing.T) {
	t.Parallel()
	_, err := loadVillageHooksFixture(villageHooksCountMismatchFixtureData)
	if err == nil || !strings.Contains(err.Error(), "expectedCaseCount=9 cases=1") {
		t.Fatalf("error = %v, want case-count rejection", err)
	}
}

func TestLoadVillageHooksFixture_RejectsMismatchedOperation(t *testing.T) {
	t.Parallel()
	_, err := loadVillageHooksFixture(villageHooksMismatchedOperationFixtureData)
	if err == nil || !strings.Contains(err.Error(), "cannot fire the") {
		t.Fatalf("error = %v, want event/operation mismatch rejection", err)
	}
}

func TestLoadVillageHooksFixture_RejectsWarningWithoutFailure(t *testing.T) {
	t.Parallel()
	_, err := loadVillageHooksFixture(villageHooksWarningWithoutFailureFixtureData)
	if err == nil || !strings.Contains(err.Error(), "contradicts uploadExitCode") {
		t.Fatalf("error = %v, want warning/exit-code contradiction rejection", err)
	}
}

// --- mounted lifecycle ------------------------------------------------------

// TestVillageHooks_InstallStatusUninstallRoundTrip drives the production command
// tree through the whole lifecycle and checks both what the user is told and
// what ends up on disk.
func TestVillageHooks_InstallStatusUninstallRoundTrip(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)

	out, err := runVillageHooks(t, "status", "--dir", repo)
	if err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	}
	for _, want := range []string{"post-commit", "pre-push", "not installed", "after the commit has been created", "has not contacted the remote"} {
		if !strings.Contains(out, want) {
			t.Errorf("status must report %q; got:\n%s", want, out)
		}
	}

	out, err = runVillageHooks(t, "install", "--dir", repo, "--event", "post-commit", "--event", "pre-push")
	if err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	for _, event := range githooks.AllEvents {
		path := hooksSlotPath(t, repo, event)
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read installed %s hook: %v", event, readErr)
		}
		if !githooks.IsManaged(content) {
			t.Errorf("%s hook is not recognizable Peasant-owned content", event)
		}
		if !strings.Contains(string(content), "peasant village push --non-interactive") {
			t.Errorf("%s hook must run the non-interactive village push", event)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if info.Mode().Perm() != githooks.ScriptMode.Perm() {
			t.Errorf("%s hook mode = %04o, want %04o", event, info.Mode().Perm(), githooks.ScriptMode.Perm())
		}
		if !strings.Contains(out, string(githooks.OutcomeCreated)) {
			t.Errorf("install output must report what it did; got:\n%s", out)
		}
	}

	out, err = runVillageHooks(t, "status", "--dir", repo)
	if err != nil {
		t.Fatalf("status after install: %v (out=%s)", err, out)
	}
	if strings.Count(out, "installed") < 2 {
		t.Errorf("status must report both events as installed; got:\n%s", out)
	}

	out, err = runVillageHooks(t, "uninstall", "--dir", repo)
	if err != nil {
		t.Fatalf("uninstall: %v (out=%s)", err, out)
	}
	for _, event := range githooks.AllEvents {
		if _, statErr := os.Lstat(hooksSlotPath(t, repo, event)); !os.IsNotExist(statErr) {
			t.Errorf("%s hook still exists after uninstall (stat err=%v)", event, statErr)
		}
	}
	if !strings.Contains(out, string(githooks.OutcomeRemoved)) {
		t.Errorf("uninstall output must report what it removed; got:\n%s", out)
	}
}

func TestVillageHooks_HelpDescribesRepositoryScopedCommand(t *testing.T) {
	t.Parallel()
	command := BuildVillageHooksCommand()
	command.SetArgs([]string{"--help"})
	var output bytes.Buffer
	command.SetOut(&output)
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	// Derived from the single shared command builder, so the help text can never
	// advertise a command that differs from the one a hook actually runs.
	want := githooks.CommandLine(githooks.Binding{}) + " --repository <resolved-repository>"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("hooks help must describe the repository-scoped command %q; got:\n%s", want, output.String())
	}
	for _, disclosure := range []string{"--quiet", "public-visibility confirmation on your behalf", "Hooks honor the configured", "owner visibility update", "no terminal local", "project identity, not a path"} {
		if !strings.Contains(output.String(), disclosure) {
			t.Errorf("hooks help must disclose %q; got:\n%s", disclosure, output.String())
		}
	}
	for _, stale := range []string{"carries no visibility", "publishes privately whatever", "future version", "visibility unavailable"} {
		if strings.Contains(output.String(), stale) {
			t.Errorf("hooks help retained stale visibility wording %q:\n%s", stale, output.String())
		}
	}
}

// TestVillageHooks_RefusesUnownedHookAndPrintsManualSection is the coexistence
// contract: an existing hook Peasant did not write is reported, left byte- and
// mode-identical, and answered with a section the user can paste in.
func TestVillageHooks_RefusesUnownedHookAndPrintsManualSection(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	path := hooksSlotPath(t, repo, githooks.EventPostCommit)
	existing := "#!/bin/sh\n# composed by the user\nmake lint || exit 1\nexit 0\n"
	if err := os.WriteFile(path, []byte(existing), 0o700); err != nil {
		t.Fatal(err)
	}
	before := hooksFingerprint(t, path)

	out, err := runVillageHooks(t, "install", "--dir", repo, "--event", "post-commit")
	if err == nil {
		t.Fatalf("installing over an unowned hook must fail; out=%s", out)
	}
	for _, want := range []string{
		string(githooks.OutcomeRefused),
		"Peasant did not write",
		"add this section to " + path + " by hand",
		"peasant village push --non-interactive",
		"|| true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal output must contain %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, githooks.ScriptMarkerBegin) {
		t.Error("the by-hand section must not carry the ownership marker: pasting it would make Peasant claim a hook it did not write")
	}
	if strings.Contains(out, "Usage:") {
		t.Errorf("a refusal is a runtime result, not a usage error; got:\n%s", out)
	}
	if after := hooksFingerprint(t, path); after != before {
		t.Errorf("the unowned hook was modified: %s became %s", before, after)
	}

	// Uninstall must leave it alone too, and must not claim to have removed it.
	out, err = runVillageHooks(t, "uninstall", "--dir", repo, "--event", "post-commit")
	if err != nil {
		t.Fatalf("uninstall must succeed when there is simply nothing of Peasant's to remove: %v (out=%s)", err, out)
	}
	if strings.Contains(out, string(githooks.OutcomeRemoved)) {
		t.Errorf("uninstall must not claim to have removed an unowned hook; got:\n%s", out)
	}
	if after := hooksFingerprint(t, path); after != before {
		t.Errorf("uninstall modified the unowned hook: %s became %s", before, after)
	}
}

// TestVillageHooks_EventsAreHandledIndependently proves a refusal on one event
// still leaves an accurate, acted-upon result for the other.
func TestVillageHooks_EventsAreHandledIndependently(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	blocked := hooksSlotPath(t, repo, githooks.EventPrePush)
	if err := os.WriteFile(blocked, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runVillageHooks(t, "install", "--dir", repo, "--event", "post-commit", "--event", "pre-push")
	if err == nil {
		t.Fatalf("a refused event must surface as a failure; out=%s", out)
	}
	if !strings.Contains(err.Error(), "pre-push") || strings.Contains(err.Error(), "post-commit") {
		t.Errorf("the summary must name only the blocked event; got: %v", err)
	}
	installed, readErr := os.ReadFile(hooksSlotPath(t, repo, githooks.EventPostCommit))
	if readErr != nil {
		t.Fatalf("the available event must still have been installed: %v", readErr)
	}
	if !githooks.IsManaged(installed) {
		t.Error("the available event must hold a Peasant-generated hook")
	}
}

// TestVillageHooks_PublicVisibilityNoticeNamesTheInstalledEvents proves the
// consent notice is per-event.
//
// It used to be computed across the whole report and then rendered against a
// single event picked from it — always post-commit, the first in report order,
// and therefore the one most likely to have been REFUSED. The result was a
// notice that warned about a hook which does not exist, said nothing about the
// hook that will actually publish publicly, and printed an uninstall command
// that removes nothing.
func TestVillageHooks_PublicVisibilityNoticeNamesTheInstalledEvents(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	// post-commit is occupied by a foreign hook, so only pre-push is installed.
	if err := os.WriteFile(hooksSlotPath(t, repo, githooks.EventPostCommit),
		[]byte("#!/bin/sh\necho another tool\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeCfg(t, t.TempDir(), "public.yaml", "version: 1\npush:\n  visibility: public\n")

	out, err := runVillageHooksWithRootFlags(t, []string{"--config", cfgPath},
		"install", "--dir", repo, "--event", "post-commit", "--event", "pre-push")
	if err == nil {
		t.Fatalf("the refused event must still surface as a failure; out=%s", out)
	}

	notices := []string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "notice: push.visibility") {
			notices = append(notices, line)
		}
	}
	if len(notices) != 0 {
		t.Fatalf("the retired visibility downgrade notice must not be printed; got %d:\n%s", len(notices), out)
	}
	// The uninstall command it hands out has to be the one that works, which
	// means the one carrying the path overrides this install ran with.
	uninstallOut, uninstallErr := runVillageHooks(t, "uninstall", "--event", githooks.EventPrePush.String(), "--dir", repo)
	if uninstallErr != nil {
		t.Fatalf("the uninstall command the notice printed must work: %v (out=%s)", uninstallErr, uninstallOut)
	}
	if !strings.Contains(uninstallOut, string(githooks.OutcomeRemoved)) {
		t.Errorf("the uninstall command the notice printed must actually remove the hook; got:\n%s", uninstallOut)
	}
}

// TestVillageHooks_InstallRequiresAnExplicitEvent keeps installation from ever
// happening on a defaulted choice.
func TestVillageHooks_InstallRequiresAnExplicitEvent(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	out, err := runVillageHooks(t, "install", "--dir", repo)
	if err == nil {
		t.Fatalf("install with no --event must fail; out=%s", out)
	}
	if !strings.Contains(err.Error(), "no hook event was requested") {
		t.Errorf("error must explain that an explicit event is required; got: %v", err)
	}
	for _, event := range githooks.AllEvents {
		if _, statErr := os.Lstat(hooksSlotPath(t, repo, event)); !os.IsNotExist(statErr) {
			t.Errorf("%s must not exist after a rejected install", event)
		}
	}
}

// TestVillageHooks_RejectsUnsupportedEvent keeps the event set closed at the CLI
// boundary.
func TestVillageHooks_RejectsUnsupportedEvent(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	out, err := runVillageHooks(t, "install", "--dir", repo, "--event", "pre-commit")
	if err == nil {
		t.Fatalf("an unmanaged event must be rejected; out=%s", out)
	}
	if !strings.Contains(err.Error(), "unsupported hook event") || !strings.Contains(err.Error(), "post-commit, pre-push") {
		t.Errorf("error must name the supported events; got: %v", err)
	}
}

// TestVillageHooks_BindsExplicitDirectoryOverrides proves an install that used
// explicit paths pins exactly those paths into the hook, and an install without
// them leaves Peasant's normal resolution in force.
func TestVillageHooks_BindsExplicitDirectoryOverrides(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	dataDir := t.TempDir()
	configDir := t.TempDir()

	out, err := runVillageHooksWithRootFlags(t,
		[]string{"--data-dir", dataDir, "--config-dir", configDir},
		"install", "--dir", repo, "--event", "post-commit")
	if err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	binding := githooks.Binding{DataDir: dataDir, ConfigDir: configDir}
	wantCommand := githooks.RepositoryCommand(repo, binding)
	if !strings.Contains(out, "It runs "+wantCommand+" and never blocks git.") {
		t.Errorf("install output must show the exact repository-scoped command; got:\n%s", out)
	}
	if strings.Contains(out, "It runs "+githooks.CommandLine(githooks.Binding{})+" and never blocks git.") {
		t.Errorf("install output must not show the unscoped command; got:\n%s", out)
	}
	bound := hooksReadFile(t, hooksSlotPath(t, repo, githooks.EventPostCommit))
	for _, want := range []string{"--data-dir '" + dataDir + "'", "--config-dir '" + configDir + "'"} {
		if !strings.Contains(bound, want) {
			t.Errorf("the hook must carry %q so it runs with the context it was installed with", want)
		}
	}
	if strings.Contains(bound, "--state-dir") {
		t.Error("the hook must not pin overrides the user did not set")
	}

	plain := hooksTestRepo(t)
	if out, err := runVillageHooks(t, "install", "--dir", plain, "--event", "post-commit"); err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	unbound := hooksReadFile(t, hooksSlotPath(t, plain, githooks.EventPostCommit))
	if !strings.Contains(unbound, "\n"+githooks.RepositoryCommand(plain, githooks.Binding{})+" </dev/null\n") {
		t.Errorf("an install without overrides must leave normal resolution in force; got:\n%s", unbound)
	}
}

// TestVillageHooks_GitExecutionFixtures installs through the production command
// tree and then runs the real git command, with a stub peasant on PATH. Git must
// always succeed; a failed upload must only warn.
func TestVillageHooks_GitExecutionFixtures(t *testing.T) {
	t.Parallel()
	document, err := loadVillageHooksFixture(villageHooksFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			repo := hooksTestRepo(t)
			remote := hooksTestRemote(t, repo)
			hooksGit(t, repo, "", "commit", "--quiet", "--allow-empty", "-m", "base")

			if out, installErr := runVillageHooks(t, "install", "--dir", repo, "--event", testCase.Event.String()); installErr != nil {
				t.Fatalf("install: %v (out=%s)", installErr, out)
			}
			binDir, log := hooksStubPeasant(t, testCase.UploadExitCode)

			var gitOutput string
			switch testCase.GitOperation {
			case gitOperationCommit:
				gitOutput = hooksGit(t, repo, binDir, "commit", "--allow-empty", "-m", "hooked")
				if count := hooksGit(t, repo, "", "rev-list", "--count", "HEAD"); strings.TrimSpace(count) != "2" {
					t.Errorf("the commit must exist regardless of the upload; rev-list count = %q", strings.TrimSpace(count))
				}
			case gitOperationPush:
				gitOutput = hooksGit(t, repo, binDir, "push", "origin", "main")
				if count := hooksGit(t, remote, "", "rev-list", "--count", "main"); strings.TrimSpace(count) != "1" {
					t.Errorf("the push must reach the remote regardless of the upload; remote count = %q", strings.TrimSpace(count))
				}
			}

			recorded := strings.TrimSpace(hooksReadFile(t, log))
			wantArgs := strings.Join(hooksUploadArgv(repo), "\n")
			if recorded != wantArgs {
				t.Errorf("the hook must invoke the quiet, non-interactive village push; peasant saw %q, want %q", recorded, wantArgs)
			}
			if testCase.ExpectWarning {
				for _, want := range []string{
					"village upload did not complete",
					repo,
					testCase.Event.String(),
					testCase.Event.Impact(),
					"peasant village hooks uninstall --event " + testCase.Event.String(),
				} {
					if !strings.Contains(gitOutput, want) {
						t.Errorf("the warning must name %q; got:\n%s", want, gitOutput)
					}
				}
			} else if strings.Contains(gitOutput, "village upload did not complete") {
				t.Errorf("a successful upload must not warn; got:\n%s", gitOutput)
			}
		})
	}
}

// TestVillageHooks_LeavesTheCheckoutHooksUntouched proves that running the
// mounted commands never writes into the hooks directory of the checkout the
// tests are running from.
func TestVillageHooks_LeavesTheCheckoutHooksUntouched(t *testing.T) {
	t.Parallel()
	checkoutHooks := hooksCheckoutHooksDir(t)
	before := hooksDirectoryFingerprint(t, checkoutHooks)

	repo := hooksTestRepo(t)
	if out, err := runVillageHooks(t, "install", "--dir", repo, "--event", "post-commit", "--event", "pre-push"); err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	if out, err := runVillageHooks(t, "status", "--dir", repo); err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	}
	if out, err := runVillageHooks(t, "uninstall", "--dir", repo); err != nil {
		t.Fatalf("uninstall: %v (out=%s)", err, out)
	}

	if after := hooksDirectoryFingerprint(t, checkoutHooks); after != before {
		t.Errorf("the checkout's hooks directory changed\n  before:\n%s\n  after:\n%s", before, after)
	}
}

// --- helpers ----------------------------------------------------------------

// hooksUploadArgv is the argv the stub peasant must record for a hook installed
// in repo. It lives here once so a change to the shared command builder is a
// single fixture edit rather than N inline literals. --quiet is part of it: a
// hook fires on every commit or push, and the default summary would print
// several lines into an ordinary git command. --timeout is part of it too: the
// client's own timeout is per request and a push issues several in sequence, so
// without an overall budget a village that stops answering stalls git for
// minutes. The budget is read from the constant so it cannot drift.
func hooksUploadArgv(repo string) []string {
	return []string{
		"village", "push", "--non-interactive", "--quiet",
		"--timeout", githooks.DefaultUploadBudget.String(),
		"--repository", repo,
	}
}

// runVillageHooks executes `peasant village hooks <args...>` through a root that
// mirrors main()'s, capturing everything the user would see.
func runVillageHooks(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runVillageHooksWithRootFlags(t, nil, args...)
}

// runVillageHooksWithRootFlags is runVillageHooks with persistent root flags set
// first, so tests can prove which overrides a generated hook binds.
func runVillageHooksWithRootFlags(t *testing.T, rootFlags []string, args ...string) (string, error) {
	t.Helper()
	root := newTestRoot()
	root.AddCommand(BuildVillageCommand())
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append(append(append([]string{}, rootFlags...), "village", "hooks"), args...))
	err := root.Execute()
	return buf.String(), err
}

// hooksTestRepo creates a disposable git repository under the test's own
// temporary directory.
func hooksTestRepo(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	hooksGit(t, repo, "", "init", "--quiet", "--initial-branch=main")
	hooksGit(t, repo, "", "config", "user.email", "hooks-test@example.invalid")
	hooksGit(t, repo, "", "config", "user.name", "Hooks Test")
	hooksGit(t, repo, "", "config", "commit.gpgsign", "false")
	return repo
}

// hooksTestRemote creates a bare repository next to repo and wires it as origin.
func hooksTestRemote(t *testing.T, repo string) string {
	t.Helper()
	remote := filepath.Join(filepath.Dir(repo), "origin.git")
	hooksGit(t, filepath.Dir(repo), "", "init", "--quiet", "--bare", remote)
	hooksGit(t, repo, "", "remote", "add", "origin", remote)
	return remote
}

// hooksGit runs one git command in argument-list form. When binDir is set it is
// placed first on PATH for that invocation only, so a hook git fires finds the
// stub peasant without this test ever mutating process-global state.
func hooksGit(t *testing.T, dir, binDir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if binDir != "" {
		cmd.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// hooksStubPeasant writes an executable `peasant` that records its arguments and
// exits with exitCode, standing in for a real upload.
func hooksStubPeasant(t *testing.T, exitCode int) (binDir, logPath string) {
	t.Helper()
	binDir = t.TempDir()
	logPath = filepath.Join(binDir, "invocations.log")
	quoted := "'" + strings.ReplaceAll(logPath, "'", `'\''`) + "'"
	stub := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> " + quoted + "\nexit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "peasant"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write peasant stub: %v", err)
	}
	if err := os.WriteFile(logPath, nil, 0o644); err != nil {
		t.Fatalf("create stub log: %v", err)
	}
	return binDir, logPath
}

// hooksSlotPath resolves the file git runs for event, and refuses to hand back
// anything outside the disposable repository.
func hooksSlotPath(t *testing.T, repo string, event githooks.Event) string {
	t.Helper()
	raw := strings.TrimSpace(hooksGit(t, repo, "", "rev-parse", "--git-path", "hooks/"+event.String()))
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(repo, raw)
	}
	path := filepath.Clean(raw)
	rel, err := filepath.Rel(repo, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		t.Fatalf("refusing to touch %s: it is outside the disposable repository %s", path, repo)
	}
	return path
}

// hooksCheckoutHooksDir resolves the hooks directory of the checkout the tests
// run from, so it can be proven untouched.
func hooksCheckoutHooksDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve working directory: %v", err)
	}
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--git-path", "hooks")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("tests are not running inside a git checkout, nothing to guard: %v", err)
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path)
}

func hooksFingerprint(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		return "absent"
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(content)
	return fmt.Sprintf("mode=%04o sha256=%s", info.Mode().Perm(), hex.EncodeToString(sum[:]))
}

func hooksDirectoryFingerprint(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "absent:" + err.Error()
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, entry.Name()+" "+hooksFingerprint(t, filepath.Join(dir, entry.Name())))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func hooksReadFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// TestVillageHooks_PrintedUninstallCommandsCarryTheBoundPaths holds every printed
// uninstall command to being runnable from the state that printed it.
//
// A hook installed with --config-dir or --data-dir is only removable by a command
// carrying the same overrides: one rendered without them resolves a different
// config directory, finds no hook, and removes nothing. Two surfaces print that
// command, and they are asserted separately on purpose - a fix at one must not be
// able to mask an omission at the other.
func TestVillageHooks_PrintedUninstallCommandsCarryTheBoundPaths(t *testing.T) {
	t.Parallel()
	repo := hooksTestRepo(t)
	configDir := t.TempDir()
	dataDir := t.TempDir()
	// A public visibility so the settings notice - the second of the two surfaces
	// - is printed at all.
	resolvedConfigDir := string(defaults.ResolveConfigDirPathWith(configDir))
	if err := os.MkdirAll(resolvedConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCfg(t, resolvedConfigDir, string(defaults.Config.FileName), "version: 1\npush:\n  method: all\n  visibility: public\n")
	rootFlags := []string{"--config-dir", configDir, "--data-dir", dataDir}

	if out, err := runVillageHooksWithRootFlags(t, rootFlags,
		"install", "--dir", repo, "--event", "post-commit"); err != nil {
		t.Fatalf("install: %v (out=%s)", err, out)
	}
	out, err := runVillageHooksWithRootFlags(t, rootFlags, "status", "--dir", repo)
	if err != nil {
		t.Fatalf("status: %v (out=%s)", err, out)
	}
	bound := githooks.UninstallCommandWithBinding(githooks.EventPostCommit, repo,
		githooks.Binding{ConfigDir: configDir, DataDir: dataDir})

	t.Run("the status report's uninstall line", func(t *testing.T) {
		if !strings.Contains(out, "  uninstall: "+bound) {
			t.Errorf("status must print an uninstall line that carries the bound paths (%s); got:\n%s", bound, out)
		}
	})
	t.Run("the retired settings notice is absent", func(t *testing.T) {
		if strings.Contains(out, "notice: push.visibility") {
			t.Errorf("status must not print the retired visibility downgrade notice; got:\n%s", out)
		}
	})
}
