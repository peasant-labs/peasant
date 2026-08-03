package githooks_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/githooks"
	"gopkg.in/yaml.v3"
)

func TestLifecycle_RefusesSharedAndLinkedHookPaths(t *testing.T) {
	t.Parallel()
	lifecycle := githooks.New(githooks.NewExecGit())

	t.Run("shared absolute hooks path", func(t *testing.T) {
		repo := disposableRepo(t)
		shared := filepath.Join(filepath.Dir(repo), "shared-hooks")
		mustGit(t, repo, "config", "core.hooksPath", shared)
		report, err := lifecycle.Install(t.Context(), githooks.Request{Dir: repo, Events: []githooks.Event{githooks.EventPostCommit}})
		if err != nil {
			t.Fatal(err)
		}
		if report.Results[0].Outcome != githooks.OutcomeRefused {
			t.Fatalf("outcome=%s, want refused", report.Results[0].Outcome)
		}
		if _, err := os.Stat(filepath.Join(shared, githooks.EventPostCommit.String())); !os.IsNotExist(err) {
			t.Fatalf("shared path was changed: %v", err)
		}
	})

	t.Run("repository local custom hooks path", func(t *testing.T) {
		repo := disposableRepo(t)
		mustGit(t, repo, "config", "core.hooksPath", ".repo-hooks")
		report, err := lifecycle.Install(t.Context(), githooks.Request{Dir: repo, Events: []githooks.Event{githooks.EventPostCommit}})
		if err != nil {
			t.Fatal(err)
		}
		if report.Results[0].Outcome != githooks.OutcomeCreated {
			t.Fatalf("outcome=%s, want created: %s", report.Results[0].Outcome, report.Results[0].Reason)
		}
	})

	t.Run("linked worktree shared hooks directory", func(t *testing.T) {
		main := disposableRepo(t)
		mustGit(t, main, "commit", "--quiet", "--allow-empty", "-m", "base")
		linked := filepath.Join(filepath.Dir(main), "linked")
		mustGit(t, main, "worktree", "add", "--quiet", "-b", "linked-test", linked)
		report, err := lifecycle.Install(t.Context(), githooks.Request{Dir: linked, Events: []githooks.Event{githooks.EventPostCommit}})
		if err != nil {
			t.Fatal(err)
		}
		if report.Results[0].Outcome != githooks.OutcomeRefused {
			t.Fatalf("outcome=%s, want refused", report.Results[0].Outcome)
		}
	})
}

