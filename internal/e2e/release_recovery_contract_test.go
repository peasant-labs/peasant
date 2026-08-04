package e2e

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/releaserecovery"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/workflows/recovery_contract.yaml
var recoveryContractFixtureBytes []byte

type recoveryMutation string
type recoveryGate string

const (
	recoveryMutationNone                     recoveryMutation = "none"
	recoveryMutationWrongRepositoryID        recoveryMutation = "wrong_repository_id"
	recoveryMutationWrongDispatchActor       recoveryMutation = "wrong_dispatch_actor"
	recoveryMutationMismatchedRecoveryHead   recoveryMutation = "mismatched_recovery_head"
	recoveryMutationRecoveryRunHeadDrift     recoveryMutation = "recovery_run_head_drift"
	recoveryMutationMissingRecoveryConfirm   recoveryMutation = "missing_recovery_confirmation"
	recoveryMutationPriorRecoveryDispatch    recoveryMutation = "prior_recovery_dispatch"
	recoveryMutationStaleOperatorPermission  recoveryMutation = "stale_operator_permission"
	recoveryMutationSameNamedBranch          recoveryMutation = "same_named_branch"
	recoveryMutationMovedTagObject           recoveryMutation = "moved_tag_object"
	recoveryMutationChangedRuleset           recoveryMutation = "changed_ruleset"
	recoveryMutationWrongOriginalWorkflow    recoveryMutation = "wrong_original_workflow"
	recoveryMutationOriginalHasJob           recoveryMutation = "original_has_job"
	recoveryMutationWrongGateSHA             recoveryMutation = "wrong_gate_sha"
	recoveryMutationWrongGateActor           recoveryMutation = "wrong_gate_actor"
	recoveryMutationWrongGateWorkflow        recoveryMutation = "wrong_gate_workflow"
	recoveryMutationSecondGateAttempt        recoveryMutation = "second_gate_attempt"
	recoveryMutationZeroGateJobs             recoveryMutation = "zero_gate_jobs"
	recoveryMutationFailedCriticalStep       recoveryMutation = "failed_critical_step"
	recoveryMutationStaleGate                recoveryMutation = "stale_gate"
	recoveryMutationExpiredRecovery          recoveryMutation = "expired_recovery"
	recoveryMutationDraftRelease             recoveryMutation = "draft_release"
	recoveryMutationPartialRelease           recoveryMutation = "partial_release"
	recoveryMutationPublishedReleaseEndpoint recoveryMutation = "published_release_endpoint"
	recoveryMutationFullReleasePage          recoveryMutation = "full_release_page"
	recoveryMutationEnabledAUR               recoveryMutation = "enabled_aur"
	recoveryMutationEnabledHomebrew          recoveryMutation = "enabled_homebrew"
	recoveryMutationUnquotedAURBoolean       recoveryMutation = "unquoted_aur_boolean"
	recoveryGateE2E                          recoveryGate     = "e2e"
	recoveryGateReleaseE2E                   recoveryGate     = "release_e2e"
)

type recoveryContractFixture struct {
	Workflow struct {
		Path                        string                              `yaml:"path"`
		Trigger                     string                              `yaml:"trigger"`
		Inputs                      nonEmptyStrings                     `yaml:"inputs"`
		ConcurrencyGroup            string                              `yaml:"concurrency_group"`
		ConcurrencyCancelInProgress string                              `yaml:"concurrency_cancel_in_progress"`
		TopEnv                      []workflowEnvExpectation            `yaml:"top_env"`
		PreflightEnv                []recoveryWorkflowEnvExpectation    `yaml:"preflight_env"`
		Jobs                        []workflowJobPermissionsExpectation `yaml:"jobs"`
		TagCheckoutJobs             nonEmptyStrings                     `yaml:"tag_checkout_jobs"`
		RequiredSource              nonEmptyStrings                     `yaml:"required_source"`
		ForbiddenSource             nonEmptyStrings                     `yaml:"forbidden_source"`
	} `yaml:"workflow"`
	PreflightCases      []recoveryCaseFixture `yaml:"preflight_cases"`
	ReleaseAbsenceCases []recoveryCaseFixture `yaml:"release_absence_cases"`
	PrePublishCases     []recoveryCaseFixture `yaml:"pre_publish_cases"`
	PublisherCases      []recoveryCaseFixture `yaml:"publisher_cases"`
}

type recoveryWorkflowEnvExpectation struct {
	Key       string `yaml:"key"`
	Value     string `yaml:"value"`
	TestValue string `yaml:"test_value"`
}

type recoveryCaseFixture struct {
	Name      string           `yaml:"name"`
	Mutation  recoveryMutation `yaml:"mutation"`
	Gate      recoveryGate     `yaml:"gate"`
	WantError string           `yaml:"want_error"`
}

