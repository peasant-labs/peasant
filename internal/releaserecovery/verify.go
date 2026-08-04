package releaserecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	Repository        = "peasant-labs/peasant"
	RepositoryID      = int64(1321556788)
	Tag               = "v0.1.0"
	TagRef            = "refs/tags/v0.1.0"
	TagObjectSHA      = "b1f8fe4b9a40ac32c1d7e1a8748cb11575595c4f"
	TagCommitSHA      = "807a1b68c8ec1952db6c289f383f42cbb0701db9"
	OriginalRunID     = int64(30946834984)
	ReleaseWorkflowID = int64(326590528)
	E2EWorkflowID     = int64(326590523)
	ReleaseE2EID      = int64(326590525)
	TagRulesetID      = int64(20343024)
	ReleaserAppID     = int64(3988034)
	BotLogin          = "peasant-labs-releaser[bot]"
	BotID             = int64(291504229)
	OperatorLogin     = "dayvidpham"
	OperatorID        = int64(22456875)

	recoveryWorkflowPath = ".github/workflows/release-recovery.yml"
	releaseWorkflowPath  = ".github/workflows/release.yml"
	e2eWorkflowPath      = ".github/workflows/e2e.yml"
	releaseE2EPath       = ".github/workflows/release-e2e.yml"
	maxEvidenceAge       = 24 * time.Hour
	maxResponseBytes     = 4 << 20
)