// TestLifecycle_StatusReportsAHookGitRefusesToRun covers a hook that is present,
// intact, owned by Peasant — and inert.
//
// Git only runs an executable hook; without the bit it skips the file and prints
// a hint. Status reported such a hook as installed and active, so a user whose
// checkout, umask, or filesystem cleared the bit was told uploads were happening
// while nothing had reached the village since. The mode was already read during
// inspection and simply never checked.
func TestLifecycle_StatusReportsAHookGitRefusesToRun(t *testing.T) {
	t.Parallel()
	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	installed, err := lifecycle.Install(t.Context(), githooks.Request{
		Dir: repo, Events: []githooks.Event{githooks.EventPostCommit},
	})
	if err != nil {
		t.Fatal(err)
	}
	if installed.Results[0].Outcome != githooks.OutcomeCreated {
		t.Fatalf("install outcome = %s: %s", installed.Results[0].Outcome, installed.Results[0].Reason)
	}

	healthy, err := lifecycle.Status(t.Context(), githooks.Request{Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	if warning, ok := findWarning(findStatus(t, healthy, githooks.EventPostCommit).Warnings, githooks.WarningHookNotExecutable); ok {
		t.Fatalf("an executable hook must not be reported as one git skips: %s", warning.Detail)
	}

	hook := hookPath(t, repo, githooks.EventPostCommit.String())
	if err := os.Chmod(hook, 0o644); err != nil {
		t.Fatalf("clear the executable bit: %v", err)
	}
	report, err := lifecycle.Status(t.Context(), githooks.Request{Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	plan := findStatus(t, report, githooks.EventPostCommit)
	warning, ok := findWarning(plan.Warnings, githooks.WarningHookNotExecutable)
	if !ok {
		t.Fatalf("status must report a hook git will skip; warnings: %v\nreason: %s", plan.Warnings, plan.Reason)
	}
	for _, want := range []string{hook, "0644", "chmod +x", "not set as executable"} {
		if !strings.Contains(warning.Detail, want) {
			t.Errorf("the warning must state %q so the user can repair it; got: %s", want, warning.Detail)
		}
	}
}

// findWarning returns the first warning of kind.
func findWarning(warnings []githooks.Warning, kind githooks.WarningKind) (githooks.Warning, bool) {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return warning, true
		}
	}
	return githooks.Warning{}, false
}

//go:embed testdata/lifecycle.yaml
var lifecycleFixtureData []byte

//go:embed testdata/lifecycle_unknown_field.yaml
var lifecycleUnknownFieldFixtureData []byte

//go:embed testdata/lifecycle_second_document.yaml
var lifecycleSecondDocumentFixtureData []byte

//go:embed testdata/lifecycle_count_mismatch.yaml
var lifecycleCountMismatchFixtureData []byte

//go:embed testdata/lifecycle_duplicate_name.yaml
var lifecycleDuplicateNameFixtureData []byte

//go:embed testdata/lifecycle_unrequested_arm.yaml
var lifecycleUnrequestedArmFixtureData []byte

//go:embed testdata/lifecycle_content_without_foreign.yaml
var lifecycleContentWithoutForeignFixtureData []byte

//go:embed testdata/lifecycle_mutation_unknown_window.yaml
var lifecycleMutationUnknownWindowFixtureData []byte

//go:embed testdata/lifecycle_mutation_unknown_shape.yaml
var lifecycleMutationUnknownShapeFixtureData []byte

//go:embed testdata/lifecycle_mutation_without_content.yaml
var lifecycleMutationWithoutContentFixtureData []byte

//go:embed testdata/lifecycle_mutation_expecting_success.yaml
var lifecycleMutationExpectingSuccessFixtureData []byte

//go:embed testdata/lifecycle_mutation_without_explanation.yaml
var lifecycleMutationWithoutExplanationFixtureData []byte

//go:embed testdata/lifecycle_mutation_on_last_event.yaml
var lifecycleMutationOnLastEventFixtureData []byte

//go:embed testdata/lifecycle_mutation_twice_in_one_case.yaml
var lifecycleMutationTwiceFixtureData []byte

const lifecycleFixturePath = "internal/githooks/testdata/lifecycle.yaml"

// --- fixture schema ---------------------------------------------------------

// seedKind is what already occupies a hook slot before a case runs.
type seedKind string

const (
	seedAbsent    seedKind = "absent"
	seedManaged   seedKind = "managed"
	seedForeign   seedKind = "foreign"
	seedHandAdded seedKind = "hand-added"
	// seedEditedManaged is a hook a real install wrote, with a line appended
	// afterwards. Appending to an existing hook is ordinary tooling behavior, and
	// it breaks the framing ownership depends on: the file is foreign from then
	// on, while the upload command it was generated with is still in it and still
	// runs on every event.
	seedEditedManaged seedKind = "edited-managed"
)

var allSeedKinds = [...]seedKind{seedAbsent, seedManaged, seedForeign, seedHandAdded, seedEditedManaged}

type lifecycleFixtureDocument struct {
	ExpectedCaseCount int                    `yaml:"expectedCaseCount"`
	Cases             []lifecycleFixtureCase `yaml:"cases"`
}

type lifecycleFixtureCase struct {
	Name            string                 `yaml:"name"`
	RequestedEvents []githooks.Event       `yaml:"requestedEvents"`
	Seed            []lifecycleFixtureSeed `yaml:"seed"`
	Expected        []lifecycleFixtureArm  `yaml:"expected"`
}

type lifecycleFixtureSeed struct {
	Event   githooks.Event `yaml:"event"`
	Kind    seedKind       `yaml:"kind"`
	Content string         `yaml:"content"`
	Mode    string         `yaml:"mode"`
}

type lifecycleFixtureArm struct {
	Event                 githooks.Event     `yaml:"event"`
	OwnershipBefore       githooks.Ownership `yaml:"ownershipBefore"`
	PlannedAction         githooks.Action    `yaml:"plannedAction"`
	InstallOutcome        githooks.Outcome   `yaml:"installOutcome"`
	ManagedAfterInstall   bool               `yaml:"managedAfterInstall"`
	UninstallOutcome      githooks.Outcome   `yaml:"uninstallOutcome"`
	PresentAfterUninstall bool               `yaml:"presentAfterUninstall"`
	// OffersManualSection states whether the refusal hands back the by-hand
	// upload section. It is not always yes: a slot whose broken generated block
	// is ALREADY uploading needs the block removed, not a second upload pasted
	// underneath it, and a hook git runs with another interpreter cannot take a
	// shell section at all.
	OffersManualSection bool `yaml:"offersManualSection"`
	// ReasonMustContain and ReasonMustNotContain are asserted against what
	// install actually TELLS the user about this slot. The outcome alone does
	// not decide whether the advice is usable: a refusal can be correct and
	// still hand back a remedy that destroys a working hook or loops back to
	// the command that just refused.
	ReasonMustContain    []string `yaml:"reasonMustContain"`
	ReasonMustNotContain []string `yaml:"reasonMustNotContain"`
	// UninstallReasonMustContain is the same assertion for what UNINSTALL tells
	// the user. It is separate because the two stages refuse for different
	// reasons and name different remedies, and a row proving a refusal at one
	// stage must not be satisfiable by the other stage's text.
	UninstallReasonMustContain []string `yaml:"uninstallReasonMustContain"`
	// PostPlanMutation changes this slot AFTER Peasant decided what to do with
	// it and BEFORE it acts, which is the only window the no-clobber guards
	// exist to cover. Absent on almost every arm; see lifecycleFixtureMutation.
	PostPlanMutation *lifecycleFixtureMutation `yaml:"postPlanMutation"`
}

// mutationWindow names which call's decide-then-act window the mutation lands
// in. The two are separate guarantees: one protects a file from being written
// over, the other protects it from being deleted.
type mutationWindow string

const (
	mutateBeforeWrite  mutationWindow = "before-write"
	mutateBeforeRemove mutationWindow = "before-remove"
)

var allMutationWindows = [...]mutationWindow{mutateBeforeWrite, mutateBeforeRemove}

// mutationShape is what the slot BECOMES in that window. Each shape defeats a
// different guard, which is why it is a closed set rather than free-form bytes.
type mutationShape string

const (
	// mutateToForeignBytes is the ordinary case: a file the user wrote appears,
	// or replaces the hook, in the instant between the decision and the act.
	mutateToForeignBytes mutationShape = "foreign-bytes"
	// mutateToSymlinkedPipe is the shape that HANGS an unguarded read. Opening a
	// FIFO read-only waits forever for a writer, so a re-check that reads before
	// it proves the object is an ordinary file turns a refusal into a stuck git
	// command.
	mutateToSymlinkedPipe mutationShape = "symlink-to-named-pipe"
	// mutateToSymlinkedGeneratedHook is the shape that defeats an unguarded
	// ownership test WITHOUT looking suspicious. The link points at a
	// byte-perfect generated hook, so a re-check that follows the link sees
	// content it recognises as Peasant's own and proceeds - acting on the user's
	// link, which Peasant does not own and must never touch.
	mutateToSymlinkedGeneratedHook mutationShape = "symlink-to-generated-hook"
)

var allMutationShapes = [...]mutationShape{
	mutateToForeignBytes, mutateToSymlinkedPipe, mutateToSymlinkedGeneratedHook,
}

// lifecycleFixtureMutation is a change applied to a hook slot inside a single
// lifecycle call, after that call has planned and before it acts.
//
// It cannot be expressed as a seed. A seed is applied before anything runs, and
// Install and Uninstall each RE-PLAN internally, so a slot changed before the
// call is simply re-inspected: it is reclassified foreign and refused at plan
// time, and the write and delete guards are never reached. That is exactly why
// they were untested. Reaching them needs the change to land while the call is
// already in flight - see postPlanSabotage.
type lifecycleFixtureMutation struct {
	Window mutationWindow `yaml:"window"`
	Shape  mutationShape  `yaml:"shape"`
	// Content and Mode are the exact bytes and mode that must survive, and
	// belong only to the foreign-bytes shape. The symlink shapes build their own
	// target, because what has to survive there is the LINK.
	Content string `yaml:"content"`
	Mode    string `yaml:"mode"`
}

// --- strict loader ----------------------------------------------------------

// loadLifecycleFixture decodes and fully validates the corpus. Every guard
// exists because a silently-accepted fixture would turn a green test into no
// evidence at all: unknown fields, a trailing document, a miscounted corpus,
// duplicate names, arms that do not line up with the requested events, or seed
// content on a kind that cannot use it.
func loadLifecycleFixture(data []byte) (lifecycleFixtureDocument, error) {
	var document lifecycleFixtureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf(
			"hook lifecycle fixture rule failed: typed YAML fields must match the document schema; "+
				"unknown or malformed data invalidates the mounted lifecycle evidence; where=%s loader=first-document decode; "+
				"when=test fixture loading; impact=install/status/uninstall coverage cannot be trusted; "+
				"fix=remove unknown fields and provide expectedCaseCount plus typed cases: %w",
			lifecycleFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, fmt.Errorf(
			"hook lifecycle fixture rule failed: exactly one YAML document is allowed; trailing data is silently ignored "+
				"and invalidates the mounted lifecycle evidence; where=%s loader=end-of-document check; when=test fixture loading; "+
				"impact=install/status/uninstall coverage cannot be trusted; fix=remove the second document so the next decode returns EOF: %w",
			lifecycleFixturePath, err)
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, fmt.Errorf(
			"hook lifecycle fixture rule failed: declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d; "+
				"a silently shrinking corpus invalidates the mounted lifecycle evidence; where=%s loader=case-count validation; "+
				"when=test fixture loading; impact=install/status/uninstall coverage cannot be trusted; "+
				"fix=set expectedCaseCount to the number of cases actually present",
			document.ExpectedCaseCount, len(document.Cases), lifecycleFixturePath)
	}
	seen := make(map[string]bool, len(document.Cases))
	for index, testCase := range document.Cases {
		if strings.TrimSpace(testCase.Name) == "" {
			return document, fixtureRuleError(index, "every case needs a name", "fix=name the case after the behavior it proves")
		}
		if seen[testCase.Name] {
			return document, fixtureRuleError(index, fmt.Sprintf("duplicate case name %q", testCase.Name),
				"fix=give every case a unique name so a failure names exactly one scenario")
		}
		seen[testCase.Name] = true
		if err := validateFixtureCase(index, testCase); err != nil {
			return document, err
		}
	}
	return document, nil
}

func validateFixtureCase(index int, testCase lifecycleFixtureCase) error {
	if len(testCase.RequestedEvents) == 0 {
		return fixtureRuleError(index, "requestedEvents is empty",
			"fix=name at least one event, mirroring the explicit choice install demands")
	}
	requested := make(map[githooks.Event]bool, len(testCase.RequestedEvents))
	for _, event := range testCase.RequestedEvents {
		if err := event.Validate(); err != nil {
			return fixtureRuleError(index, fmt.Sprintf("unsupported requested event %q", event), "fix=use a managed event")
		}
		if requested[event] {
			return fixtureRuleError(index, fmt.Sprintf("duplicate requested event %q", event), "fix=request each event once")
		}
		requested[event] = true
	}

	seeded := make(map[githooks.Event]bool, len(testCase.Seed))
	for _, seed := range testCase.Seed {
		if err := seed.Event.Validate(); err != nil {
			return fixtureRuleError(index, fmt.Sprintf("unsupported seed event %q", seed.Event), "fix=use a managed event")
		}
		if seeded[seed.Event] {
			return fixtureRuleError(index, fmt.Sprintf("duplicate seed for %q", seed.Event), "fix=seed each event exactly once")
		}
		seeded[seed.Event] = true
		if !containsValue(allSeedKinds[:], seed.Kind) {
			return fixtureRuleError(index, fmt.Sprintf("unsupported seed kind %q", seed.Kind),
				"fix=use one of absent, managed, foreign, hand-added")
		}
		hasContent := seed.Content != "" || seed.Mode != ""
		if seed.Kind == seedForeign && !hasContent {
			return fixtureRuleError(index, fmt.Sprintf("foreign seed for %q needs content and mode", seed.Event),
				"fix=state the exact bytes and mode the test must leave untouched")
		}
		if seed.Kind != seedForeign && hasContent {
			return fixtureRuleError(index, fmt.Sprintf("seed kind %q for %q cannot carry content or mode", seed.Kind, seed.Event),
				"fix=drop content/mode, or change the kind to foreign")
		}
		if seed.Kind == seedForeign {
			if _, err := parseFixtureMode(seed.Mode); err != nil {
				return fixtureRuleError(index, fmt.Sprintf("mode %q for %q is not octal", seed.Mode, seed.Event),
					"fix=write the mode as an octal string such as \"0755\"")
			}
		}
	}
	for _, event := range githooks.AllEvents {
		if !seeded[event] {
			return fixtureRuleError(index, fmt.Sprintf("no seed for %q", event),
				"fix=seed every managed event so unrequested slots can be proven untouched")
		}
	}

	if len(testCase.Expected) != len(testCase.RequestedEvents) {
		return fixtureRuleError(index, fmt.Sprintf("expected has %d arms for %d requested events", len(testCase.Expected), len(testCase.RequestedEvents)),
			"fix=declare exactly one arm per requested event")
	}
	// How many mutations the case declares is a property of the whole case, so it
	// is settled before any single arm is judged: reporting "this arm has no
	// window behind it" for the second of two mutations describes a detail of a
	// case that was already invalid for a simpler reason.
	mutated := 0
	for _, arm := range testCase.Expected {
		if arm.PostPlanMutation != nil {
			mutated++
		}
	}
	if mutated > 1 {
		return fixtureRuleError(index, fmt.Sprintf("%d arms declare a post-plan mutation", mutated),
			"fix=declare at most one per case; the mutation is applied at one point in one call, so a second would have no window to land in and would silently prove nothing")
	}

	position := 0
	for _, event := range githooks.AllEvents {
		if !requested[event] {
			continue
		}
		arm := testCase.Expected[position]
		if arm.Event != event {
			return fixtureRuleError(index, fmt.Sprintf("arm %d is %q, expected %q", position, arm.Event, event),
				"fix=list arms in report order: post-commit before pre-push")
		}
		if !containsValue(githooks.AllOwnerships[:], arm.OwnershipBefore) {
			return fixtureRuleError(index, fmt.Sprintf("unsupported ownershipBefore %q", arm.OwnershipBefore), "fix=use a known ownership state")
		}
		if !containsValue(githooks.AllActions[:], arm.PlannedAction) {
			return fixtureRuleError(index, fmt.Sprintf("unsupported plannedAction %q", arm.PlannedAction), "fix=use a known action")
		}
		if !containsValue(githooks.AllOutcomes[:], arm.InstallOutcome) {
			return fixtureRuleError(index, fmt.Sprintf("unsupported installOutcome %q", arm.InstallOutcome), "fix=use a known outcome")
		}
		if !containsValue(githooks.AllOutcomes[:], arm.UninstallOutcome) {
			return fixtureRuleError(index, fmt.Sprintf("unsupported uninstallOutcome %q", arm.UninstallOutcome), "fix=use a known outcome")
		}
		if arm.OffersManualSection && arm.PlannedAction != githooks.ActionRefuse {
			return fixtureRuleError(index, fmt.Sprintf("arm %d offers the by-hand section for a %q action", position, arm.PlannedAction),
				"fix=only a refusal can hand back a by-hand section; a slot Peasant can manage gets a hook instead")
		}
		for _, forbidden := range arm.ReasonMustNotContain {
			if containsValue(arm.ReasonMustContain, forbidden) {
				return fixtureRuleError(index, fmt.Sprintf("arm %d both requires and forbids %q in the explanation", position, forbidden),
					"fix=decide which one it is; a phrase in both lists can never pass")
			}
		}
		if arm.PostPlanMutation != nil {
			if err := validateFixtureMutation(index, position, testCase, arm); err != nil {
				return err
			}
		}
		position++
	}
	return nil
}

// validateFixtureMutation rejects a post-plan mutation that could not prove what
// it claims. Every rule here corresponds to a way the case would PASS while
// testing nothing, which is the failure this whole group of cases exists to end.
func validateFixtureMutation(index, position int, testCase lifecycleFixtureCase, arm lifecycleFixtureArm) error {
	mutation := arm.PostPlanMutation
	if !containsValue(allMutationWindows[:], mutation.Window) {
		return fixtureRuleError(index, fmt.Sprintf("arm %d has unsupported mutation window %q", position, mutation.Window),
			"fix=use before-write or before-remove")
	}
	if !containsValue(allMutationShapes[:], mutation.Shape) {
		return fixtureRuleError(index, fmt.Sprintf("arm %d has unsupported mutation shape %q", position, mutation.Shape),
			"fix=use one of the modelled shapes; each defeats a different guard")
	}
	hasContent := mutation.Content != "" || mutation.Mode != ""
	if mutation.Shape == mutateToForeignBytes {
		if !hasContent {
			return fixtureRuleError(index, fmt.Sprintf("arm %d mutates to foreign bytes without naming them", position),
				"fix=state the exact content and mode that must survive; the guarantee is byte identity, not an outcome")
		}
		if _, err := parseFixtureMode(mutation.Mode); err != nil {
			return fixtureRuleError(index, fmt.Sprintf("arm %d mutation mode %q is not octal", position, mutation.Mode),
				"fix=write the mode as an octal string such as \"0755\"")
		}
	} else if hasContent {
		return fixtureRuleError(index, fmt.Sprintf("arm %d shape %q cannot carry content or mode", position, mutation.Shape),
			"fix=drop content/mode; a symlink shape builds its own target, and what must survive is the link")
	}

	// The stage that acts must report a FAILURE, and it must say why. Without
	// the explanation assertion the row would pass on any failure at all,
	// including one from an unrelated cause, and stop being evidence that the
	// no-clobber guard is what refused.
	switch mutation.Window {
	case mutateBeforeWrite:
		if arm.InstallOutcome != githooks.OutcomeFailed {
			return fixtureRuleError(index, fmt.Sprintf("arm %d mutates before the write but expects installOutcome %q", position, arm.InstallOutcome),
				"fix=expect failed; a slot that changed after the plan must stop the write")
		}
		if len(arm.ReasonMustContain) == 0 {
			return fixtureRuleError(index, fmt.Sprintf("arm %d mutates before the write without asserting the explanation", position),
				"fix=name a phrase from the guard's own message, so the row cannot pass on an unrelated failure")
		}
	case mutateBeforeRemove:
		if arm.UninstallOutcome != githooks.OutcomeFailed {
			return fixtureRuleError(index, fmt.Sprintf("arm %d mutates before the remove but expects uninstallOutcome %q", position, arm.UninstallOutcome),
				"fix=expect failed; a slot that changed after the plan must stop the delete")
		}
		if !arm.PresentAfterUninstall {
			return fixtureRuleError(index, fmt.Sprintf("arm %d mutates before the remove but expects the slot to be gone", position),
				"fix=expect it present; refusing to delete means the file is still there")
		}
		if len(arm.UninstallReasonMustContain) == 0 {
			return fixtureRuleError(index, fmt.Sprintf("arm %d mutates before the remove without asserting the explanation", position),
				"fix=name a phrase from the guard's own message, so the row cannot pass on an unrelated failure")
		}
	}

	// The window is INSIDE one call, and the only deterministic way into it is
	// the planning of a LATER event in the same call. So the case must request
	// another event after this one; the last arm has no window behind it.
	if arm.Event == testCase.RequestedEvents[len(testCase.RequestedEvents)-1] {
		return fixtureRuleError(index, fmt.Sprintf("arm %d mutates the last requested event %q", position, arm.Event),
			"fix=request a later event too; the mutation is applied while the call is still planning the event after this one, so the last event has no window behind it")
	}
	return nil
}

func fixtureRuleError(index int, what, fix string) error {
	return fmt.Errorf(
		"hook lifecycle fixture rule failed: %s; a malformed case invalidates the mounted lifecycle evidence; "+
			"where=%s case index %d; when=test fixture loading; impact=install/status/uninstall coverage cannot be trusted; %s",
		what, lifecycleFixturePath, index, fix)
}

func parseFixtureMode(raw string) (fs.FileMode, error) {
	value, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, err
	}
	return fs.FileMode(value), nil
}

func containsValue[T comparable](values []T, want T) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// --- loader guards ----------------------------------------------------------

func TestLoadLifecycleFixture_RejectsUnknownField(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleUnknownFieldFixtureData)
	if err == nil || !strings.Contains(err.Error(), "field unexpectedEvidence not found") {
		t.Fatalf("error = %v, want unknown-field rejection", err)
	}
}