func loadRecoveryContractFixture(t *testing.T) recoveryContractFixture {
	t.Helper()
	var fixture recoveryContractFixture
	decoder := yaml.NewDecoder(bytes.NewReader(recoveryContractFixtureBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("release recovery: parse contract fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("release recovery: fixture must have exact EOF: %v", err)
	}
	if fixture.Workflow.Path == "" || fixture.Workflow.Trigger == "" || len(fixture.Workflow.Inputs) != 5 || fixture.Workflow.ConcurrencyGroup == "" || fixture.Workflow.ConcurrencyCancelInProgress == "" || len(fixture.Workflow.TopEnv) != 4 || len(fixture.Workflow.PreflightEnv) != 14 || len(fixture.Workflow.Jobs) != 4 || len(fixture.Workflow.TagCheckoutJobs) != 3 || len(fixture.Workflow.RequiredSource) != 9 || len(fixture.Workflow.ForbiddenSource) != 8 || len(fixture.PreflightCases) != 28 || len(fixture.ReleaseAbsenceCases) != 5 || len(fixture.PrePublishCases) != 4 || len(fixture.PublisherCases) != 4 {
		t.Fatalf("release recovery: fixture row-count guard failed: %+v", fixture)
	}
	for index, environment := range fixture.Workflow.PreflightEnv {
		if strings.TrimSpace(environment.Key) == "" || strings.TrimSpace(environment.Value) == "" || strings.TrimSpace(environment.TestValue) == "" {
			t.Fatalf("release recovery: preflight environment row %d is incomplete: %+v", index, environment)
		}
	}
	seenNames := make(map[string]struct{})
	for _, group := range [][]recoveryCaseFixture{fixture.PreflightCases, fixture.ReleaseAbsenceCases, fixture.PrePublishCases, fixture.PublisherCases} {
		for _, testCase := range group {
			if strings.TrimSpace(testCase.Name) == "" || !validRecoveryMutation(testCase.Mutation) {
				t.Fatalf("release recovery: invalid fixture case: %+v", testCase)
			}
			if testCase.Mutation != recoveryMutationNone && strings.TrimSpace(testCase.WantError) == "" {
				t.Fatalf("release recovery: negative case %q needs an expected error", testCase.Name)
			}
			if isGateRecoveryMutation(testCase.Mutation) != (testCase.Gate == recoveryGateE2E || testCase.Gate == recoveryGateReleaseE2E) {
				t.Fatalf("release recovery: case %q must bind gate mutations to exactly e2e or release_e2e: %+v", testCase.Name, testCase)
			}
			if _, exists := seenNames[testCase.Name]; exists {
				t.Fatalf("release recovery: duplicate fixture case name %q", testCase.Name)
			}
			seenNames[testCase.Name] = struct{}{}
		}
	}
	return fixture
}

func isGateRecoveryMutation(mutation recoveryMutation) bool {
	switch mutation {
	case recoveryMutationWrongGateSHA, recoveryMutationWrongGateActor, recoveryMutationWrongGateWorkflow, recoveryMutationSecondGateAttempt, recoveryMutationZeroGateJobs, recoveryMutationFailedCriticalStep, recoveryMutationStaleGate:
		return true
	default:
		return false
	}
}

func validRecoveryMutation(mutation recoveryMutation) bool {
	switch mutation {
	case recoveryMutationNone,
		recoveryMutationWrongRepositoryID,
		recoveryMutationWrongDispatchActor,
		recoveryMutationMismatchedRecoveryHead,
		recoveryMutationRecoveryRunHeadDrift,
		recoveryMutationMissingRecoveryConfirm,
		recoveryMutationPriorRecoveryDispatch,
		recoveryMutationStaleOperatorPermission,
		recoveryMutationSameNamedBranch,
		recoveryMutationMovedTagObject,
		recoveryMutationChangedRuleset,
		recoveryMutationWrongOriginalWorkflow,
		recoveryMutationOriginalHasJob,
		recoveryMutationWrongGateSHA,
		recoveryMutationWrongGateActor,
		recoveryMutationWrongGateWorkflow,
		recoveryMutationSecondGateAttempt,
		recoveryMutationZeroGateJobs,
		recoveryMutationFailedCriticalStep,
		recoveryMutationStaleGate,
		recoveryMutationExpiredRecovery,
		recoveryMutationDraftRelease,
		recoveryMutationPartialRelease,
		recoveryMutationPublishedReleaseEndpoint,
		recoveryMutationFullReleasePage,
		recoveryMutationEnabledAUR,
		recoveryMutationEnabledHomebrew,
		recoveryMutationUnquotedAURBoolean:
		return true
	default:
		return false
	}
}

func TestReleaseRecoveryWorkflowContract(t *testing.T) {
	fixture := loadRecoveryContractFixture(t)
	path := filepath.Join(releaseWorkflowRepoRoot(t), fixture.Workflow.Path)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("release recovery: read workflow: %v", err)
	}
	doc := readWorkflowDoc(t, path)

	on := yamlMappingValue(doc, "on")
	if on == nil || on.Kind != yaml.MappingNode || len(on.Content) != 2 || yamlMappingValue(on, fixture.Workflow.Trigger) == nil {
		t.Fatalf("release recovery: workflow must have only the %s trigger", fixture.Workflow.Trigger)
	}
	dispatch := yamlMappingValue(on, fixture.Workflow.Trigger)
	inputs := yamlMappingValue(dispatch, "inputs")
	assertYAMLMappingKeysExact(t, inputs, fixture.Workflow.Inputs, "release recovery inputs")
	for _, name := range fixture.Workflow.Inputs {
		input := yamlMappingValue(inputs, name)
		required := yamlMappingValue(input, "required")
		inputType := yamlMappingValue(input, "type")
		description := yamlMappingValue(input, "description")
		if required == nil || required.Value != "true" || inputType == nil || inputType.Value != "string" || description == nil || strings.TrimSpace(description.Value) == "" || yamlMappingValue(input, "default") != nil {
			t.Fatalf("release recovery: input %q must be required string confirmation with no default", name)
		}
	}

	permissions := yamlMappingValue(doc, "permissions")
	if permissions == nil || permissions.Kind != yaml.MappingNode || len(permissions.Content) != 0 {
		t.Fatalf("release recovery: top-level permissions must be exactly empty")
	}
	assertYAMLMappingExact(t, yamlMappingValue(doc, "env"), fixture.Workflow.TopEnv, "release recovery top-level env")
	concurrency := yamlMappingValue(doc, "concurrency")
	assertYAMLScalar(t, yamlMappingValue(concurrency, "group"), fixture.Workflow.ConcurrencyGroup, "release recovery concurrency group")
	assertYAMLScalar(t, yamlMappingValue(concurrency, "cancel-in-progress"), fixture.Workflow.ConcurrencyCancelInProgress, "release recovery concurrency cancellation")
	canonicalRelease := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release.yml"))
	canonicalConcurrency := yamlMappingValue(canonicalRelease, "concurrency")
	canonicalGroup := yamlMappingValue(canonicalConcurrency, "group")
	if canonicalGroup == nil || strings.Replace(canonicalGroup.Value, "${{ github.ref }}", "refs/tags/v0.1.0", 1) != fixture.Workflow.ConcurrencyGroup {
		t.Fatalf("release recovery: concurrency group %q must match release.yml's effective immutable-tag group, canonical=%v", fixture.Workflow.ConcurrencyGroup, canonicalGroup)
	}
	assertYAMLScalar(t, yamlMappingValue(canonicalConcurrency, "cancel-in-progress"), fixture.Workflow.ConcurrencyCancelInProgress, "canonical release concurrency cancellation")

	jobs := yamlMappingValue(doc, "jobs")
	wantJobs := make([]string, 0, len(fixture.Workflow.Jobs))
	for _, expected := range fixture.Workflow.Jobs {
		wantJobs = append(wantJobs, expected.Job)
		job := yamlMappingValue(jobs, expected.Job)
		if job == nil {
			t.Fatalf("release recovery: missing job %q", expected.Job)
		}
		assertYAMLMappingExact(t, yamlMappingValue(job, "permissions"), expected.Permissions, "release recovery job "+expected.Job+" permissions")
	}
	assertYAMLMappingKeysExact(t, jobs, wantJobs, "release recovery jobs")
	assertRecoveryActionsPinned(t, jobs)
	preflightEnv := make([]workflowEnvExpectation, 0, len(fixture.Workflow.PreflightEnv))
	for _, environment := range fixture.Workflow.PreflightEnv {
		preflightEnv = append(preflightEnv, workflowEnvExpectation{Key: environment.Key, Value: environment.Value})
	}
	assertYAMLMappingExact(t, yamlMappingValue(yamlMappingValue(jobs, "preflight"), "env"), preflightEnv, "release recovery preflight environment")
	assertYAMLStringSet(t, yamlMappingValue(yamlMappingValue(jobs, "nix-vendor-hash"), "needs"), []string{"preflight"}, "release recovery Nix needs")
	assertYAMLStringSet(t, yamlMappingValue(yamlMappingValue(jobs, "release"), "needs"), []string{"preflight", "nix-vendor-hash"}, "release recovery publish needs")
	assertYAMLStringSet(t, yamlMappingValue(yamlMappingValue(jobs, "smoke"), "needs"), []string{"release"}, "release recovery smoke needs")

	for _, jobName := range fixture.Workflow.TagCheckoutJobs {
		job := yamlMappingValue(jobs, jobName)
		steps := yamlMappingValue(job, "steps")
		checkout := workflowStepNode(t, steps.Content, "Checkout immutable release source")
		with := yamlMappingValue(checkout, "with")
		assertYAMLScalar(t, yamlMappingValue(with, "ref"), "refs/tags/v0.1.0", jobName+" immutable checkout ref")
		assertYAMLScalar(t, yamlMappingValue(with, "path"), "release-src", jobName+" immutable checkout path")
		assertYAMLScalar(t, yamlMappingValue(with, "persist-credentials"), "false", jobName+" immutable checkout credentials")
	}
	for _, jobName := range []string{"preflight", "release"} {
		job := yamlMappingValue(jobs, jobName)
		steps := yamlMappingValue(job, "steps")
		checkout := workflowStepNode(t, steps.Content, "Checkout reviewed recovery verifier")
		with := yamlMappingValue(checkout, "with")
		assertYAMLScalar(t, yamlMappingValue(with, "ref"), "${{ github.sha }}", jobName+" verifier checkout ref")
		assertYAMLScalar(t, yamlMappingValue(with, "path"), "orchestrator", jobName+" verifier checkout path")
		assertYAMLScalar(t, yamlMappingValue(with, "persist-credentials"), "false", jobName+" verifier checkout credentials")
	}

	releaseSteps := yamlMappingValue(yamlMappingValue(jobs, "release"), "steps")
	assertStepBefore(t, releaseSteps.Content, "Authoritatively re-check immutable tag and release absence", "Publish immutable GitHub Release")
	prePublish := workflowStepNode(t, releaseSteps.Content, "Authoritatively re-check immutable tag and release absence")
	assertYAMLMappingExact(t, yamlMappingValue(prePublish, "env"), []workflowEnvExpectation{
		{Key: "GH_TOKEN", Value: "${{ github.token }}"},
		{Key: "RECOVERY_RUN_ID", Value: "${{ github.run_id }}"},
		{Key: "RECOVERY_HEAD_SHA", Value: "${{ github.sha }}"},
	}, "release recovery pre-publish environment")
	if run := yamlMappingValue(prePublish, "run"); run == nil || strings.TrimSpace(run.Value) != "go run ./cmd/release-recovery-verify pre-publish" {
		t.Fatalf("release recovery: pre-publish step must invoke the production verifier, got %v", run)
	}
	publish := workflowStepNode(t, releaseSteps.Content, "Publish immutable GitHub Release")
	assertYAMLMappingExact(t, yamlMappingValue(publish, "env"), []workflowEnvExpectation{
		{Key: "GITHUB_TOKEN", Value: "${{ github.token }}"},
		{Key: "GORELEASER_CURRENT_TAG", Value: "v0.1.0"},
		{Key: "AUR_KEY", Value: "unset-publisher-disabled"},
		{Key: "TAP_GITHUB_TOKEN", Value: "unset-publisher-disabled"},
	}, "release recovery publish environment")
	if run := yamlMappingValue(publish, "run"); run == nil || !strings.Contains(run.Value, "goreleaser\" release --clean") {
		t.Fatalf("release recovery: publish step must run tagged GoReleaser, got %v", run)
	}

	source := string(raw)
	for _, required := range fixture.Workflow.RequiredSource {
		if !strings.Contains(source, required) {
			t.Fatalf("release recovery: workflow source is missing %q", required)
		}
	}
	for _, forbidden := range fixture.Workflow.ForbiddenSource {
		if strings.Contains(source, forbidden) {
			t.Fatalf("release recovery: workflow source contains forbidden capability %q", forbidden)
		}
	}
}

func TestReleaseRecoveryPreflightEvidence(t *testing.T) {
	fixture := loadRecoveryContractFixture(t)
	for _, testCase := range fixture.PreflightCases {
		t.Run(testCase.Name, func(t *testing.T) {
			state := newRecoveryAPIState()
			applyRecoveryMutation(t, state, testCase.Mutation, testCase.Gate)
			server := httptest.NewServer(state)
			defer server.Close()
			verifier, err := releaserecovery.NewVerifier(releaserecovery.Config{
				APIURL:     server.URL,
				Token:      "fixture-token",
				HTTPClient: server.Client(),
				Now:        func() time.Time { return state.now },
			})
			if err != nil {
				t.Fatalf("release recovery: create verifier: %v", err)
			}
			err = verifier.VerifyPreflight(context.Background(), state.input)
			assertRecoveryError(t, err, testCase.WantError)
		})
	}
}

func TestReleaseRecoveryAuthoritativeAbsenceCheck(t *testing.T) {
	fixture := loadRecoveryContractFixture(t)
	for _, testCase := range fixture.ReleaseAbsenceCases {
		t.Run(testCase.Name, func(t *testing.T) {
			state := newRecoveryAPIState()
			applyRecoveryMutation(t, state, testCase.Mutation, testCase.Gate)
			server := httptest.NewServer(state)
			defer server.Close()
			verifier, err := releaserecovery.NewVerifier(releaserecovery.Config{
				APIURL: server.URL, Token: "fixture-token", HTTPClient: server.Client(), Now: func() time.Time { return state.now },
			})
			if err != nil {
				t.Fatalf("release recovery: create absence verifier: %v", err)
			}
			err = verifier.VerifyReleaseAbsent(context.Background())
			assertRecoveryError(t, err, testCase.WantError)
		})
	}
}

func TestReleaseRecoveryPrePublishRevalidation(t *testing.T) {
	fixture := loadRecoveryContractFixture(t)
	for _, testCase := range fixture.PrePublishCases {
		t.Run(testCase.Name, func(t *testing.T) {
			state := newRecoveryAPIState()
			applyRecoveryMutation(t, state, testCase.Mutation, testCase.Gate)
			server := httptest.NewServer(state)
			defer server.Close()
			verifier, err := releaserecovery.NewVerifier(releaserecovery.Config{
				APIURL: server.URL, Token: "fixture-token", HTTPClient: server.Client(), Now: func() time.Time { return state.now },
			})
			if err != nil {
				t.Fatalf("release recovery: create pre-publish verifier: %v", err)
			}
			err = verifier.VerifyPrePublish(context.Background(), releaserecovery.RecoveryRunInput{RunID: mockRecoveryRunID, HeadSHA: mockRecoveryHeadSHA})
			assertRecoveryError(t, err, testCase.WantError)
		})
	}
}

func TestReleaseRecoveryExternalPublishersStayDisabled(t *testing.T) {
	fixture := loadRecoveryContractFixture(t)
	configPath := filepath.Join(releaseWorkflowRepoRoot(t), ".goreleaser.yml")
	baseline, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("release recovery: read tagged-equivalent GoReleaser config: %v", err)
	}
	for _, testCase := range fixture.PublisherCases {
		t.Run(testCase.Name, func(t *testing.T) {
			mutated := string(baseline)
			switch testCase.Mutation {
			case recoveryMutationNone:
			case recoveryMutationEnabledAUR:
				mutated = replaceNth(t, mutated, `skip_upload: "true"`, `skip_upload: "false"`, 1)
			case recoveryMutationEnabledHomebrew:
				mutated = replaceNth(t, mutated, `skip_upload: "true"`, `skip_upload: "false"`, 2)
			case recoveryMutationUnquotedAURBoolean:
				mutated = replaceNth(t, mutated, `skip_upload: "true"`, `skip_upload: true`, 1)
			default:
				t.Fatalf("release recovery: publisher case uses unsupported mutation %q", testCase.Mutation)
			}
			path := filepath.Join(t.TempDir(), ".goreleaser.yml")
			if err := os.WriteFile(path, []byte(mutated), 0o600); err != nil {
				t.Fatalf("release recovery: write mutated publisher config: %v", err)
			}
			err := releaserecovery.VerifyPublishersDisabled(path)
			assertRecoveryError(t, err, testCase.WantError)
		})
	}
}

