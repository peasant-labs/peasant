package e2e

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/workflows/e2e_contract.yaml
var e2eWorkflowContractFixtureBytes []byte

type e2eWorkflowContractFixture struct {
	ExpectedVillageRef string `yaml:"expected_village_ref"`
	E2E                struct {
		RequiredTriggers         nonEmptyStrings          `yaml:"required_triggers"`
		RequiredPaths            nonEmptyStrings          `yaml:"required_paths"`
		ParityStep               string                   `yaml:"parity_step"`
		ParityRunContains        string                   `yaml:"parity_run_contains"`
		ParityEnv                workflowEnvExpectation   `yaml:"parity_env"`
		DriverStep               string                   `yaml:"driver_step"`
		DriverEnv                workflowEnvExpectation   `yaml:"driver_env"`
		DriverContains           nonEmptyStrings          `yaml:"driver_contains"`
		AssertedTests            nonEmptyStrings          `yaml:"asserted_tests"`
		CleanupRequiredStatuses  nonEmptyStrings          `yaml:"cleanup_required_statuses"`
		CleanupForbiddenStatuses []workflowForbiddenValue `yaml:"cleanup_forbidden_statuses"`
	} `yaml:"e2e"`
	Release struct {
		ParityStep        string                 `yaml:"parity_step"`
		ParityRunContains string                 `yaml:"parity_run_contains"`
		ParityEnv         workflowEnvExpectation `yaml:"parity_env"`
		DriverStep        string                 `yaml:"driver_step"`
		DriverEnv         workflowEnvExpectation `yaml:"driver_env"`
		DriverContains    nonEmptyStrings        `yaml:"driver_contains"`
		OutputGuards      nonEmptyStrings        `yaml:"output_guards"`
	} `yaml:"release"`
	ReleaseGuard struct {
		Workflow               string                   `yaml:"workflow"`
		Job                    string                   `yaml:"job"`
		JobIfCondition         string                   `yaml:"job_if_condition"`
		RequiredPRBranches     nonEmptyStrings          `yaml:"required_pr_branches"`
		RequiredPRPaths        nonEmptyStrings          `yaml:"required_pr_paths"`
		Step                   string                   `yaml:"step"`
		IfCondition            string                   `yaml:"if_condition"`
		InitialFinal           string                   `yaml:"initial_final"`
		ExpectedRun            string                   `yaml:"expected_run"`
		RequiredJobPermissions []workflowEnvExpectation `yaml:"required_job_permissions"`
		RequiredJobEnv         []workflowEnvExpectation `yaml:"required_job_env"`
		ActorStep              string                   `yaml:"actor_step"`
		ActorEnv               []workflowEnvExpectation `yaml:"actor_env"`
		ActorRun               string                   `yaml:"actor_run"`
		CheckoutStep           string                   `yaml:"checkout_step"`
		ParseStep              string                   `yaml:"parse_step"`
		CheckoutFetchDepth     string                   `yaml:"checkout_fetch_depth"`
		CheckoutFetchTags      string                   `yaml:"checkout_fetch_tags"`
	} `yaml:"release_guard"`
	ReusableCallers []reusableWorkflowCallerExpectation `yaml:"reusable_callers"`
	ReleaseValidate struct {
		Workflow      string          `yaml:"workflow"`
		RequiredPaths nonEmptyStrings `yaml:"required_paths"`
	} `yaml:"release_validate"`
	TestsWorkflow struct {
		Triggers      nonEmptyStrings `yaml:"triggers"`
		RequiredPaths nonEmptyStrings `yaml:"required_paths"`
	} `yaml:"tests_workflow"`
}

type nonEmptyStrings []string

func (values *nonEmptyStrings) UnmarshalYAML(node *yaml.Node) error {
	var decoded []string
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	for i, value := range decoded {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("list item %d must be non-empty", i)
		}
	}
	*values = decoded
	return nil
}

type workflowEnvExpectation struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
}

type workflowJobPermissionsExpectation struct {
	Job         string                   `yaml:"job"`
	Permissions []workflowEnvExpectation `yaml:"permissions"`
}

type reusableWorkflowCallerExpectation struct {
	Workflow string                              `yaml:"workflow"`
	Jobs     []workflowJobPermissionsExpectation `yaml:"jobs"`
}