func TestLoadLifecycleFixture_RejectsSecondDocument(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleSecondDocumentFixtureData)
	if err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("error = %v, want second-document rejection", err)
	}
}

func TestLoadLifecycleFixture_RejectsCountMismatch(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleCountMismatchFixtureData)
	if err == nil || !strings.Contains(err.Error(), "expectedCaseCount=4 cases=1") {
		t.Fatalf("error = %v, want case-count rejection", err)
	}
}

func TestLoadLifecycleFixture_RejectsDuplicateCaseName(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleDuplicateNameFixtureData)
	if err == nil || !strings.Contains(err.Error(), "duplicate case name") {
		t.Fatalf("error = %v, want duplicate-name rejection", err)
	}
}

func TestLoadLifecycleFixture_RejectsArmForUnrequestedEvent(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleUnrequestedArmFixtureData)
	if err == nil || !strings.Contains(err.Error(), "expected \"post-commit\"") {
		t.Fatalf("error = %v, want arm/event mismatch rejection", err)
	}
}

func TestLoadLifecycleFixture_RejectsContentOnNonForeignSeed(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleContentWithoutForeignFixtureData)
	if err == nil || !strings.Contains(err.Error(), "cannot carry content or mode") {
		t.Fatalf("error = %v, want content-on-non-foreign rejection", err)
	}
}