type recoveryAPIState struct {
	now                   time.Time
	input                 releaserecovery.PreflightInput
	repositoryID          int64
	permission            string
	branchExists          bool
	tagObjectSHA          string
	rulesetAppID          int64
	runs                  map[int64]*mockWorkflowRun
	jobs                  map[int64]*mockWorkflowJobs
	recoveryDispatches    []*mockWorkflowRun
	releases              []mockRelease
	releaseEndpointExists bool
}

type mockUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type mockWorkflowRun struct {
	ID              int64     `json:"id"`
	WorkflowID      int64     `json:"workflow_id"`
	Event           string    `json:"event"`
	Status          string    `json:"status"`
	Conclusion      string    `json:"conclusion"`
	HeadBranch      string    `json:"head_branch"`
	HeadSHA         string    `json:"head_sha"`
	Path            string    `json:"path"`
	RunAttempt      int       `json:"run_attempt"`
	Actor           mockUser  `json:"actor"`
	TriggeringActor mockUser  `json:"triggering_actor"`
	CreatedAt       time.Time `json:"created_at"`
}

type mockWorkflowJobs struct {
	TotalCount int               `json:"total_count"`
	Jobs       []mockWorkflowJob `json:"jobs"`
}

type mockWorkflowJob struct {
	Name       string             `json:"name"`
	Status     string             `json:"status"`
	Conclusion string             `json:"conclusion"`
	RunAttempt int                `json:"run_attempt"`
	Steps      []mockWorkflowStep `json:"steps"`
}

type mockWorkflowStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type mockRelease struct {
	ID      int64       `json:"id"`
	TagName string      `json:"tag_name"`
	Draft   bool        `json:"draft"`
	Assets  []mockAsset `json:"assets"`
}

type mockAsset struct {
	ID int64 `json:"id"`
}

const (
	mockRecoveryRunID   = int64(91001)
	mockE2ERunID        = int64(91002)
	mockReleaseE2ERunID = int64(91003)
	mockRecoveryHeadSHA = "1111111111111111111111111111111111111111"
)

func newRecoveryAPIState() *recoveryAPIState {
	now := time.Date(2026, time.August, 4, 21, 0, 0, 0, time.UTC)
	operator := mockUser{Login: releaserecovery.OperatorLogin, ID: releaserecovery.OperatorID}
	bot := mockUser{Login: releaserecovery.BotLogin, ID: releaserecovery.BotID}
	state := &recoveryAPIState{
		now:          now,
		repositoryID: releaserecovery.RepositoryID,
		permission:   "admin",
		tagObjectSHA: releaserecovery.TagObjectSHA,
		rulesetAppID: releaserecovery.ReleaserAppID,
		input: releaserecovery.PreflightInput{
			Repository:              releaserecovery.Repository,
			EventName:               "workflow_dispatch",
			Ref:                     "refs/heads/develop",
			RunID:                   mockRecoveryRunID,
			RunAttempt:              1,
			Actor:                   releaserecovery.OperatorLogin,
			ActorID:                 releaserecovery.OperatorID,
			ConfirmationTag:         releaserecovery.Tag,
			ConfirmationSHA:         releaserecovery.TagCommitSHA,
			RecoveryHeadSHA:         mockRecoveryHeadSHA,
			ConfirmationRecoverySHA: mockRecoveryHeadSHA,
			E2ERunID:                mockE2ERunID,
			ReleaseE2ERunID:         mockReleaseE2ERunID,
		},
		runs: map[int64]*mockWorkflowRun{
			mockRecoveryRunID: {
				ID: mockRecoveryRunID, Event: "workflow_dispatch", Status: "in_progress", HeadBranch: "develop", HeadSHA: mockRecoveryHeadSHA, Path: ".github/workflows/release-recovery.yml", RunAttempt: 1, Actor: operator, TriggeringActor: operator, CreatedAt: now.Add(-time.Minute),
			},
			releaserecovery.OriginalRunID: {
				ID: releaserecovery.OriginalRunID, WorkflowID: releaserecovery.ReleaseWorkflowID, Event: "push", Status: "completed", Conclusion: "startup_failure", HeadBranch: releaserecovery.Tag, HeadSHA: releaserecovery.TagCommitSHA, Path: ".github/workflows/release.yml", RunAttempt: 1, Actor: bot, TriggeringActor: bot, CreatedAt: time.Date(2026, time.August, 4, 20, 12, 59, 0, time.UTC),
			},
			mockE2ERunID:        gateRun(mockE2ERunID, releaserecovery.E2EWorkflowID, ".github/workflows/e2e.yml", operator, now),
			mockReleaseE2ERunID: gateRun(mockReleaseE2ERunID, releaserecovery.ReleaseE2EID, ".github/workflows/release-e2e.yml", operator, now),
		},
		jobs: map[int64]*mockWorkflowJobs{
			releaserecovery.OriginalRunID: {TotalCount: 0, Jobs: []mockWorkflowJob{}},
			mockE2ERunID:                  successfulJob("full-stack push e2e (podman)", []string{"Verify schema module pin parity", "make e2e", "Assert asserted e2e tests ran and passed"}),
			mockReleaseE2ERunID:           successfulJob("release e2e (installed packages)", []string{"Verify schema module pin parity", "Assert real dashboard artifact", "Build release snapshot artifacts", "Run release per-distro e2e driver"}),
		},
	}
	state.recoveryDispatches = []*mockWorkflowRun{state.runs[mockRecoveryRunID]}
	return state
}