func (expectation *workflowEnvExpectation) UnmarshalYAML(node *yaml.Node) error {
	type rawExpectation workflowEnvExpectation
	var decoded rawExpectation
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	if strings.TrimSpace(decoded.Key) == "" || strings.TrimSpace(decoded.Value) == "" {
		return fmt.Errorf("workflow environment expectation requires non-empty key and value")
	}
	*expectation = workflowEnvExpectation(decoded)
	return nil
}

type workflowForbiddenValue struct {
	Value  string `yaml:"value"`
	Reason string `yaml:"reason"`
}

func (forbidden *workflowForbiddenValue) UnmarshalYAML(node *yaml.Node) error {
	type rawForbiddenValue workflowForbiddenValue
	var decoded rawForbiddenValue
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	if strings.TrimSpace(decoded.Value) == "" || strings.TrimSpace(decoded.Reason) == "" {
		return fmt.Errorf("forbidden workflow value requires non-empty value and reason")
	}
	*forbidden = workflowForbiddenValue(decoded)
	return nil
}

func loadE2EWorkflowContractFixture(t *testing.T) e2eWorkflowContractFixture {
	t.Helper()
	var fixture e2eWorkflowContractFixture
	decoder := yaml.NewDecoder(bytes.NewReader(e2eWorkflowContractFixtureBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("e2e: parse workflow contract fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("e2e: workflow contract fixture must have exact EOF: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(fixture.ExpectedVillageRef) || len(fixture.E2E.RequiredTriggers) != 3 || len(fixture.E2E.RequiredPaths) != 9 ||
		fixture.E2E.ParityStep == "" || fixture.E2E.ParityRunContains == "" ||
		fixture.E2E.ParityEnv.Key == "" || fixture.E2E.ParityEnv.Value == "" ||
		fixture.E2E.DriverStep == "" || fixture.E2E.DriverEnv.Key == "" || fixture.E2E.DriverEnv.Value == "" ||
		len(fixture.E2E.DriverContains) != 3 || len(fixture.E2E.AssertedTests) != 6 ||
		len(fixture.E2E.CleanupRequiredStatuses) != 2 || len(fixture.E2E.CleanupForbiddenStatuses) != 3 ||
		fixture.Release.ParityStep == "" || fixture.Release.ParityRunContains == "" ||
		fixture.Release.ParityEnv.Key == "" || fixture.Release.ParityEnv.Value == "" ||
		fixture.Release.DriverStep == "" || fixture.Release.DriverEnv.Key == "" || fixture.Release.DriverEnv.Value == "" ||
		len(fixture.Release.DriverContains) != 4 || len(fixture.Release.OutputGuards) != 3 ||
		fixture.ReleaseGuard.Workflow == "" || fixture.ReleaseGuard.Job == "" || fixture.ReleaseGuard.JobIfCondition == "" || len(fixture.ReleaseGuard.RequiredPRBranches) != 1 || len(fixture.ReleaseGuard.RequiredPRPaths) != 5 || fixture.ReleaseGuard.Step == "" || fixture.ReleaseGuard.IfCondition == "" ||
		!regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(fixture.ReleaseGuard.InitialFinal) ||
		fixture.ReleaseGuard.ExpectedRun == "" || strings.Count(fixture.ReleaseGuard.ExpectedRun, "--initial-final "+fixture.ReleaseGuard.InitialFinal) != 1 ||
		len(fixture.ReleaseGuard.RequiredJobPermissions) != 2 || len(fixture.ReleaseGuard.RequiredJobEnv) != 2 ||
		fixture.ReleaseGuard.ActorStep == "" || len(fixture.ReleaseGuard.ActorEnv) != 4 || strings.TrimSpace(fixture.ReleaseGuard.ActorRun) == "" ||
		fixture.ReleaseGuard.CheckoutStep == "" || fixture.ReleaseGuard.ParseStep == "" || fixture.ReleaseGuard.CheckoutFetchDepth == "" || fixture.ReleaseGuard.CheckoutFetchTags == "" ||
		len(fixture.ReusableCallers) != 2 || fixture.ReleaseValidate.Workflow == "" || len(fixture.ReleaseValidate.RequiredPaths) != 16 ||
		len(fixture.TestsWorkflow.Triggers) != 2 || len(fixture.TestsWorkflow.RequiredPaths) != 2 {
		t.Fatalf("e2e: workflow contract fixture is incomplete: %+v", fixture)
	}
	seenCallers := make(map[string]struct{}, len(fixture.ReusableCallers))
	for callerIndex, caller := range fixture.ReusableCallers {
		if strings.TrimSpace(caller.Workflow) == "" || len(caller.Jobs) != 3 {
			t.Fatalf("e2e: reusable caller fixture %d is incomplete: %+v", callerIndex, caller)
		}
		if _, exists := seenCallers[caller.Workflow]; exists {
			t.Fatalf("e2e: reusable caller fixture repeats workflow %q", caller.Workflow)
		}
		seenCallers[caller.Workflow] = struct{}{}
		seenJobs := make(map[string]struct{}, len(caller.Jobs))
		for jobIndex, job := range caller.Jobs {
			if strings.TrimSpace(job.Job) == "" || len(job.Permissions) != 1 {
				t.Fatalf("e2e: reusable caller fixture %d job %d is incomplete: %+v", callerIndex, jobIndex, job)
			}
			if _, exists := seenJobs[job.Job]; exists {
				t.Fatalf("e2e: reusable caller fixture %d repeats job %q", callerIndex, job.Job)
			}
			seenJobs[job.Job] = struct{}{}
		}
	}
	return fixture
}

func TestReleaseE2EWorkflowContract(t *testing.T) {
	assertReleaseE2EWorkflowContract(t)
	assertProductionWorkflowReusableJobsHaveNoSecrets(t)
}

func TestReleaseArtifactWorkflowsRequireRealDashboard(t *testing.T) {
	assertReleaseWorkflowBuildsRealDashboard(t, ".github/workflows/release.yml", "release", "Run goreleaser")
	assertReleaseWorkflowBuildsRealDashboard(t, ".github/workflows/release-e2e.yml", "release-e2e", "Build release snapshot artifacts")
	assertReleaseWorkflowBuildsRealDashboard(t, ".github/workflows/release-validate.yml", "snapshot", "goreleaser release --snapshot")
}

func TestReleaseWorkflowInitialFinalBootstrap(t *testing.T) {
	fixture := loadE2EWorkflowContractFixture(t)
	path := filepath.Join(releaseWorkflowRepoRoot(t), fixture.ReleaseGuard.Workflow)
	doc := readWorkflowDoc(t, path)
	job := yamlMappingValue(yamlMappingValue(doc, "jobs"), fixture.ReleaseGuard.Job)
	if job == nil {
		t.Fatalf("release: workflow must define jobs.%s", fixture.ReleaseGuard.Job)
	}
	jobCondition := yamlMappingValue(job, "if")
	if jobCondition == nil || jobCondition.Value != fixture.ReleaseGuard.JobIfCondition {
		t.Fatalf("release: jobs.%s if = %v, want %q", fixture.ReleaseGuard.Job, jobCondition, fixture.ReleaseGuard.JobIfCondition)
	}
	on := yamlMappingValue(doc, "on")
	pullRequest := yamlMappingValue(on, "pull_request")
	assertYAMLSequenceExact(t, yamlMappingValue(pullRequest, "branches"), fixture.ReleaseGuard.RequiredPRBranches, "release startup PR branches")
	assertYAMLSequenceExact(t, yamlMappingValue(pullRequest, "paths"), fixture.ReleaseGuard.RequiredPRPaths, "release startup PR paths")

	permissions := yamlMappingValue(job, "permissions")
	assertYAMLMappingExact(t, permissions, fixture.ReleaseGuard.RequiredJobPermissions, "release guard permissions")
	jobEnv := yamlMappingValue(job, "env")
	assertYAMLMappingExact(t, jobEnv, fixture.ReleaseGuard.RequiredJobEnv, "release guard job env")

	steps := yamlMappingValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		t.Fatalf("release: jobs.%s must define steps", fixture.ReleaseGuard.Job)
	}
	actorStep := workflowStepNode(t, steps.Content, fixture.ReleaseGuard.ActorStep)
	actorEnv := yamlMappingValue(actorStep, "env")
	assertYAMLMappingExact(t, actorEnv, fixture.ReleaseGuard.ActorEnv, "release actor guard env")
	actorGuard := workflowStepRun(t, steps.Content, fixture.ReleaseGuard.ActorStep)
	if strings.TrimSpace(actorGuard) != strings.TrimSpace(fixture.ReleaseGuard.ActorRun) {
		t.Fatalf("release: actor guard run script = %q, want exact fail-closed script %q", actorGuard, fixture.ReleaseGuard.ActorRun)
	}

	guard := workflowStepRun(t, steps.Content, fixture.ReleaseGuard.Step)
	guardStep := workflowStepNode(t, steps.Content, fixture.ReleaseGuard.Step)
	condition := yamlMappingValue(guardStep, "if")
	if condition == nil || condition.Value != fixture.ReleaseGuard.IfCondition {
		t.Fatalf("release: initial-final guard condition = %v, want %q", condition, fixture.ReleaseGuard.IfCondition)
	}
	if strings.TrimSpace(guard) != strings.TrimSpace(fixture.ReleaseGuard.ExpectedRun) {
		t.Fatalf("release: initial-final guard run = %q, want exact command %q", guard, fixture.ReleaseGuard.ExpectedRun)
	}

	checkout := workflowStepNode(t, steps.Content, fixture.ReleaseGuard.CheckoutStep)
	with := yamlMappingValue(checkout, "with")
	depth := yamlMappingValue(with, "fetch-depth")
	tags := yamlMappingValue(with, "fetch-tags")
	if depth == nil || depth.Value != fixture.ReleaseGuard.CheckoutFetchDepth || tags == nil || tags.Value != fixture.ReleaseGuard.CheckoutFetchTags {
		t.Fatalf("release: initial-final guard requires full tag history; fetch-depth=%v fetch-tags=%v", depth, tags)
	}
	assertStepBefore(t, steps.Content, fixture.ReleaseGuard.ActorStep, fixture.ReleaseGuard.CheckoutStep)
	assertStepBefore(t, steps.Content, fixture.ReleaseGuard.CheckoutStep, fixture.ReleaseGuard.ParseStep)
	assertStepBefore(t, steps.Content, fixture.ReleaseGuard.ParseStep, fixture.ReleaseGuard.Step)
}

func TestReusableWorkflowCallerPermissions(t *testing.T) {
	fixture := loadE2EWorkflowContractFixture(t)
	for _, caller := range fixture.ReusableCallers {
		path := filepath.Join(releaseWorkflowRepoRoot(t), caller.Workflow)
		doc := readWorkflowDoc(t, path)
		jobs := yamlMappingValue(doc, "jobs")
		if jobs == nil || jobs.Kind != yaml.MappingNode {
			t.Fatalf("%s: workflow must define a jobs mapping", caller.Workflow)
		}
		actualReusableJobs := 0
		for i := 0; i+1 < len(jobs.Content); i += 2 {
			if yamlMappingValue(jobs.Content[i+1], "uses") != nil {
				actualReusableJobs++
			}
		}
		if actualReusableJobs != len(caller.Jobs) {
			t.Fatalf("%s: found %d reusable caller jobs, want exactly %d", caller.Workflow, actualReusableJobs, len(caller.Jobs))
		}
		for _, expectation := range caller.Jobs {
			job := yamlMappingValue(jobs, expectation.Job)
			if job == nil || yamlMappingValue(job, "uses") == nil {
				t.Fatalf("%s: workflow must define reusable job %q", caller.Workflow, expectation.Job)
			}
			assertYAMLMappingExact(t, yamlMappingValue(job, "permissions"), expectation.Permissions, caller.Workflow+" reusable job "+expectation.Job+" permissions")
		}
	}
}

func TestReleaseValidateTracksPackagingInputs(t *testing.T) {
	fixture := loadE2EWorkflowContractFixture(t)
	path := filepath.Join(releaseWorkflowRepoRoot(t), fixture.ReleaseValidate.Workflow)
	doc := readWorkflowDoc(t, path)
	paths := yamlMappingValue(yamlMappingValue(yamlMappingValue(doc, "on"), "pull_request"), "paths")
	for _, required := range fixture.ReleaseValidate.RequiredPaths {
		if !yamlSequenceContains(paths, required) {
			t.Fatalf("release-validate: on.pull_request.paths missing %q", required)
		}
	}
}

func assertReleaseWorkflowBuildsRealDashboard(t *testing.T, relativePath, jobName, artifactStep string) {
	t.Helper()
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), relativePath))
	job := yamlMappingValue(yamlMappingValue(doc, "jobs"), jobName)
	if job == nil {
		t.Fatalf("%s: workflow job %q is missing", relativePath, jobName)
	}
	steps := yamlMappingValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		t.Fatalf("%s: workflow job %q has no steps", relativePath, jobName)
	}
	build := workflowStepRun(t, steps.Content, "Build real web dashboard")
	if strings.TrimSpace(build) != "make web" || strings.Contains(build, "web-stub") || strings.Contains(build, "||") {
		t.Fatalf("%s: artifact workflow must fail closed on the exact real dashboard build, got %q", relativePath, build)
	}
	assertion := workflowStepRun(t, steps.Content, "Assert real dashboard artifact")
	for _, required := range []string{"web/out/index.html", "is not bundled in this build", "exit 1"} {
		if !strings.Contains(assertion, required) {
			t.Fatalf("%s: real-dashboard assertion is missing %q", relativePath, required)
		}
	}
	assertStepBefore(t, steps.Content, "Build real web dashboard", "Assert real dashboard artifact")
	assertStepBefore(t, steps.Content, "Assert real dashboard artifact", artifactStep)
}