// The guards below reject a post-plan mutation that could not prove what it
// claims. They matter more than the schema guards above: a malformed seed usually
// makes a case fail, while a malformed mutation makes it PASS while testing
// nothing, which is indistinguishable from working coverage.

func TestLoadLifecycleFixture_RejectsUnknownMutationWindow(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleMutationUnknownWindowFixtureData)
	if err == nil || !strings.Contains(err.Error(), "unsupported mutation window") {
		t.Fatalf("error = %v, want rejection of a window the driver cannot apply", err)
	}
}

func TestLoadLifecycleFixture_RejectsUnknownMutationShape(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleMutationUnknownShapeFixtureData)
	if err == nil || !strings.Contains(err.Error(), "unsupported mutation shape") {
		t.Fatalf("error = %v, want rejection of a shape with no implementation", err)
	}
}

func TestLoadLifecycleFixture_RejectsMutationWithoutTheBytesItProtects(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleMutationWithoutContentFixtureData)
	if err == nil || !strings.Contains(err.Error(), "without naming them") {
		t.Fatalf("error = %v, want rejection of foreign bytes that are never stated", err)
	}
}

func TestLoadLifecycleFixture_RejectsMutationThatStillExpectsSuccess(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleMutationExpectingSuccessFixtureData)
	if err == nil || !strings.Contains(err.Error(), "expects installOutcome") {
		t.Fatalf("error = %v, want rejection of a row asserting the guard does not work", err)
	}
}