func gateRun(id, workflowID int64, path string, actor mockUser, now time.Time) *mockWorkflowRun {
	return &mockWorkflowRun{
		ID: id, WorkflowID: workflowID, Event: "workflow_dispatch", Status: "completed", Conclusion: "success", HeadBranch: releaserecovery.Tag, HeadSHA: releaserecovery.TagCommitSHA, Path: path, RunAttempt: 1, Actor: actor, TriggeringActor: actor, CreatedAt: now.Add(-10 * time.Minute),
	}
}

func successfulJob(name string, steps []string) *mockWorkflowJobs {
	job := mockWorkflowJob{Name: name, Status: "completed", Conclusion: "success", RunAttempt: 1}
	for _, step := range steps {
		job.Steps = append(job.Steps, mockWorkflowStep{Name: step, Status: "completed", Conclusion: "success"})
	}
	return &mockWorkflowJobs{TotalCount: 1, Jobs: []mockWorkflowJob{job}}
}

func applyRecoveryMutation(t *testing.T, state *recoveryAPIState, mutation recoveryMutation, gate recoveryGate) {
	t.Helper()
	gateRunID := mockE2ERunID
	if gate == recoveryGateReleaseE2E {
		gateRunID = mockReleaseE2ERunID
	}
	switch mutation {
	case recoveryMutationNone:
	case recoveryMutationWrongRepositoryID:
		state.repositoryID++
	case recoveryMutationWrongDispatchActor:
		state.input.Actor = "someone-else"
	case recoveryMutationMismatchedRecoveryHead:
		state.input.RecoveryHeadSHA = strings.Repeat("2", 40)
	case recoveryMutationRecoveryRunHeadDrift:
		state.runs[mockRecoveryRunID].HeadSHA = strings.Repeat("2", 40)
	case recoveryMutationMissingRecoveryConfirm:
		state.input.ConfirmationRecoverySHA = ""
	case recoveryMutationPriorRecoveryDispatch:
		state.recoveryDispatches = append(state.recoveryDispatches, &mockWorkflowRun{
			ID: 91004, Event: "workflow_dispatch", Status: "completed", Conclusion: "cancelled", HeadBranch: "develop", HeadSHA: mockRecoveryHeadSHA, Path: ".github/workflows/release-recovery.yml", RunAttempt: 1, Actor: state.runs[mockRecoveryRunID].Actor, TriggeringActor: state.runs[mockRecoveryRunID].TriggeringActor, CreatedAt: state.now.Add(-2 * time.Minute),
		})
	case recoveryMutationStaleOperatorPermission:
		state.permission = "write"
	case recoveryMutationSameNamedBranch:
		state.branchExists = true
	case recoveryMutationMovedTagObject:
		state.tagObjectSHA = strings.Repeat("a", 40)
	case recoveryMutationChangedRuleset:
		state.rulesetAppID++
	case recoveryMutationWrongOriginalWorkflow:
		state.runs[releaserecovery.OriginalRunID].WorkflowID++
	case recoveryMutationOriginalHasJob:
		state.jobs[releaserecovery.OriginalRunID] = successfulJob("unexpected original job", []string{"unexpected"})
	case recoveryMutationWrongGateSHA:
		state.runs[gateRunID].HeadSHA = strings.Repeat("b", 40)
	case recoveryMutationWrongGateActor:
		state.runs[gateRunID].Actor = mockUser{Login: "someone-else", ID: 1}
	case recoveryMutationWrongGateWorkflow:
		state.runs[gateRunID].WorkflowID++
	case recoveryMutationSecondGateAttempt:
		state.runs[gateRunID].RunAttempt = 2
	case recoveryMutationZeroGateJobs:
		state.jobs[gateRunID] = &mockWorkflowJobs{TotalCount: 0, Jobs: []mockWorkflowJob{}}
	case recoveryMutationFailedCriticalStep:
		lastStep := len(state.jobs[gateRunID].Jobs[0].Steps) - 1
		state.jobs[gateRunID].Jobs[0].Steps[lastStep].Conclusion = "failure"
	case recoveryMutationStaleGate:
		state.runs[gateRunID].CreatedAt = state.now.Add(-25 * time.Hour)
	case recoveryMutationExpiredRecovery:
		state.now = time.Date(2026, time.August, 8, 0, 0, 0, 0, time.UTC)
	case recoveryMutationDraftRelease:
		state.releases = []mockRelease{{ID: 1, TagName: releaserecovery.Tag, Draft: true}}
	case recoveryMutationPartialRelease:
		state.releases = []mockRelease{{ID: 2, TagName: releaserecovery.Tag, Draft: true, Assets: []mockAsset{{ID: 3}}}}
	case recoveryMutationPublishedReleaseEndpoint:
		state.releaseEndpointExists = true
	case recoveryMutationFullReleasePage:
		state.releases = make([]mockRelease, 100)
		for index := range state.releases {
			state.releases[index] = mockRelease{ID: int64(index + 1), TagName: fmt.Sprintf("v0.0.%d", index)}
		}
	default:
		t.Fatalf("release recovery: unsupported API mutation %q", mutation)
	}
}