func TestE2EWorkflowContract(t *testing.T) {
	fixture := loadE2EWorkflowContractFixture(t)
	workflowPath := filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "e2e.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("e2e: read workflow contract %s: %v", workflowPath, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("e2e: parse workflow YAML: %v", err)
	}
	if len(root.Content) == 0 {
		t.Fatal("e2e: workflow YAML is empty")
	}
	doc := root.Content[0]
	on := yamlMappingValue(doc, "on")
	if on == nil {
		t.Fatal("e2e: workflow must define triggers")
	}
	for _, trigger := range fixture.E2E.RequiredTriggers {
		if yamlMappingValue(on, trigger) == nil {
			t.Fatalf("e2e: workflow must keep on.%s trigger", trigger)
		}
	}
	pr := yamlMappingValue(on, "pull_request")
	if pr == nil {
		t.Fatal("e2e: workflow must run on relevant pull_request events")
	}
	branches := yamlMappingValue(pr, "branches")
	if !yamlSequenceContains(branches, "develop") {
		t.Fatal("e2e: pull_request trigger must target develop")
	}
	paths := yamlMappingValue(pr, "paths")
	for _, want := range fixture.E2E.RequiredPaths {
		if !yamlSequenceContains(paths, want) {
			t.Fatalf("e2e: pull_request paths missing %q", want)
		}
	}
	concurrency := yamlMappingValue(doc, "concurrency")
	cancel := yamlMappingValue(concurrency, "cancel-in-progress")
	if cancel == nil || cancel.Value != "true" {
		t.Fatal("e2e: concurrency.cancel-in-progress must stay true")
	}
	steps := e2eWorkflowSteps(t, doc)
	cleanup := workflowStepRun(t, steps, "Clean up stale e2e podman containers")
	if !strings.Contains(cleanup, "podman ps -aq --filter name=peasant-e2e-") {
		t.Fatal("e2e: cleanup step must select peasant-e2e-* podman containers")
	}
	if !strings.Contains(cleanup, "podman rm") {
		t.Fatal("e2e: cleanup step must remove selected stale containers, not just list them")
	}
	for _, status := range fixture.E2E.CleanupRequiredStatuses {
		if !strings.Contains(cleanup, "--filter "+status) {
			t.Fatalf("e2e: cleanup step must filter %s containers", status)
		}
	}
	if got := strings.Count(cleanup, "--filter status="); got != len(fixture.E2E.CleanupRequiredStatuses) {
		t.Fatalf("e2e: cleanup step status filter count = %d, want %d", got, len(fixture.E2E.CleanupRequiredStatuses))
	}
	for _, forbidden := range fixture.E2E.CleanupForbiddenStatuses {
		if strings.Contains(cleanup, forbidden.Value) {
			t.Fatalf("e2e: cleanup step must not use %s: %s", forbidden.Value, forbidden.Reason)
		}
	}
	parity := workflowStepRun(t, steps, fixture.E2E.ParityStep)
	if !strings.Contains(parity, fixture.E2E.ParityRunContains) {
		t.Fatalf("e2e: parity step must run %q", fixture.E2E.ParityRunContains)
	}
	assertWorkflowStepEnv(t, steps, fixture.E2E.ParityStep, fixture.E2E.ParityEnv)
	driver := workflowStepRun(t, steps, fixture.E2E.DriverStep)
	assertWorkflowStepEnv(t, steps, fixture.E2E.DriverStep, fixture.E2E.DriverEnv)
	for _, want := range fixture.E2E.DriverContains {
		if !strings.Contains(driver, want) {
			t.Fatalf("e2e: driver step %q missing %q", fixture.E2E.DriverStep, want)
		}
	}
	assertStepBefore(t, steps, fixture.E2E.ParityStep, fixture.E2E.DriverStep)
	assertStepBefore(t, steps, "Clean up stale e2e podman containers", fixture.E2E.DriverStep)
	asserted := workflowStepRun(t, steps, "Assert asserted e2e tests ran and passed")
	for _, testName := range fixture.E2E.AssertedTests {
		if !strings.Contains(asserted, "--- SKIP: "+testName) {
			t.Fatalf("e2e: assertion step must fail when %s skips", testName)
		}
		if !strings.Contains(asserted, "--- PASS: "+testName) {
			t.Fatalf("e2e: assertion step must require positive PASS for %s", testName)
		}
	}
	if !strings.Contains(asserted, "no tests to run") {
		t.Fatal("e2e: assertion step must fail on no-tests output")
	}
	assertWorkflowVillageRefsMatch(t, doc, fixture.ExpectedVillageRef)
	assertTestsWorkflowTracksE2EChanges(t, fixture)
}