func TestLoadLifecycleFixture_RejectsMutationWithoutAnExplanationAssertion(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleMutationWithoutExplanationFixtureData)
	if err == nil || !strings.Contains(err.Error(), "without asserting the explanation") {
		t.Fatalf("error = %v, want rejection of a row any failure would satisfy", err)
	}
}

func TestLoadLifecycleFixture_RejectsMutationWithNoWindowBehindIt(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleMutationOnLastEventFixtureData)
	if err == nil || !strings.Contains(err.Error(), "mutates the last requested event") {
		t.Fatalf("error = %v, want rejection of a mutation with no window to land in", err)
	}
}

func TestLoadLifecycleFixture_RejectsTwoMutationsInOneCase(t *testing.T) {
	t.Parallel()
	_, err := loadLifecycleFixture(lifecycleMutationTwiceFixtureData)
	if err == nil || !strings.Contains(err.Error(), "declare a post-plan mutation") {
		t.Fatalf("error = %v, want rejection of a second mutation that could never be applied", err)
	}
}

// --- the corpus -------------------------------------------------------------

// TestLifecycle_Fixtures drives the whole plan/install/status/uninstall
// lifecycle against a disposable repository for every case in the corpus.
func TestLifecycle_Fixtures(t *testing.T) {
	t.Parallel()
	document, err := loadLifecycleFixture(lifecycleFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			runLifecycleCase(t, testCase)
		})
	}
}