func (state *recoveryAPIState) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet || request.Header.Get("Authorization") != "Bearer fixture-token" || request.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
		writer.WriteHeader(http.StatusUnauthorized)
		return
	}
	base := "/repos/" + releaserecovery.Repository
	switch request.URL.Path {
	case base:
		writeRecoveryJSON(writer, http.StatusOK, map[string]any{"id": state.repositoryID, "full_name": releaserecovery.Repository, "default_branch": "develop"})
	case base + "/collaborators/" + releaserecovery.OperatorLogin + "/permission":
		writeRecoveryJSON(writer, http.StatusOK, map[string]any{"permission": state.permission, "user": mockUser{Login: releaserecovery.OperatorLogin, ID: releaserecovery.OperatorID}})
	case base + "/git/ref/heads/" + releaserecovery.Tag:
		if state.branchExists {
			writeRecoveryJSON(writer, http.StatusOK, map[string]any{"ref": "refs/heads/" + releaserecovery.Tag})
			return
		}
		writeRecoveryJSON(writer, http.StatusNotFound, map[string]any{"message": "Not Found"})
	case base + "/git/ref/tags/" + releaserecovery.Tag:
		writeRecoveryJSON(writer, http.StatusOK, map[string]any{"ref": releaserecovery.TagRef, "object": map[string]any{"type": "tag", "sha": state.tagObjectSHA}})
	case base + "/git/tags/" + releaserecovery.TagObjectSHA:
		writeRecoveryJSON(writer, http.StatusOK, map[string]any{
			"tag":    releaserecovery.Tag,
			"object": map[string]any{"type": "commit", "sha": releaserecovery.TagCommitSHA},
			"tagger": map[string]any{"name": "peasant-release-bot", "email": "noreply@peasantlabs.org", "date": "2026-08-04T20:12:49Z"},
		})
	case base + "/compare/" + releaserecovery.TagCommitSHA + "...develop":
		writeRecoveryJSON(writer, http.StatusOK, map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": releaserecovery.TagCommitSHA}})
	case fmt.Sprintf("%s/rulesets/%d", base, releaserecovery.TagRulesetID):
		writeRecoveryJSON(writer, http.StatusOK, map[string]any{
			"id": releaserecovery.TagRulesetID, "name": "Protect release tags", "target": "tag", "source_type": "Repository", "source": releaserecovery.Repository, "enforcement": "active",
			"conditions":              map[string]any{"ref_name": map[string]any{"exclude": []string{}, "include": []string{"refs/tags/v*"}}},
			"rules":                   []map[string]any{{"type": "creation"}, {"type": "update"}, {"type": "deletion"}, {"type": "non_fast_forward"}},
			"bypass_actors":           []map[string]any{{"actor_id": state.rulesetAppID, "actor_type": "Integration", "bypass_mode": "always"}},
			"current_user_can_bypass": "never",
		})
	case base + "/actions/workflows/release-recovery.yml/runs":
		writeRecoveryJSON(writer, http.StatusOK, map[string]any{"total_count": len(state.recoveryDispatches), "workflow_runs": state.recoveryDispatches})
	case base + "/releases":
		writeRecoveryJSON(writer, http.StatusOK, state.releases)
	case base + "/releases/tags/" + releaserecovery.Tag:
		if state.releaseEndpointExists {
			writeRecoveryJSON(writer, http.StatusOK, mockRelease{ID: 4, TagName: releaserecovery.Tag})
			return
		}
		writeRecoveryJSON(writer, http.StatusNotFound, map[string]any{"message": "Not Found"})
	default:
		for runID, run := range state.runs {
			if request.URL.Path == fmt.Sprintf("%s/actions/runs/%d", base, runID) {
				writeRecoveryJSON(writer, http.StatusOK, run)
				return
			}
		}
		for runID, jobs := range state.jobs {
			if request.URL.Path == fmt.Sprintf("%s/actions/runs/%d/jobs", base, runID) {
				writeRecoveryJSON(writer, http.StatusOK, jobs)
				return
			}
		}
		writeRecoveryJSON(writer, http.StatusNotFound, map[string]any{"message": "fixture route missing", "path": request.URL.Path})
	}
}

func writeRecoveryJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func assertRecoveryError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("release recovery: unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("release recovery: error = %v, want substring %q", err, want)
	}
}

func assertYAMLMappingKeysExact(t *testing.T, node *yaml.Node, want []string, context string) {
	t.Helper()
	if node == nil || node.Kind != yaml.MappingNode {
		t.Fatalf("%s: expected mapping, got %v", context, node)
	}
	got := make([]string, 0, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		got = append(got, node.Content[index].Value)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s keys = %v, want exact %v", context, got, want)
	}
}

func assertYAMLStringSet(t *testing.T, node *yaml.Node, want []string, context string) {
	t.Helper()
	var got []string
	if node != nil && node.Kind == yaml.ScalarNode {
		got = []string{node.Value}
	} else if node != nil && node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			got = append(got, item.Value)
		}
	} else {
		t.Fatalf("%s: expected scalar or sequence, got %v", context, node)
	}
	slices.Sort(got)
	wantCopy := append([]string(nil), want...)
	slices.Sort(wantCopy)
	if !slices.Equal(got, wantCopy) {
		t.Fatalf("%s = %v, want %v", context, got, wantCopy)
	}
}

func assertYAMLScalar(t *testing.T, node *yaml.Node, want, context string) {
	t.Helper()
	if node == nil || node.Kind != yaml.ScalarNode || node.Value != want {
		t.Fatalf("%s = %v, want scalar %q", context, node, want)
	}
}