func assertReleaseE2EWorkflowContract(t *testing.T) {
	t.Helper()
	fixture := loadE2EWorkflowContractFixture(t)
	workflowPath := filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release-e2e.yml")
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("release-e2e: read workflow contract %s: %v", workflowPath, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("release-e2e: parse workflow YAML: %v", err)
	}
	if len(root.Content) == 0 {
		t.Fatal("release-e2e: workflow YAML is empty")
	}
	doc := root.Content[0]
	on := yamlMappingValue(doc, "on")
	if on == nil || yamlMappingValue(on, "workflow_call") == nil {
		t.Fatal("release-e2e: workflow must expose on.workflow_call for release.yml to call")
	}
	jobs := yamlMappingValue(doc, "jobs")
	if jobs == nil {
		t.Fatal("release-e2e: workflow must define jobs")
	}
	job := yamlMappingValue(jobs, defaults.ReleaseE2EWorkflowJob)
	if job == nil {
		t.Fatalf("release-e2e: workflow must define job named exactly %q", defaults.ReleaseE2EWorkflowJob)
	}
	if yamlMappingValue(job, "if") != nil {
		t.Fatalf("release-e2e: jobs.%s must not have an if condition; it must run whenever release.yml calls it", defaults.ReleaseE2EWorkflowJob)
	}
	runsOn := yamlMappingValue(job, "runs-on")
	if runsOn == nil || runsOn.Value != "blacksmith-4vcpu-ubuntu-2404" {
		got := "<missing>"
		if runsOn != nil {
			got = runsOn.Value
		}
		t.Fatalf("release-e2e: jobs.%s runs-on = %s, want blacksmith-4vcpu-ubuntu-2404 for Arch amd64 coverage", defaults.ReleaseE2EWorkflowJob, got)
	}
	steps := yamlMappingValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		t.Fatalf("release-e2e: jobs.%s must have a steps sequence", defaults.ReleaseE2EWorkflowJob)
	}
	stepNodes := steps.Content
	parity := workflowStepRun(t, stepNodes, fixture.Release.ParityStep)
	if !strings.Contains(parity, fixture.Release.ParityRunContains) {
		t.Fatalf("release-e2e: parity step must run %q", fixture.Release.ParityRunContains)
	}
	assertWorkflowStepEnv(t, stepNodes, fixture.Release.ParityStep, fixture.Release.ParityEnv)
	driver := workflowStepRun(t, stepNodes, fixture.Release.DriverStep)
	assertWorkflowStepEnv(t, stepNodes, fixture.Release.DriverStep, fixture.Release.DriverEnv)
	for _, want := range fixture.Release.DriverContains {
		if !strings.Contains(driver, want) {
			t.Fatalf("release-e2e: driver step %q missing %q", fixture.Release.DriverStep, want)
		}
	}
	for _, want := range fixture.Release.OutputGuards {
		if !strings.Contains(driver, want) {
			t.Fatalf("release-e2e: driver step %q missing output guard %q", fixture.Release.DriverStep, want)
		}
	}
	assertStepBefore(t, stepNodes, fixture.Release.ParityStep, fixture.Release.DriverStep)
	goTestSteps := 0
	for _, step := range steps.Content {
		run := yamlMappingValue(step, "run")
		if run != nil && strings.Contains(run.Value, "go test") && strings.Contains(run.Value, "TestReleasePerDistro") {
			goTestSteps++
		}
	}
	if goTestSteps != 1 {
		t.Fatalf("release-e2e: jobs.%s must have exactly one go test driver step for TestReleasePerDistro, found %d", defaults.ReleaseE2EWorkflowJob, goTestSteps)
	}
}