func runLifecycleCase(t *testing.T, testCase lifecycleFixtureCase) {
	t.Helper()
	repo := disposableRepo(t)
	lifecycle := githooks.New(githooks.NewExecGit())
	ctx := t.Context()

	// Seed every slot, and remember the exact bytes and mode of anything Peasant
	// must never touch.
	untouchable := make(map[string]string)
	for _, seed := range testCase.Seed {
		path := hookPath(t, repo, seed.Event.String())
		switch seed.Kind {
		case seedManaged:
			seedManagedHook(t, lifecycle, repo, seed.Event)
		case seedForeign:
			mode, err := parseFixtureMode(seed.Mode)
			if err != nil {
				t.Fatalf("parse seed mode: %v", err)
			}
			writeSeedFile(t, repo, path, seed.Content, mode)
			untouchable[path] = fileFingerprint(t, path)
		case seedEditedManaged:
			// Install a real hook, then append a line the way any other tool
			// would. The framing breaks; the upload command does not.
			seedManagedHook(t, lifecycle, repo, seed.Event)
			appendToFile(t, path, "\n# another tool appended this line\n")
			untouchable[path] = fileFingerprint(t, path)
		case seedHandAdded:
			snippet, err := githooks.ManualSnippet(seed.Event, repo, path, githooks.Binding{})
			if err != nil {
				t.Fatalf("render manual snippet: %v", err)
			}
			composed := "#!/bin/sh\n# a hook the user composed themselves\n" + snippet + "\nexit 0\n"
			writeSeedFile(t, repo, path, composed, 0o755)
			untouchable[path] = fileFingerprint(t, path)
		}
	}

	beforeInstall := slotFingerprints(t, repo)
	requested := requestedSet(testCase.RequestedEvents)

	// Status is read-only and describes exactly what install will do.
	planReport, err := lifecycle.Status(ctx, githooks.Request{Dir: repo, Events: testCase.RequestedEvents})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if planReport.Repository.Root != repo {
		t.Fatalf("plan repository root = %q, want %q", planReport.Repository.Root, repo)
	}
	if len(planReport.Plans) != len(testCase.Expected) {
		t.Fatalf("plan returned %d entries, want %d", len(planReport.Plans), len(testCase.Expected))
	}
	for index, arm := range testCase.Expected {
		plan := planReport.Plans[index]
		requireInsideRepo(t, repo, plan.Path)
		if plan.Event != arm.Event {
			t.Fatalf("plan[%d] event = %q, want %q", index, plan.Event, arm.Event)
		}
		if plan.Ownership != arm.OwnershipBefore {
			t.Errorf("%s ownership = %q, want %q", arm.Event, plan.Ownership, arm.OwnershipBefore)
		}
		if plan.Action != arm.PlannedAction {
			t.Errorf("%s action = %q, want %q", arm.Event, plan.Action, arm.PlannedAction)
		}
		if plan.Action == githooks.ActionRefuse {
			if plan.Script != "" {
				t.Errorf("%s refusal must not carry a script to write", arm.Event)
			}
			if arm.OffersManualSection {
				assertUsableManualSnippet(t, arm.Event, plan.Manual)
			} else if plan.Manual != "" {
				t.Errorf("%s refusal must not offer a by-hand section here; an upload already runs from that file:\n%s", arm.Event, plan.Manual)
			}
		} else {
			if !githooks.IsManaged([]byte(plan.Script)) {
				t.Errorf("%s planned script is not recognizable Peasant-owned content", arm.Event)
			}
			if plan.Manual != "" {
				t.Errorf("%s plan offers manual guidance for an action it can perform", arm.Event)
			}
		}
	}
	if changed := changedSlots(t, repo, beforeInstall); len(changed) != 0 {
		t.Fatalf("plan changed %v; planning must be read-only", changed)
	}

	// Install acts per event and never touches an unrequested slot.
	installer, writeSabotage := stageLifecycle(t, repo, testCase, mutateBeforeWrite)
	installReport, err := withinStageBudget(t, "install", func() (githooks.ChangeReport, error) {
		return installer.Install(ctx, githooks.Request{Dir: repo, Events: testCase.RequestedEvents})
	})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	recordSabotage(t, writeSabotage, untouchable)
	for index, arm := range testCase.Expected {
		result := installReport.Results[index]
		if result.Event != arm.Event {
			t.Fatalf("install[%d] event = %q, want %q", index, result.Event, arm.Event)
		}
		if result.Outcome != arm.InstallOutcome {
			t.Errorf("%s install outcome = %q, want %q (reason: %s)", arm.Event, result.Outcome, arm.InstallOutcome, result.Reason)
		}
		if result.Outcome == githooks.OutcomeRefused && arm.OffersManualSection {
			assertUsableManualSnippet(t, arm.Event, result.Manual)
		}
		for _, want := range arm.ReasonMustContain {
			if !strings.Contains(result.Reason, want) {
				t.Errorf("%s install explanation must state %q; got:\n%s", arm.Event, want, result.Reason)
			}
		}
		for _, forbidden := range arm.ReasonMustNotContain {
			if strings.Contains(result.Reason, forbidden) {
				t.Errorf("%s install explanation must not say %q; got:\n%s", arm.Event, forbidden, result.Reason)
			}
		}
		assertHookState(t, result.Path, arm.ManagedAfterInstall, planReport.Plans[index].Script)
	}
	assertUnrequestedSlotsUnchanged(t, repo, beforeInstall, requested, "install")
	assertUntouchable(t, untouchable, "install")

	// Status reports every managed event, read-only.
	statusReport, err := lifecycle.Status(ctx, githooks.Request{Dir: repo})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(statusReport.Plans) != len(githooks.AllEvents) {
		t.Fatalf("status returned %d events, want %d", len(statusReport.Plans), len(githooks.AllEvents))
	}
	for _, arm := range testCase.Expected {
		status := findStatus(t, statusReport, arm.Event)
		if status.Managed() != arm.ManagedAfterInstall {
			t.Errorf("%s status managed = %v, want %v", arm.Event, status.Managed(), arm.ManagedAfterInstall)
		}
	}
	afterStatus := slotFingerprints(t, repo)

	// Uninstall removes only intact Peasant-generated files.
	remover, removeSabotage := stageLifecycle(t, repo, testCase, mutateBeforeRemove)
	uninstallReport, err := withinStageBudget(t, "uninstall", func() (githooks.ChangeReport, error) {
		return remover.Uninstall(ctx, githooks.Request{Dir: repo, Events: testCase.RequestedEvents})
	})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	recordSabotage(t, removeSabotage, untouchable)
	for index, arm := range testCase.Expected {
		result := uninstallReport.Results[index]
		if result.Event != arm.Event {
			t.Fatalf("uninstall[%d] event = %q, want %q", index, result.Event, arm.Event)
		}
		if result.Outcome != arm.UninstallOutcome {
			t.Errorf("%s uninstall outcome = %q, want %q (reason: %s)", arm.Event, result.Outcome, arm.UninstallOutcome, result.Reason)
		}
		if present := slotExists(result.Path); present != arm.PresentAfterUninstall {
			t.Errorf("%s present after uninstall = %v, want %v", arm.Event, present, arm.PresentAfterUninstall)
		}
		for _, want := range arm.UninstallReasonMustContain {
			if !strings.Contains(result.Reason, want) {
				t.Errorf("%s uninstall explanation must state %q; got:\n%s", arm.Event, want, result.Reason)
			}
		}
	}
	assertUnrequestedSlotsUnchanged(t, repo, afterStatus, requested, "uninstall")
	assertUntouchable(t, untouchable, "uninstall")
}