var (
	recoveryNotBefore = time.Date(2026, time.August, 4, 20, 40, 42, 0, time.UTC)
	recoveryExpires   = time.Date(2026, time.August, 8, 0, 0, 0, 0, time.UTC)
	fullCommitSHA     = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type PreflightInput struct {
	Repository              string
	EventName               string
	Ref                     string
	RunID                   int64
	RunAttempt              int
	Actor                   string
	ActorID                 int64
	ConfirmationTag         string
	ConfirmationSHA         string
	RecoveryHeadSHA         string
	ConfirmationRecoverySHA string
	E2ERunID                int64
	ReleaseE2ERunID         int64
}

type RecoveryRunInput struct {
	RunID   int64
	HeadSHA string
}

type Config struct {
	APIURL     string
	Token      string
	HTTPClient *http.Client
	Now        func() time.Time
}

type Verifier struct {
	apiURL string
	token  string
	http   *http.Client
	now    func() time.Time
}

func NewVerifier(config Config) (*Verifier, error) {
	parsed, err := url.Parse(config.APIURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, failure("initializing the release-recovery verifier", "the GitHub API URL is invalid", fmt.Sprintf("GITHUB_API_URL=%q is not an absolute URL", config.APIURL), "no recovery evidence can be trusted", "set GITHUB_API_URL to the GitHub Actions API URL")
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, failure("initializing the release-recovery verifier", "the GitHub token is missing", "GH_TOKEN was empty", "GitHub evidence cannot be read", "grant the job only the documented read permission and pass github.token as GH_TOKEN")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &Verifier{
		apiURL: strings.TrimRight(config.APIURL, "/"),
		token:  config.Token,
		http:   client,
		now:    now,
	}, nil
}

func (v *Verifier) VerifyPreflight(ctx context.Context, input PreflightInput) error {
	now := v.now().UTC()
	if now.Before(recoveryNotBefore) || !now.Before(recoveryExpires) {
		return failure("checking the one-time recovery window", "the recovery workflow is outside its approved time window", fmt.Sprintf("current time %s must be on or after %s and before %s", now.Format(time.RFC3339), recoveryNotBefore.Format(time.RFC3339), recoveryExpires.Format(time.RFC3339)), "publication is blocked before any release mutation", "review the incident and land a new time-bounded recovery rather than extending this run")
	}
	if input.Repository != Repository || input.EventName != "workflow_dispatch" || input.Ref != "refs/heads/develop" || input.RunAttempt != 1 {
		return failure("validating the recovery invocation", "the workflow was not a first-attempt dispatch from protected develop", fmt.Sprintf("repository=%q event=%q ref=%q attempt=%d", input.Repository, input.EventName, input.Ref, input.RunAttempt), "the operator or source revision is ambiguous, so publication is blocked", "dispatch the reviewed workflow from develop exactly once")
	}
	if input.Actor != OperatorLogin || input.ActorID != OperatorID {
		return failure("validating the recovery operator", "the dispatcher is not the fixed recovery operator", fmt.Sprintf("actor=%q actor_id=%d", input.Actor, input.ActorID), "an unauthorized actor cannot publish the release", "have the approved maintainer dispatch the reviewed recovery workflow")
	}
	if input.ConfirmationTag != Tag || input.ConfirmationSHA != TagCommitSHA {
		return failure("validating explicit recovery confirmation", "the typed tag and commit confirmation do not match the immutable release", fmt.Sprintf("tag=%q commit=%q", input.ConfirmationTag, input.ConfirmationSHA), "the workflow may be targeting unintended source", fmt.Sprintf("confirm exactly %s and %s", Tag, TagCommitSHA))
	}
	if !fullCommitSHA.MatchString(input.RecoveryHeadSHA) || input.RecoveryHeadSHA != input.ConfirmationRecoverySHA {
		return failure("binding the reviewed recovery source", "the executing recovery commit is missing, malformed, or differs from the explicit confirmation", fmt.Sprintf("head_sha=%q confirmed_sha=%q", input.RecoveryHeadSHA, input.ConfirmationRecoverySHA), "the workflow and verifier may not be the revision the operator reviewed", "confirm the full 40-character merge commit currently executing from protected develop")
	}
	if input.RunID <= 0 || input.E2ERunID <= 0 || input.ReleaseE2ERunID <= 0 || input.E2ERunID == input.ReleaseE2ERunID || input.RunID == input.E2ERunID || input.RunID == input.ReleaseE2ERunID {
		return failure("validating recovery run receipts", "one or more run IDs are missing, repeated, or refer to the recovery itself", fmt.Sprintf("recovery=%d e2e=%d release_e2e=%d", input.RunID, input.E2ERunID, input.ReleaseE2ERunID), "independent gate evidence is not established", "pass the two distinct run IDs returned by exact-tag workflow dispatches")
	}

	if err := v.verifyRepository(ctx); err != nil {
		return err
	}
	if err := v.verifyCurrentRun(ctx, input); err != nil {
		return err
	}
	if err := v.verifyOnlyRecoveryDispatch(ctx, RecoveryRunInput{RunID: input.RunID, HeadSHA: input.RecoveryHeadSHA}); err != nil {
		return err
	}
	if err := v.verifyOperatorPermission(ctx); err != nil {
		return err
	}
	if err := v.VerifyImmutableTag(ctx); err != nil {
		return err
	}
	if err := v.verifyRuleset(ctx); err != nil {
		return err
	}
	if err := v.verifyOriginalFailure(ctx); err != nil {
		return err
	}
	if err := v.verifyGateRun(ctx, gateExpectation{
		ID: input.E2ERunID, WorkflowID: E2EWorkflowID, Path: e2eWorkflowPath,
		Job:           "full-stack push e2e (podman)",
		CriticalSteps: []string{"Verify schema module pin parity", "make e2e", "Assert asserted e2e tests ran and passed"},
	}); err != nil {
		return err
	}
	if err := v.verifyGateRun(ctx, gateExpectation{
		ID: input.ReleaseE2ERunID, WorkflowID: ReleaseE2EID, Path: releaseE2EPath,
		Job:           "release e2e (installed packages)",
		CriticalSteps: []string{"Verify schema module pin parity", "Assert real dashboard artifact", "Build release snapshot artifacts", "Run release per-distro e2e driver"},
	}); err != nil {
		return err
	}
	return v.VerifyReleaseAbsent(ctx)
}

func (v *Verifier) VerifyPrePublish(ctx context.Context, input RecoveryRunInput) error {
	now := v.now().UTC()
	if now.Before(recoveryNotBefore) || !now.Before(recoveryExpires) {
		return failure("checking the pre-publication recovery window", "the recovery authorization expired before publication", now.Format(time.RFC3339), "the write-capable job is blocked before mutation", "land a newly reviewed recovery rather than extending or redispatching this one")
	}
	if input.RunID <= 0 || !fullCommitSHA.MatchString(input.HeadSHA) {
		return failure("binding the pre-publication recovery run", "the run ID or executing commit is invalid", fmt.Sprintf("run_id=%d head_sha=%q", input.RunID, input.HeadSHA), "the write job cannot be tied to reviewed orchestration", "pass github.run_id and github.sha from the same first-attempt recovery run")
	}
	if err := v.verifyRepository(ctx); err != nil {
		return err
	}
	if err := v.verifyOnlyRecoveryDispatch(ctx, input); err != nil {
		return err
	}
	if err := v.VerifyImmutableTag(ctx); err != nil {
		return err
	}
	if err := v.verifyRuleset(ctx); err != nil {
		return err
	}
	return v.VerifyReleaseAbsent(ctx)
}

func (v *Verifier) VerifyReleaseAbsent(ctx context.Context) error {
	var releases []githubRelease
	status, err := v.get(ctx, "/repos/"+Repository+"/releases?per_page=100", &releases)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return unexpectedStatus("listing releases before publication", status, http.StatusOK)
	}
	if len(releases) == 100 {
		return failure("listing releases before publication", "the first release page is full", "the verifier cannot prove that a matching draft is absent without pagination", "automatic publication could collide with hidden state", "inspect all releases and revise the verifier through review")
	}
	for _, release := range releases {
		if release.TagName == Tag {
			return failure("checking release absence before publication", "a release record already exists for the immutable tag", fmt.Sprintf("release_id=%d draft=%t assets=%d", release.ID, release.Draft, len(release.Assets)), "automation will not overwrite, append to, or delete existing release state", "inspect the existing release and use read-only diagnosis; do not redispatch publication")
		}
	}

	status, err = v.get(ctx, "/repos/"+Repository+"/releases/tags/"+Tag, nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status == http.StatusOK {
		return failure("checking the public release endpoint", "GitHub reports an existing release for the immutable tag", "the tag lookup returned HTTP 200", "duplicate publication is blocked", "inspect the existing release and do not rerun the publishing job")
	}
	return unexpectedStatus("checking the public release endpoint", status, http.StatusNotFound)
}

func VerifyPublishersDisabled(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return failure("reading the tagged GoReleaser configuration", "the publisher configuration could not be read", err.Error(), "external publisher state cannot be proven disabled", "restore the exact tagged .goreleaser.yml and rerun preflight")
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || len(root.Content) != 1 {
		return failure("parsing the tagged GoReleaser configuration", "the publisher configuration is invalid YAML", fmt.Sprint(err), "AUR and Homebrew safety cannot be established", "repair configuration on a later release; never alter the immutable tag")
	}
	for _, key := range []string{"aurs", "homebrew_casks"} {
		publishers := yamlMapValue(root.Content[0], key)
		if publishers == nil || publishers.Kind != yaml.SequenceNode || len(publishers.Content) != 1 {
			return failure("checking tagged external publishers", fmt.Sprintf("%s does not contain exactly one publisher", key), "the one-time recovery only recognizes the reviewed v0.1.0 publisher shape", "external publication might not remain disabled", "stop recovery and inspect the immutable tag configuration")
		}
		skip := yamlMapValue(publishers.Content[0], "skip_upload")
		if skip == nil || skip.Kind != yaml.ScalarNode || skip.Tag != "!!str" || skip.Value != "true" {
			return failure("checking tagged external publishers", fmt.Sprintf("%s skip_upload is not the exact string true", key), fmt.Sprintf("found node %s", describeYAMLNode(skip)), "the recovery could publish outside GitHub Releases", "keep AUR and Homebrew skip_upload set to the quoted string true")
		}
	}
	return nil
}

func (v *Verifier) verifyRepository(ctx context.Context) error {
	var repository struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	}
	status, err := v.get(ctx, "/repos/"+Repository, &repository)
	if err != nil {
		return err
	}
	if status != http.StatusOK || repository.ID != RepositoryID || repository.FullName != Repository || repository.DefaultBranch != "develop" {
		return failure("verifying the recovery repository", "repository identity or default branch differs from the reviewed constants", fmt.Sprintf("status=%d id=%d full_name=%q default_branch=%q", status, repository.ID, repository.FullName, repository.DefaultBranch), "the workflow may be running against the wrong repository", "run only the reviewed workflow in peasant-labs/peasant with develop as default")
	}
	return nil
}

func (v *Verifier) verifyCurrentRun(ctx context.Context, input PreflightInput) error {
	run, status, err := v.workflowRun(ctx, input.RunID)
	if err != nil {
		return err
	}
	if status != http.StatusOK || run.ID != input.RunID || run.Event != "workflow_dispatch" || run.HeadBranch != "develop" || run.HeadSHA != input.RecoveryHeadSHA || run.Path != recoveryWorkflowPath || run.RunAttempt != 1 || run.Status != "in_progress" || run.Actor.Login != OperatorLogin || run.Actor.ID != OperatorID || run.TriggeringActor.Login != OperatorLogin || run.TriggeringActor.ID != OperatorID {
		return failure("verifying the active recovery run", "GitHub run metadata differs from the fixed first-attempt develop dispatch", fmt.Sprintf("status=%d run=%+v", status, run), "the current run is not authorized to publish", "cancel this run and dispatch the reviewed workflow from develop as the approved operator")
	}
	if run.CreatedAt.Before(recoveryNotBefore) || !run.CreatedAt.Before(recoveryExpires) {
		return failure("verifying the active recovery run timestamp", "the run was created outside the approved recovery window", run.CreatedAt.Format(time.RFC3339), "the one-time authorization has expired", "land a newly reviewed recovery rather than rerunning this workflow")
	}
	return nil
}

func (v *Verifier) verifyOnlyRecoveryDispatch(ctx context.Context, input RecoveryRunInput) error {
	var runs struct {
		TotalCount   int           `json:"total_count"`
		WorkflowRuns []workflowRun `json:"workflow_runs"`
	}
	status, err := v.get(ctx, "/repos/"+Repository+"/actions/workflows/release-recovery.yml/runs?event=workflow_dispatch&per_page=100", &runs)
	if err != nil {
		return err
	}
	if status != http.StatusOK || runs.TotalCount != 1 || len(runs.WorkflowRuns) != 1 {
		return failure("checking one-time recovery dispatch uniqueness", "the current run is not the only recovery workflow dispatch", fmt.Sprintf("status=%d total_count=%d returned_runs=%d current_run=%d", status, runs.TotalCount, len(runs.WorkflowRuns), input.RunID), "a redispatch or concurrent attempt could duplicate irreversible publication", "cancel all recovery runs and require a newly reviewed recovery change before any second dispatch")
	}
	run := runs.WorkflowRuns[0]
	if run.ID != input.RunID || run.Event != "workflow_dispatch" || run.HeadBranch != "develop" || run.HeadSHA != input.HeadSHA || run.Path != recoveryWorkflowPath || run.RunAttempt != 1 || run.Status != "in_progress" || run.Actor.Login != OperatorLogin || run.Actor.ID != OperatorID || run.TriggeringActor.Login != OperatorLogin || run.TriggeringActor.ID != OperatorID || run.CreatedAt.Before(recoveryNotBefore) || !run.CreatedAt.Before(recoveryExpires) {
		return failure("checking one-time recovery dispatch identity", "the sole recovery dispatch differs from the active reviewed run", fmt.Sprintf("run=%+v expected_id=%d expected_sha=%q", run, input.RunID, input.HeadSHA), "the write path cannot prove it is the one authorized attempt", "cancel the run and review the workflow history before proceeding")
	}
	return nil
}

func (v *Verifier) verifyOperatorPermission(ctx context.Context) error {
	var permission struct {
		Permission string     `json:"permission"`
		User       githubUser `json:"user"`
	}
	status, err := v.get(ctx, "/repos/"+Repository+"/collaborators/"+OperatorLogin+"/permission", &permission)
	if err != nil {
		return err
	}
	if status != http.StatusOK || permission.User.Login != OperatorLogin || permission.User.ID != OperatorID || (permission.Permission != "admin" && permission.Permission != "maintain") {
		return failure("checking current recovery-operator permission", "the fixed operator no longer has admin or maintain permission", fmt.Sprintf("status=%d permission=%q user=%+v", status, permission.Permission, permission.User), "a stale authorization cannot publish", "restore reviewed repository governance or appoint and review a new recovery operator")
	}
	return nil
}

func (v *Verifier) VerifyImmutableTag(ctx context.Context) error {
	status, err := v.get(ctx, "/repos/"+Repository+"/git/ref/heads/"+Tag, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return failure("checking for a same-named branch", "the v0.1.0 ref name is ambiguous", fmt.Sprintf("branch lookup returned HTTP %d", status), "head_branch evidence cannot be safely bound to the tag", "remove no refs automatically; inspect the repository and create a newly reviewed recovery")
	}

	var ref gitReference
	status, err = v.get(ctx, "/repos/"+Repository+"/git/ref/tags/"+Tag, &ref)
	if err != nil {
		return err
	}
	if status != http.StatusOK || ref.Ref != TagRef || ref.Object.Type != "tag" || ref.Object.SHA != TagObjectSHA {
		return failure("verifying the immutable tag reference", "the tag ref no longer points to the reviewed annotated tag object", fmt.Sprintf("status=%d ref=%q type=%q sha=%q", status, ref.Ref, ref.Object.Type, ref.Object.SHA), "publication source has changed or become ambiguous", "stop recovery; never move or recreate v0.1.0")
	}

	var tag struct {
		Tag    string `json:"tag"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
		Tagger struct {
			Name  string    `json:"name"`
			Email string    `json:"email"`
			Date  time.Time `json:"date"`
		} `json:"tagger"`
	}
	status, err = v.get(ctx, "/repos/"+Repository+"/git/tags/"+TagObjectSHA, &tag)
	if err != nil {
		return err
	}
	if status != http.StatusOK || tag.Tag != Tag || tag.Object.Type != "commit" || tag.Object.SHA != TagCommitSHA || tag.Tagger.Name != "peasant-release-bot" || tag.Tagger.Email != "noreply@peasantlabs.org" || !tag.Tagger.Date.Equal(time.Date(2026, time.August, 4, 20, 12, 49, 0, time.UTC)) {
		return failure("verifying the annotated tag object", "the tag payload, peeled commit, or tagger differs from the reviewed object", fmt.Sprintf("status=%d tag=%q object=%s:%s tagger=%s <%s> at %s", status, tag.Tag, tag.Object.Type, tag.Object.SHA, tag.Tagger.Name, tag.Tagger.Email, tag.Tagger.Date.Format(time.RFC3339)), "the exact release source is not proven", "stop recovery and preserve the existing tag for investigation")
	}

	var comparison struct {
		Status          string `json:"status"`
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	status, err = v.get(ctx, "/repos/"+Repository+"/compare/"+TagCommitSHA+"...develop", &comparison)
	if err != nil {
		return err
	}
	if status != http.StatusOK || (comparison.Status != "ahead" && comparison.Status != "identical") || comparison.MergeBaseCommit.SHA != TagCommitSHA {
		return failure("checking tag reachability from protected develop", "the immutable tag commit is no longer an ancestor of develop", fmt.Sprintf("status=%d comparison=%q merge_base=%q", status, comparison.Status, comparison.MergeBaseCommit.SHA), "the protected-branch provenance is broken", "stop recovery and inspect repository history without moving the tag")
	}
	return nil
}

func (v *Verifier) verifyRuleset(ctx context.Context) error {
	var ruleset struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Target      string `json:"target"`
		SourceType  string `json:"source_type"`
		Source      string `json:"source"`
		Enforcement string `json:"enforcement"`
		Conditions  struct {
			RefName struct {
				Exclude []string `json:"exclude"`
				Include []string `json:"include"`
			} `json:"ref_name"`
		} `json:"conditions"`
		Rules []struct {
			Type string `json:"type"`
		} `json:"rules"`
		BypassActors []struct {
			ActorID    int64  `json:"actor_id"`
			ActorType  string `json:"actor_type"`
			BypassMode string `json:"bypass_mode"`
		} `json:"bypass_actors"`
		CurrentUserCanBypass string `json:"current_user_can_bypass"`
	}
	status, err := v.get(ctx, fmt.Sprintf("/repos/%s/rulesets/%d", Repository, TagRulesetID), &ruleset)
	if err != nil {
		return err
	}
	ruleTypes := make([]string, 0, len(ruleset.Rules))
	for _, rule := range ruleset.Rules {
		ruleTypes = append(ruleTypes, rule.Type)
	}
	slices.Sort(ruleTypes)
	expectedRules := []string{"creation", "deletion", "non_fast_forward", "update"}
	validBypass := len(ruleset.BypassActors) == 1 && ruleset.BypassActors[0].ActorID == ReleaserAppID && ruleset.BypassActors[0].ActorType == "Integration" && ruleset.BypassActors[0].BypassMode == "always"
	if status != http.StatusOK || ruleset.ID != TagRulesetID || ruleset.Name != "Protect release tags" || ruleset.Target != "tag" || ruleset.SourceType != "Repository" || ruleset.Source != Repository || ruleset.Enforcement != "active" || !slices.Equal(ruleset.Conditions.RefName.Include, []string{"refs/tags/v*"}) || len(ruleset.Conditions.RefName.Exclude) != 0 || !slices.Equal(ruleTypes, expectedRules) || !validBypass || ruleset.CurrentUserCanBypass != "never" {
		return failure("verifying release-tag protection", "the active tag ruleset differs from the reviewed App-only policy", fmt.Sprintf("status=%d id=%d target=%q source=%s:%s enforcement=%q include=%v exclude=%v rules=%v bypass=%v current_user_can_bypass=%q", status, ruleset.ID, ruleset.Target, ruleset.SourceType, ruleset.Source, ruleset.Enforcement, ruleset.Conditions.RefName.Include, ruleset.Conditions.RefName.Exclude, ruleTypes, ruleset.BypassActors, ruleset.CurrentUserCanBypass), "tag immutability or App provenance is no longer guaranteed", "restore the reviewed ruleset before creating a new recovery authorization")
	}
	return nil
}

func (v *Verifier) verifyOriginalFailure(ctx context.Context) error {
	run, status, err := v.workflowRun(ctx, OriginalRunID)
	if err != nil {
		return err
	}
	if status != http.StatusOK || run.ID != OriginalRunID || run.WorkflowID != ReleaseWorkflowID || run.Event != "push" || run.HeadBranch != Tag || run.HeadSHA != TagCommitSHA || run.Path != releaseWorkflowPath || run.RunAttempt != 1 || run.Status != "completed" || run.Conclusion != "startup_failure" || run.Actor.Login != BotLogin || run.Actor.ID != BotID || run.TriggeringActor.Login != BotLogin || run.TriggeringActor.ID != BotID {
		return failure("verifying the original tag-triggered failure", "the recorded failed run differs from the reviewed App-created startup failure", fmt.Sprintf("status=%d run=%+v", status, run), "the exceptional recovery premise is not proven", "stop and review the original run before authorizing publication")
	}
	var jobs workflowJobs
	status, err = v.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest", Repository, OriginalRunID), &jobs)
	if err != nil {
		return err
	}
	if status != http.StatusOK || jobs.TotalCount != 0 || len(jobs.Jobs) != 0 {
		return failure("verifying the original startup failure had no jobs", "the original run contains executable job records", fmt.Sprintf("status=%d total_count=%d jobs=%d", status, jobs.TotalCount, len(jobs.Jobs)), "recovery could duplicate partial release work", "inspect the original run and do not publish automatically")
	}
	return nil
}

type gateExpectation struct {
	ID            int64
	WorkflowID    int64
	Path          string
	Job           string
	CriticalSteps []string
}

func (v *Verifier) verifyGateRun(ctx context.Context, expected gateExpectation) error {
	run, status, err := v.workflowRun(ctx, expected.ID)
	if err != nil {
		return err
	}
	now := v.now().UTC()
	age := now.Sub(run.CreatedAt)
	if status != http.StatusOK || run.ID != expected.ID || run.WorkflowID != expected.WorkflowID || run.Event != "workflow_dispatch" || run.HeadBranch != Tag || run.HeadSHA != TagCommitSHA || run.Path != expected.Path || run.RunAttempt != 1 || run.Status != "completed" || run.Conclusion != "success" || run.Actor.Login != OperatorLogin || run.Actor.ID != OperatorID || run.TriggeringActor.Login != OperatorLogin || run.TriggeringActor.ID != OperatorID || age < 0 || age > maxEvidenceAge {
		return failure("verifying exact-tag gate evidence", "a supplied gate run is stale or differs from the required workflow, source, actor, attempt, or conclusion", fmt.Sprintf("expected_workflow=%d expected_path=%q age=%s status=%d run=%+v", expected.WorkflowID, expected.Path, age, status, run), "the immutable release has not passed a fresh independent gate", "dispatch the named workflow at short ref v0.1.0 and pass the returned first-attempt run ID")
	}
	var jobs workflowJobs
	status, err = v.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs?filter=latest", Repository, expected.ID), &jobs)
	if err != nil {
		return err
	}
	if status != http.StatusOK || jobs.TotalCount != 1 || len(jobs.Jobs) != 1 || jobs.Jobs[0].Name != expected.Job || jobs.Jobs[0].Status != "completed" || jobs.Jobs[0].Conclusion != "success" || jobs.Jobs[0].RunAttempt != 1 {
		return failure("verifying exact-tag gate jobs", "the gate did not produce exactly one successful first-attempt production job", fmt.Sprintf("status=%d expected_job=%q jobs=%+v", status, expected.Job, jobs), "workflow-level success is not backed by the expected job", "rerun the exact workflow at v0.1.0 and inspect all job results")
	}
	steps := make(map[string]workflowStep, len(jobs.Jobs[0].Steps))
	for _, step := range jobs.Jobs[0].Steps {
		steps[step.Name] = step
	}
	for _, name := range expected.CriticalSteps {
		step, ok := steps[name]
		if !ok || step.Status != "completed" || step.Conclusion != "success" {
			return failure("verifying exact-tag gate assertions", "a critical gate step is missing or did not succeed", fmt.Sprintf("run=%d step=%q found=%t status=%q conclusion=%q", expected.ID, name, ok, step.Status, step.Conclusion), "the gate can be vacuous or incomplete", "inspect the gate log and obtain a fresh run where every critical assertion passes")
		}
	}
	return nil
}

func (v *Verifier) workflowRun(ctx context.Context, id int64) (workflowRun, int, error) {
	var run workflowRun
	status, err := v.get(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d", Repository, id), &run)
	return run, status, err
}

func (v *Verifier) get(ctx context.Context, path string, target any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, v.apiURL+path, nil)
	if err != nil {
		return 0, failure("constructing a GitHub evidence request", "the request URL could not be created", err.Error(), "recovery evidence cannot be read", "check the reviewed API endpoint and rerun")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+v.token)
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := v.http.Do(request)
	if err != nil {
		return 0, failure("requesting GitHub recovery evidence", "the GitHub API request failed", err.Error(), "publication is blocked because remote state is unknown", "check GitHub availability and rerun before the fixed expiry")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return 0, failure("reading GitHub recovery evidence", "the API response could not be read", err.Error(), "remote state is incomplete", "retry the first-attempt recovery only if no release was created")
	}
	if len(body) > maxResponseBytes {
		return 0, failure("reading GitHub recovery evidence", "the API response exceeded the verifier limit", fmt.Sprintf("response was larger than %d bytes", maxResponseBytes), "the response cannot be safely validated", "inspect the endpoint and narrow the reviewed query")
	}
	if target != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		if err := json.Unmarshal(body, target); err != nil {
			return 0, failure("decoding GitHub recovery evidence", "the API response did not match the expected schema", err.Error(), "the verifier cannot establish exact state", "inspect the GitHub response and update the verifier through review")
		}
	}
	return response.StatusCode, nil
}

type githubUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type workflowRun struct {
	ID              int64      `json:"id"`
	WorkflowID      int64      `json:"workflow_id"`
	Event           string     `json:"event"`
	Status          string     `json:"status"`
	Conclusion      string     `json:"conclusion"`
	HeadBranch      string     `json:"head_branch"`
	HeadSHA         string     `json:"head_sha"`
	Path            string     `json:"path"`
	RunAttempt      int        `json:"run_attempt"`
	Actor           githubUser `json:"actor"`
	TriggeringActor githubUser `json:"triggering_actor"`
	CreatedAt       time.Time  `json:"created_at"`
}

type workflowJobs struct {
	TotalCount int           `json:"total_count"`
	Jobs       []workflowJob `json:"jobs"`
}

type workflowJob struct {
	Name       string         `json:"name"`
	Status     string         `json:"status"`
	Conclusion string         `json:"conclusion"`
	RunAttempt int            `json:"run_attempt"`
	Steps      []workflowStep `json:"steps"`
}

type workflowStep struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type gitReference struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

type githubRelease struct {
	ID      int64  `json:"id"`
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		ID int64 `json:"id"`
	} `json:"assets"`
}

func yamlMapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func describeYAMLNode(node *yaml.Node) string {
	if node == nil {
		return "<missing>"
	}
	return fmt.Sprintf("kind=%d tag=%q value=%q", node.Kind, node.Tag, node.Value)
}

func unexpectedStatus(operation string, got, want int) error {
	return failure(operation, "GitHub returned an unexpected status", fmt.Sprintf("got HTTP %d, want HTTP %d", got, want), "remote state is ambiguous and publication is blocked", "inspect GitHub state and rerun only if no release mutation occurred")
}

func failure(operation, what, why, impact, fix string) error {
	return errors.New(operation + ": " + what + "; why: " + why + "; impact: " + impact + "; fix: " + fix)
}