func assertWorkflowVillageRefsMatch(t *testing.T, e2eDoc *yaml.Node, expected string) {
	t.Helper()
	releasePath := filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release-e2e.yml")
	releaseDoc := readWorkflowDoc(t, releasePath)
	e2eRef := workflowEnvValue(t, e2eDoc, "VILLAGE_REF")
	releaseRef := workflowEnvValue(t, releaseDoc, "VILLAGE_REF")
	if e2eRef == "" || releaseRef == "" || e2eRef != releaseRef || e2eRef != expected || releaseRef != expected {
		t.Fatalf("e2e: VILLAGE_REF pins must equal exact accepted fixture SHA %q; e2e=%q release-e2e=%q", expected, e2eRef, releaseRef)
	}
}

func assertTestsWorkflowTracksE2EChanges(t *testing.T, fixture e2eWorkflowContractFixture) {
	t.Helper()
	path := filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "tests.yml")
	doc := readWorkflowDoc(t, path)
	on := yamlMappingValue(doc, "on")
	for _, trigger := range fixture.TestsWorkflow.Triggers {
		paths := yamlMappingValue(yamlMappingValue(on, trigger), "paths")
		for _, want := range fixture.TestsWorkflow.RequiredPaths {
			if !yamlSequenceContains(paths, want) {
				t.Fatalf("e2e: tests workflow on.%s.paths missing %q", trigger, want)
			}
		}
	}
}