// --- the post-plan mutation window ------------------------------------------

// lifecycleStageBudget bounds one Install or Uninstall in the corpus.
//
// It is generous, because these stages run git and touch the filesystem, and its
// only job is to turn a HANG into a failure. Reading a FIFO with no writer waits
// forever, so a regression in the pre-write re-check does not fail - it parks the
// whole package until the outer test timeout kills it, ten minutes later, with a
// goroutine dump instead of an explanation.
const lifecycleStageBudget = 30 * time.Second

// postPlanSabotage is one mutation applied inside a lifecycle call, plus the
// fingerprint of the slot immediately after it was applied.
//
// The fingerprint is taken THERE rather than by the test beforehand, because the
// guarantee is about the state Peasant was actually about to act on. Taking it
// afterwards would measure the damage instead of detecting it.
type postPlanSabotage struct {
	path        string
	fingerprint string
	applied     bool
	err         error
}

// slotSaboteur decorates the production resolver and applies a mutation while a
// lifecycle call is still planning.
//
// This is the only deterministic way to reach the window the no-clobber guards
// defend. Install and Uninstall each call plan() themselves, so the window is
// INSIDE one call: mutating before the call is pointless, because that call
// re-inspects and refuses at plan time. But plan() resolves the hook path of
// EVERY requested event before the call acts on ANY of them, so answering the
// path question for a later event is a point that is provably after the earlier
// event was inspected and provably before it is written or deleted.
//
// It mocks a DEPENDENCY - the git resolver, which githooks.New already takes as
// an interface - and not the subject: the real Lifecycle, the real planner, and
// the real write and delete paths all run untouched.
type slotSaboteur struct {
	inner   githooks.GitResolver
	trigger githooks.Event
	mutate  func() (string, error)
	result  *postPlanSabotage
}

var _ githooks.GitResolver = (*slotSaboteur)(nil)

func (s *slotSaboteur) Root(ctx context.Context, dir string) (string, error) {
	return s.inner.Root(ctx, dir)
}

func (s *slotSaboteur) GitDir(ctx context.Context, dir string) (string, error) {
	return s.inner.GitDir(ctx, dir)
}

func (s *slotSaboteur) HookPath(ctx context.Context, dir string, event githooks.Event) (string, error) {
	path, err := s.inner.HookPath(ctx, dir, event)
	if event != s.trigger || s.result.applied {
		return path, err
	}
	s.result.applied = true
	s.result.fingerprint, s.result.err = s.mutate()
	return path, err
}

// stageLifecycle returns the Lifecycle to drive one stage with: the plain one,
// or one whose resolver applies this case's mutation inside the call when the
// case declares it for this stage's window.
func stageLifecycle(t *testing.T, repo string, testCase lifecycleFixtureCase, window mutationWindow) (*githooks.Lifecycle, *postPlanSabotage) {
	t.Helper()
	for position, arm := range testCase.Expected {
		if arm.PostPlanMutation == nil || arm.PostPlanMutation.Window != window {
			continue
		}
		// The loader has already proven a later requested event exists.
		trigger := testCase.RequestedEvents[position+1]
		path := hookPath(t, repo, arm.Event.String())
		sabotage := &postPlanSabotage{path: path}
		mutation := *arm.PostPlanMutation
		return githooks.New(&slotSaboteur{
			inner:   githooks.NewExecGit(),
			trigger: trigger,
			mutate:  func() (string, error) { return applyPostPlanMutation(repo, path, arm.Event, mutation) },
			result:  sabotage,
		}), sabotage
	}
	return githooks.New(githooks.NewExecGit()), nil
}

// applyPostPlanMutation makes the slot into mutation.Shape and returns the
// fingerprint of what now occupies it.
//
// It returns errors rather than failing the test directly: it runs on the
// goroutine driving the lifecycle call, where t.Fatalf is not allowed.
func applyPostPlanMutation(repo, path string, event githooks.Event, mutation lifecycleFixtureMutation) (string, error) {
	if mutation.Shape == mutateToForeignBytes {
		mode, err := parseFixtureMode(mutation.Mode)
		if err != nil {
			return "", err
		}
		// O_TRUNC, not Remove-then-create: the slot may be occupied or empty, and
		// replacing the bytes in place is what an unrelated tool would do.
		if err := os.WriteFile(path, []byte(mutation.Content), mode); err != nil {
			return "", err
		}
		if err := os.Chmod(path, mode); err != nil {
			return "", err
		}
		return slotFingerprint(path)
	}

	// Both symlink shapes need the slot to BE a link, so whatever is there now
	// goes first. The target is built beside the slot, inside the repository.
	target := path + ".swapped-target"
	switch mutation.Shape {
	case mutateToSymlinkedPipe:
		if err := exec.Command("mkfifo", target).Run(); err != nil {
			return "", fmt.Errorf("create the named-pipe target with mkfifo: %w", err)
		}
	case mutateToSymlinkedGeneratedHook:
		// A byte-perfect generated hook, so the link defeats an ownership test
		// that follows it: the content on the far side is recognisably Peasant's.
		script, err := githooks.Script(event, repo, path, githooks.Binding{})
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(target, []byte(script), githooks.ScriptMode); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("no mutation implemented for shape %q", mutation.Shape)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := os.Symlink(target, path); err != nil {
		return "", err
	}
	return slotFingerprint(path)
}

// recordSabotage registers what the mutation left behind as a file Peasant must
// not have touched, and fails loudly if the mutation never ran.
//
// The never-ran check is what keeps this whole group of cases from going quietly
// vacuous. If the window ever closes - a refactor that plans and acts per event,
// say - the mutation stops firing, the stage returns an ordinary plan-time
// refusal, and without this the row would still pass while proving nothing. That
// is the exact failure these rows were written to end.
func recordSabotage(t *testing.T, sabotage *postPlanSabotage, untouchable map[string]string) {
	t.Helper()
	if sabotage == nil {
		return
	}
	if !sabotage.applied {
		t.Fatalf("the declared post-plan mutation never ran, so nothing was proven: the resolver was never asked for the triggering event's hook path. The decide-then-act window this case exists to test is no longer where it was.")
	}
	if sabotage.err != nil {
		t.Fatalf("apply the post-plan mutation to %s: %v", sabotage.path, sabotage.err)
	}
	untouchable[sabotage.path] = sabotage.fingerprint
}

// withinStageBudget runs one lifecycle stage and fails if it does not return.
func withinStageBudget(
	t *testing.T,
	stage string,
	run func() (githooks.ChangeReport, error),
) (githooks.ChangeReport, error) {
	t.Helper()
	type outcome struct {
		report githooks.ChangeReport
		err    error
	}
	done := make(chan outcome, 1)
	go func() { report, err := run(); done <- outcome{report, err} }()
	timer := time.NewTimer(lifecycleStageBudget)
	defer timer.Stop()
	select {
	case result := <-done:
		return result.report, result.err
	case <-timer.C:
		t.Fatalf("%s did not return within %s. The re-check that runs immediately before a write or a delete must REFUSE a slot it cannot read as an ordinary file, not block on it: opening a FIFO read-only waits forever for a writer, which turns a refusal into a git command that never finishes.",
			stage, lifecycleStageBudget)
		return githooks.ChangeReport{}, nil
	}
}

// --- case helpers -----------------------------------------------------------

func seedManagedHook(t *testing.T, lifecycle *githooks.Lifecycle, repo string, event githooks.Event) {
	t.Helper()
	report, err := lifecycle.Install(t.Context(), githooks.Request{Dir: repo, Events: []githooks.Event{event}})
	if err != nil {
		t.Fatalf("seed managed %s hook: %v", event, err)
	}
	if len(report.Results) != 1 || report.Results[0].Outcome != githooks.OutcomeCreated {
		t.Fatalf("seed managed %s hook: unexpected report %+v", event, report.Results)
	}
}

// appendToFile adds text to an existing hook, the way an unrelated tool would.
func appendToFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
	if _, err := file.WriteString(text); err != nil {
		file.Close()
		t.Fatalf("append to %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("append to %s: %v", path, err)
	}
}

func writeSeedFile(t *testing.T, repo, path, content string, mode fs.FileMode) {
	t.Helper()
	requireInsideRepo(t, repo, path)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write seed file %s: %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod seed file %s: %v", path, err)
	}
}

