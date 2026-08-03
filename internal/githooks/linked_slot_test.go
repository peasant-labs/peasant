package githooks_test

import (
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/githooks"
)

//go:embed testdata/linked_slot.yaml
var linkedSlotFixtureData []byte

const linkedSlotFixturePath = "internal/githooks/testdata/linked_slot.yaml"

// linkTargetShape is what a symlinked hook slot points at. It is a closed set
// because each shape decides a different answer, and one wrong answer — treating
// an unclassified target as a shell — is what destroys a working hook.
type linkTargetShape string

const (
	linkTargetShellScript      linkTargetShape = "shell-script"
	linkTargetNonShellScript   linkTargetShape = "non-shell-script"
	linkTargetGeneratedHook    linkTargetShape = "generated-hook"
	linkTargetInertGenerated   linkTargetShape = "generated-hook-not-executable"
	linkTargetHandAddedSection linkTargetShape = "hand-added-section"
	linkTargetBinary           linkTargetShape = "binary"
	linkTargetNamedPipe        linkTargetShape = "named-pipe"
	linkTargetMissing          linkTargetShape = "missing"
	linkTargetDirectory        linkTargetShape = "directory"
)

var allLinkTargetShapes = [...]linkTargetShape{
	linkTargetShellScript, linkTargetNonShellScript, linkTargetGeneratedHook,
	linkTargetInertGenerated, linkTargetHandAddedSection, linkTargetBinary,
	linkTargetNamedPipe, linkTargetMissing, linkTargetDirectory,
}

// linkTargetLocation is where the target lives relative to the directory git
// runs hooks from. It decides containment: a section pinned to one absolute
// repository must never be offered for a file outside that directory.
type linkTargetLocation string

const (
	linkTargetInsideHooksDir  linkTargetLocation = "inside-hooks-dir"
	linkTargetOutsideHooksDir linkTargetLocation = "outside-hooks-dir"
)

var allLinkTargetLocations = [...]linkTargetLocation{linkTargetInsideHooksDir, linkTargetOutsideHooksDir}

type linkedSlotDocument struct {
	ExpectedCaseCount int              `yaml:"expectedCaseCount"`
	Cases             []linkedSlotCase `yaml:"cases"`
}

type linkedSlotCase struct {
	Name                       string                `yaml:"name"`
	Target                     linkTargetShape       `yaml:"target"`
	Location                   linkTargetLocation    `yaml:"location"`
	ExpectedLanguage           githooks.HookLanguage `yaml:"expectedLanguage"`
	ExpectedCarriesUpload      bool                  `yaml:"expectedCarriesUpload"`
	ExpectedUploads            bool                  `yaml:"expectedUploads"`
	ExpectedOffersShellSection bool                  `yaml:"expectedOffersShellSection"`
	ExpectedUninstallOutcome   githooks.Outcome      `yaml:"expectedUninstallOutcome"`
}

// loadLinkedSlotFixture decodes and fully validates the corpus. Every enum-typed
// field is checked against the production closed set it names, so a case that
// expects a classification Peasant cannot produce fails at load rather than
// quietly proving nothing.
func loadLinkedSlotFixture(data []byte) (linkedSlotDocument, error) {
	document, err := decodeFixtureDocument[linkedSlotDocument](data, linkedSlotFixturePath)
	if err != nil {
		return document, err
	}
	if err := fixtureCountGuard(linkedSlotFixturePath, document.ExpectedCaseCount, len(document.Cases)); err != nil {
		return document, err
	}
	names := make([]string, 0, len(document.Cases))
	for _, testCase := range document.Cases {
		names = append(names, testCase.Name)
	}
	if err := fixtureUniqueNames(linkedSlotFixturePath, names); err != nil {
		return document, err
	}
	for index, testCase := range document.Cases {
		if !containsValue(allLinkTargetShapes[:], testCase.Target) {
			return document, fixtureCaseError(linkedSlotFixturePath, index,
				fmt.Sprintf("unsupported target %q", testCase.Target),
				"fix=use one of the modelled link targets")
		}
		if !containsValue(allLinkTargetLocations[:], testCase.Location) {
			return document, fixtureCaseError(linkedSlotFixturePath, index,
				fmt.Sprintf("unsupported location %q", testCase.Location),
				"fix=use inside-hooks-dir or outside-hooks-dir")
		}
		if !containsValue(githooks.AllHookLanguages[:], testCase.ExpectedLanguage) {
			return document, fixtureCaseError(linkedSlotFixturePath, index,
				fmt.Sprintf("unsupported expectedLanguage %q", testCase.ExpectedLanguage),
				"fix=name one of the HookLanguage classifications; the empty value is the unclassified, fail-closed one")
		}
		if !containsValue(githooks.AllOutcomes[:], testCase.ExpectedUninstallOutcome) {
			return document, fixtureCaseError(linkedSlotFixturePath, index,
				fmt.Sprintf("unsupported expectedUninstallOutcome %q", testCase.ExpectedUninstallOutcome),
				"fix=name an outcome from githooks.AllOutcomes")
		}
		if testCase.ExpectedOffersShellSection && testCase.ExpectedLanguage != githooks.HookLanguagePOSIXShell {
			return document, fixtureCaseError(linkedSlotFixturePath, index,
				fmt.Sprintf("case %q expects the shell section for a %q target", testCase.Name, testCase.ExpectedLanguage),
				"fix=the section is POSIX shell; expecting it for anything not positively classified as a shell is the destructive answer this corpus exists to prevent")
		}
		if testCase.ExpectedUploads && !testCase.ExpectedCarriesUpload {
			return document, fixtureCaseError(linkedSlotFixturePath, index,
				fmt.Sprintf("case %q expects an active upload without an upload section", testCase.Name),
				"fix=activation requires a generated or by-hand upload section")
		}
	}
	return document, nil
}