func assertRecoveryActionsPinned(t *testing.T, jobs *yaml.Node) {
	t.Helper()
	pinned := regexp.MustCompile(`^[^@]+@[0-9a-f]{40}$`)
	for index := 0; index+1 < len(jobs.Content); index += 2 {
		jobName := jobs.Content[index].Value
		steps := yamlMappingValue(jobs.Content[index+1], "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			t.Fatalf("release recovery job %q must define steps", jobName)
		}
		for _, step := range steps.Content {
			uses := yamlMappingValue(step, "uses")
			if uses != nil && !pinned.MatchString(uses.Value) {
				t.Fatalf("release recovery job %q uses unpinned action %q", jobName, uses.Value)
			}
		}
	}
}

func replaceNth(t *testing.T, source, old, replacement string, occurrence int) string {
	t.Helper()
	if occurrence <= 0 {
		t.Fatalf("release recovery: replacement occurrence must be positive")
	}
	start := 0
	for current := 1; current <= occurrence; current++ {
		index := strings.Index(source[start:], old)
		if index < 0 {
			t.Fatalf("release recovery: could not find occurrence %d of %q", occurrence, old)
		}
		index += start
		if current == occurrence {
			return source[:index] + replacement + source[index+len(old):]
		}
		start = index + len(old)
	}
	return source
}