// assertHookState checks a slot after install: either it holds exactly the
// script the plan promised and is executable, or it holds nothing of Peasant's.
func assertHookState(t *testing.T, path string, wantManaged bool, promised string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		if wantManaged {
			t.Errorf("read %s: %v", path, err)
		}
		return
	}
	if githooks.IsManaged(content) != wantManaged {
		t.Errorf("%s managed = %v, want %v", path, githooks.IsManaged(content), wantManaged)
		return
	}
	if !wantManaged {
		return
	}
	if string(content) != promised {
		t.Errorf("%s does not hold the exact bytes the plan promised", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm() != githooks.ScriptMode.Perm() {
		t.Errorf("%s mode = %04o, want %04o; git only runs an executable hook",
			path, info.Mode().Perm(), githooks.ScriptMode.Perm())
	}
}

// assertUsableManualSnippet checks the by-hand section is present, names the
// event, and - critically - carries no ownership marker, so pasting it into a
// user's own hook can never make Peasant claim that file later.
func assertUsableManualSnippet(t *testing.T, event githooks.Event, snippet string) {
	t.Helper()
	if strings.TrimSpace(snippet) == "" {
		t.Errorf("%s refusal must hand back the exact section to add by hand", event)
		return
	}
	if !strings.Contains(snippet, "peasant village push --non-interactive") {
		t.Errorf("%s manual section must name the exact upload command; got:\n%s", event, snippet)
	}
	if !strings.Contains(snippet, event.String()) {
		t.Errorf("%s manual section must name its event; got:\n%s", event, snippet)
	}
	for _, marker := range []string{githooks.ScriptMarkerBegin, githooks.ScriptMarkerEnd} {
		if strings.Contains(snippet, marker) {
			t.Errorf("%s manual section must not carry the ownership marker %q: pasting it would make Peasant claim a hook it did not write",
				event, marker)
		}
	}
}

func requestedSet(events []githooks.Event) map[githooks.Event]bool {
	requested := make(map[githooks.Event]bool, len(events))
	for _, event := range events {
		requested[event] = true
	}
	return requested
}

// slotFingerprints fingerprints every managed event's slot, absent included.
func slotFingerprints(t *testing.T, repo string) map[githooks.Event]string {
	t.Helper()
	fingerprints := make(map[githooks.Event]string, len(githooks.AllEvents))
	for _, event := range githooks.AllEvents {
		fingerprints[event] = mustSlotFingerprint(t, hookPath(t, repo, event.String()))
	}
	return fingerprints
}

func changedSlots(t *testing.T, repo string, before map[githooks.Event]string) []githooks.Event {
	t.Helper()
	var changed []githooks.Event
	for event, fingerprint := range slotFingerprints(t, repo) {
		if before[event] != fingerprint {
			changed = append(changed, event)
		}
	}
	return changed
}

func assertUnrequestedSlotsUnchanged(t *testing.T, repo string, before map[githooks.Event]string, requested map[githooks.Event]bool, stage string) {
	t.Helper()
	after := slotFingerprints(t, repo)
	for _, event := range githooks.AllEvents {
		if requested[event] {
			continue
		}
		if before[event] != after[event] {
			t.Errorf("%s changed the unrequested %s slot: %q became %q", stage, event, before[event], after[event])
		}
	}
}

func assertUntouchable(t *testing.T, untouchable map[string]string, stage string) {
	t.Helper()
	for path, want := range untouchable {
		if got := mustSlotFingerprint(t, path); got != want {
			t.Errorf("%s modified a file Peasant does not own: %s\n  before: %s\n  after:  %s", stage, path, want, got)
		}
	}
}

func findStatus(t *testing.T, report githooks.PlanReport, event githooks.Event) githooks.Plan {
	t.Helper()
	for _, plan := range report.Plans {
		if plan.Event == event {
			return plan
		}
	}
	t.Fatalf("status report has no entry for %s", event)
	return githooks.Plan{}
}

func slotExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