func readWorkflowDoc(t *testing.T, path string) *yaml.Node {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("e2e: read workflow %s: %v", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("e2e: parse workflow %s: %v", path, err)
	}
	if len(root.Content) == 0 {
		t.Fatalf("e2e: workflow %s is empty", path)
	}
	return root.Content[0]
}

func workflowEnvValue(t *testing.T, doc *yaml.Node, key string) string {
	t.Helper()
	env := yamlMappingValue(doc, "env")
	value := yamlMappingValue(env, key)
	if value == nil {
		t.Fatalf("e2e: workflow env missing %s", key)
	}
	return value.Value
}

func yamlMappingValue(node *yaml.Node, key string) *yaml.Node {
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

func assertYAMLMappingValue(t *testing.T, node *yaml.Node, key, want, context string) {
	t.Helper()
	value := yamlMappingValue(node, key)
	if value == nil || value.Value != want {
		t.Fatalf("%s: %s = %v, want %q", context, key, value, want)
	}
}

func assertYAMLMappingExact(t *testing.T, node *yaml.Node, want []workflowEnvExpectation, context string) {
	t.Helper()
	if node == nil || node.Kind != yaml.MappingNode {
		t.Fatalf("%s: expected YAML mapping, got %v", context, node)
	}
	if len(node.Content) != 2*len(want) {
		t.Fatalf("%s: mapping has %d entries, want exactly %d", context, len(node.Content)/2, len(want))
	}
	for _, expectation := range want {
		assertYAMLMappingValue(t, node, expectation.Key, expectation.Value, context)
	}
}

func assertYAMLSequenceExact(t *testing.T, node *yaml.Node, want []string, context string) {
	t.Helper()
	if node == nil || node.Kind != yaml.SequenceNode {
		t.Fatalf("%s: expected YAML sequence, got %v", context, node)
	}
	if len(node.Content) != len(want) {
		t.Fatalf("%s: sequence has %d entries, want exactly %d", context, len(node.Content), len(want))
	}
	for i, expected := range want {
		if node.Content[i].Value != expected {
			t.Fatalf("%s: entry %d = %q, want %q", context, i, node.Content[i].Value, expected)
		}
	}
}

func yamlSequenceContains(node *yaml.Node, want string) bool {
	if node == nil || node.Kind != yaml.SequenceNode {
		return false
	}
	for _, item := range node.Content {
		if item.Value == want {
			return true
		}
	}
	return false
}

func assertProductionWorkflowReusableJobsHaveNoSecrets(t *testing.T) {
	t.Helper()
	path := filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release.yml")
	doc := readWorkflowDoc(t, path)
	jobs := yamlMappingValue(doc, "jobs")
	e2eJob := yamlMappingValue(jobs, "e2e")
	releaseE2EJob := yamlMappingValue(jobs, defaults.ReleaseE2EWorkflowJob)
	if e2eJob == nil || releaseE2EJob == nil {
		t.Fatal("release: workflow must define reusable jobs e2e and release-e2e")
	}
	if yamlMappingValue(e2eJob, "secrets") != nil || yamlMappingValue(releaseE2EJob, "secrets") != nil {
		t.Fatal("release: reusable jobs e2e and release-e2e must not declare a secrets mapping or scalar; they run without inherited credentials")
	}
}

func e2eWorkflowSteps(t *testing.T, doc *yaml.Node) []*yaml.Node {
	t.Helper()
	jobs := yamlMappingValue(doc, "jobs")
	if jobs == nil {
		t.Fatal("e2e: workflow must define jobs")
	}
	job := yamlMappingValue(jobs, "e2e")
	if job == nil {
		t.Fatal("e2e: workflow must define jobs.e2e")
	}
	steps := yamlMappingValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		t.Fatal("e2e: jobs.e2e must define a steps sequence")
	}
	return steps.Content
}