// --- loader guards ----------------------------------------------------------

func TestLoadLinkedSlotFixture_RejectsUnmodelledTarget(t *testing.T) {
	t.Parallel()
	_, err := loadLinkedSlotFixture([]byte(`expectedCaseCount: 1
cases:
  - name: unknown-target
    target: socket
    location: inside-hooks-dir
    expectedLanguage: posix-shell
    expectedUploads: false
    expectedOffersShellSection: false
    expectedUninstallOutcome: not-present
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported target") {
		t.Fatalf("error = %v, want rejection of an unmodelled link target", err)
	}
}

func TestLoadLinkedSlotFixture_RejectsSectionForANonShellTarget(t *testing.T) {
	t.Parallel()
	_, err := loadLinkedSlotFixture([]byte(`expectedCaseCount: 1
cases:
  - name: shell-section-for-python
    target: non-shell-script
    location: inside-hooks-dir
    expectedLanguage: other
    expectedUploads: false
    expectedOffersShellSection: true
    expectedUninstallOutcome: not-present
`))
	if err == nil || !strings.Contains(err.Error(), "expects the shell section") {
		t.Fatalf("error = %v, want rejection of a case that expects the destructive answer", err)
	}
}

func TestLoadLinkedSlotFixture_RejectsUnknownLanguage(t *testing.T) {
	t.Parallel()
	_, err := loadLinkedSlotFixture([]byte(`expectedCaseCount: 1
cases:
  - name: invented-classification
    target: shell-script
    location: inside-hooks-dir
    expectedLanguage: perl-ish
    expectedUploads: false
    expectedOffersShellSection: false
    expectedUninstallOutcome: not-present
`))
	if err == nil || !strings.Contains(err.Error(), "unsupported expectedLanguage") {
		t.Fatalf("error = %v, want rejection of a classification githooks cannot produce", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestLifecycle_SymlinkedSlotIsJudgedOnWhatGitRuns drives every symlinked-slot
// shape through status, install, and uninstall against a real repository.
//
// The two gates it proves are the ones a symlink used to switch off wholesale:
// no by-hand POSIX shell section is ever offered for a target git does not run
// with a shell (or that could not be read at all), and a slot that uploads is
// reported as uploading by both status and uninstall.
func TestLifecycle_SymlinkedSlotIsJudgedOnWhatGitRuns(t *testing.T) {
	t.Parallel()
	document, err := loadLinkedSlotFixture(linkedSlotFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runLinkedSlotCase(t, testCase)
		})
	}
}

func runLinkedSlotCase(t *testing.T, testCase linkedSlotCase) {
	t.Helper()
	repo := disposableRepo(t)
	slot := hookPath(t, repo, githooks.EventPrePush.String())
	target := seedLinkedSlot(t, repo, slot, testCase)

	lifecycle := githooks.New(githooks.NewExecGit())
	request := githooks.Request{Dir: repo, Events: []githooks.Event{githooks.EventPrePush}}

	before := linkedSlotFingerprint(t, slot, target)

	status, err := linkedSlotStatus(t, lifecycle, request, testCase.Target)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	plan := planForEvent(t, status, githooks.EventPrePush)

	if plan.Language != testCase.ExpectedLanguage {
		t.Errorf("language = %q, want %q — what git runs the TARGET as decides whether the shell section may be offered",
			plan.Language, testCase.ExpectedLanguage)
	}
	if plan.Ownership != githooks.OwnershipForeign {
		t.Errorf("ownership = %q, want %q — a symlinked slot is never Peasant's to rewrite or delete",
			plan.Ownership, githooks.OwnershipForeign)
	}
	if got := plan.UploadsFromForeignFile(); got != testCase.ExpectedUploads {
		t.Errorf("UploadsFromForeignFile() = %v, want %v (reason: %s)", got, testCase.ExpectedUploads, plan.Reason)
	}
	if got := plan.CarriesUploadSection(); got != testCase.ExpectedCarriesUpload {
		t.Errorf("CarriesUploadSection() = %v, want %v (reason: %s)", got, testCase.ExpectedCarriesUpload, plan.Reason)
	}
	if got := plan.Manual != ""; got != testCase.ExpectedOffersShellSection {
		t.Errorf("offered the by-hand shell section = %v, want %v (reason: %s)",
			got, testCase.ExpectedOffersShellSection, plan.Reason)
	}
	if plan.Manual != "" && !strings.Contains(plan.Manual, target) {
		t.Errorf("the offered section names %q but not the file git actually executes (%s): appending to the link appends to the target",
			plan.Path, target)
	}
	if testCase.ExpectedUploads && strings.Contains(plan.Reason, "no upload hook is active for this event") {
		t.Errorf("status reported no active upload for a slot that uploads on every event:\n%s", plan.Reason)
	}
	if testCase.ExpectedUploads && !strings.Contains(plan.Reason, target) {
		t.Errorf("status did not name the real file %s the user has to edit:\n%s", target, plan.Reason)
	}
	if testCase.ExpectedCarriesUpload && !testCase.ExpectedUploads {
		if strings.Contains(plan.Reason, "uploads are running") || !strings.Contains(plan.Reason, "no upload is active") {
			t.Errorf("an inert target was reported as actively uploading:\n%s", plan.Reason)
		}
	}
	if !testCase.ExpectedCarriesUpload && testCase.Location == linkTargetOutsideHooksDir {
		switch testCase.ExpectedLanguage {
		case githooks.HookLanguagePOSIXShell:
			if !strings.Contains(plan.Reason, "run this POSIX shell command: peasant village push") {
				t.Errorf("an outside shell target was not given a valid shell invocation:\n%s", plan.Reason)
			}
		case githooks.HookLanguageOther:
			if !strings.Contains(plan.Reason, "as an argument list with no shell quoting") || strings.Contains(plan.Reason, "--repository '") {
				t.Errorf("an outside non-shell target was not given raw interpreter argv:\n%s", plan.Reason)
			}
		case githooks.HookLanguageBinary:
			if strings.Contains(plan.Reason, "as an argument list") {
				t.Errorf("a binary target was offered interpreter argv it cannot execute:\n%s", plan.Reason)
			}
		}
	}

	// Install must refuse and change nothing.
	installed, err := lifecycle.Install(t.Context(), request)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if outcome := installed.Results[0].Outcome; outcome != githooks.OutcomeRefused {
		t.Errorf("install outcome = %q, want %q — a symlinked slot is a file Peasant did not write", outcome, githooks.OutcomeRefused)
	}
	if got := installed.Results[0].Manual != ""; got != testCase.ExpectedOffersShellSection {
		t.Errorf("install offered the by-hand shell section = %v, want %v (reason: %s)",
			got, testCase.ExpectedOffersShellSection, installed.Results[0].Reason)
	}

	// Uninstall must reach the modelled verdict and change nothing.
	removed, err := lifecycle.Uninstall(t.Context(), request)
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	result := removed.Results[0]
	if result.Outcome != testCase.ExpectedUninstallOutcome {
		t.Errorf("uninstall outcome = %q, want %q (reason: %s)",
			result.Outcome, testCase.ExpectedUninstallOutcome, result.Reason)
	}
	if testCase.ExpectedUploads {
		if strings.Contains(result.Reason, "no upload runs from it") {
			t.Errorf("uninstall told a user who wants uploads to stop that none run:\n%s", result.Reason)
		}
		if !strings.Contains(result.Reason, target) {
			t.Errorf("uninstall did not name the real file %s the user has to edit:\n%s", target, result.Reason)
		}
	}
	if testCase.ExpectedCarriesUpload && !testCase.ExpectedUploads {
		if strings.Contains(result.Reason, "it still uploads on every") || !strings.Contains(result.Reason, "does not currently run") {
			t.Errorf("uninstall reported an inert upload section as active:\n%s", result.Reason)
		}
	}

	if after := linkedSlotFingerprint(t, slot, target); after != before {
		t.Errorf("the link or its target changed:\nbefore: %s\nafter:  %s", before, after)
	}
}

// linkedSlotStatus bounds the named-pipe case. Without O_NONBLOCK, opening a
// FIFO for inspection waits forever for a writer before Peasant can discover
// that the target is not a regular file; a regression must fail this test rather
// than hanging the package until the outer test timeout.
func linkedSlotStatus(
	t *testing.T,
	lifecycle *githooks.Lifecycle,
	request githooks.Request,
	shape linkTargetShape,
) (githooks.PlanReport, error) {
	t.Helper()
	if shape != linkTargetNamedPipe {
		return lifecycle.Status(t.Context(), request)
	}
	type outcome struct {
		report githooks.PlanReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		report, err := lifecycle.Status(t.Context(), request)
		done <- outcome{report: report, err: err}
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.report, result.err
	case <-timer.C:
		t.Fatal("status blocked while opening a symlinked FIFO; inspection must use a non-blocking descriptor before rejecting non-regular targets")
		return githooks.PlanReport{}, nil
	}
}

// planForEvent picks one event's plan out of a report.
func planForEvent(t *testing.T, report githooks.PlanReport, event githooks.Event) githooks.Plan {
	t.Helper()
	for _, plan := range report.Plans {
		if plan.Event == event {
			return plan
		}
	}
	t.Fatalf("no plan for %s in the report", event)
	return githooks.Plan{}
}

// seedLinkedSlot builds the target and links the hook slot at it, returning the
// resolved target path. A missing target is deliberately never created, so the
// link dangles exactly as a stale dotfiles link does.
func seedLinkedSlot(t *testing.T, repo, slot string, testCase linkedSlotCase) string {
	t.Helper()
	var target string
	switch testCase.Location {
	case linkTargetInsideHooksDir:
		target = filepath.Join(filepath.Dir(slot), "pre-push.real")
	default:
		scripts := filepath.Join(repo, "scripts")
		if err := os.MkdirAll(scripts, 0o755); err != nil {
			t.Fatalf("create scripts directory: %v", err)
		}
		target = filepath.Join(scripts, "pre-push.real")
	}

	switch testCase.Target {
	case linkTargetMissing:
		// Nothing is written: the link dangles.
	case linkTargetDirectory:
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatalf("create the directory target: %v", err)
		}
	case linkTargetNamedPipe:
		if runtime.GOOS == "windows" {
			t.Skip("named pipes do not use POSIX filesystem FIFO semantics on Windows")
		}
		command := exec.Command("mkfifo", target)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("create named-pipe target with mkfifo: %v: %s", err, output)
		}
	case linkTargetInertGenerated:
		if err := os.WriteFile(target, []byte(linkedSlotTargetContent(t, testCase.Target, repo, slot)), 0o644); err != nil {
			t.Fatalf("write non-executable target %s: %v", target, err)
		}
	default:
		writeExecutable(t, target, linkedSlotTargetContent(t, testCase.Target, repo, slot))
	}
	if err := os.Symlink(target, slot); err != nil {
		t.Fatalf("link the hook slot at %s: %v", target, err)
	}
	return target
}

// linkedSlotTargetContent renders the bytes of each modelled target. The
// generated hook and the hand-added section come from the production builders,
// so a change to either marker is caught here rather than by a stale copy.
func linkedSlotTargetContent(t *testing.T, shape linkTargetShape, repo, slot string) string {
	t.Helper()
	switch shape {
	case linkTargetShellScript:
		return "#!/bin/sh\n# a hook the user wrote\nexit 0\n"
	case linkTargetNonShellScript:
		return "#!/usr/bin/env python3\nimport sys\nprint('policy gate')\nsys.exit(0)\n"
	case linkTargetBinary:
		return "\x7fELF\x00\x00\x00\x00 not a text script\n"
	case linkTargetGeneratedHook, linkTargetInertGenerated:
		script, err := githooks.Script(githooks.EventPrePush, repo, slot, githooks.Binding{})
		if err != nil {
			t.Fatalf("render the generated hook: %v", err)
		}
		return script
	case linkTargetHandAddedSection:
		section, err := githooks.ManualSnippet(githooks.EventPrePush, repo, slot, githooks.Binding{})
		if err != nil {
			t.Fatalf("render the by-hand section: %v", err)
		}
		return "#!/bin/sh\n# a hook the user wrote\n" + section
	}
	t.Fatalf("no content for link target %q", shape)
	return ""
}

// writeExecutable writes content at path with the mode git needs to run a hook.
func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// linkedSlotFingerprint captures the link itself and the bytes behind it, so a
// mutation reached THROUGH the link is caught as readily as one applied to it.
func linkedSlotFingerprint(t *testing.T, slot, target string) string {
	t.Helper()
	link, err := os.Readlink(slot)
	if err != nil {
		t.Fatalf("read the link at %s: %v", slot, err)
	}
	info, statErr := os.Lstat(target)
	if statErr != nil {
		return fmt.Sprintf("link=%s target=absent", link)
	}
	if info.IsDir() {
		return fmt.Sprintf("link=%s target=dir mode=%04o", link, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("link=%s target=non-regular mode=%s", link, info.Mode())
	}
	return fmt.Sprintf("link=%s target=%s", link, fileFingerprint(t, target))
}