func workflowStepRun(t *testing.T, steps []*yaml.Node, name string) string {
	t.Helper()
	step := workflowStepNode(t, steps, name)
	run := yamlMappingValue(step, "run")
	if run == nil {
		t.Fatalf("e2e: step %q must have a run script", name)
	}
	return run.Value
}

func workflowStepNode(t *testing.T, steps []*yaml.Node, name string) *yaml.Node {
	t.Helper()
	for _, step := range steps {
		stepName := yamlMappingValue(step, "name")
		if stepName == nil || stepName.Value != name {
			continue
		}
		return step
	}
	t.Fatalf("e2e: missing workflow step %q", name)
	return nil
}

func workflowStepEnvValue(t *testing.T, steps []*yaml.Node, stepName, key string) string {
	t.Helper()
	for _, step := range steps {
		name := yamlMappingValue(step, "name")
		if name == nil || name.Value != stepName {
			continue
		}
		env := yamlMappingValue(step, "env")
		value := yamlMappingValue(env, key)
		if value == nil {
			t.Fatalf("e2e: step %q env missing %s", stepName, key)
		}
		return value.Value
	}
	t.Fatalf("e2e: missing workflow step %q", stepName)
	return ""
}

func assertWorkflowStepEnv(t *testing.T, steps []*yaml.Node, stepName string, want workflowEnvExpectation) {
	t.Helper()
	got := workflowStepEnvValue(t, steps, stepName, want.Key)
	if got != want.Value {
		t.Fatalf("e2e: step %q env %s = %q, want %q", stepName, want.Key, got, want.Value)
	}
}

func assertStepBefore(t *testing.T, steps []*yaml.Node, before, after string) {
	t.Helper()
	beforeIndex, afterIndex := -1, -1
	for i, step := range steps {
		name := yamlMappingValue(step, "name")
		if name == nil {
			continue
		}
		switch name.Value {
		case before:
			beforeIndex = i
		case after:
			afterIndex = i
		}
	}
	if beforeIndex < 0 || afterIndex < 0 {
		t.Fatalf("e2e: cannot compare step order, %q index=%d %q index=%d", before, beforeIndex, after, afterIndex)
	}
	if beforeIndex >= afterIndex {
		t.Fatalf("e2e: workflow step %q must run before %q", before, after)
	}
}

func releaseWorkflowRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("release-e2e: getwd: %v", err)
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/peasant-labs/peasant") {
			return dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			t.Fatalf("release-e2e: cannot locate peasant repository root from %s", wd)
		}
	}
}
