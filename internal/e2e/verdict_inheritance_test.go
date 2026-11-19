package e2e

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/workflows/verdict_inheritance.yaml
var verdictInheritanceFixtureBytes []byte

//go:embed testdata/workflows/verdict_inheritance_merge_gate.manifest.yaml
var verdictInheritanceMergeGateManifest []byte

//go:embed testdata/workflows/verdict_inheritance_precheck.manifest.yaml
var verdictInheritancePrecheckManifest []byte

//go:embed testdata/workflows/verdict_inheritance_classify.manifest.yaml
var verdictInheritanceClassifyManifest []byte

//go:embed testdata/workflows/verdict_inheritance_jobs.manifest.yaml
var verdictInheritanceJobsManifest []byte

type verdictInheritanceFixture struct {
	VerdictMarkers struct {
		Version int      `yaml:"version"`
		Names   []string `yaml:"names"`
	} `yaml:"verdict_markers"`
	PolicyFile    string `yaml:"policy_file"`
	WorkflowFile  string `yaml:"workflow_file"`
	ReleaseConfig string `yaml:"release_config"`
	AggregatorJob string `yaml:"aggregator_job"`
	FullSuiteJob  string `yaml:"full_suite_job"`
	MergeGateStep string `yaml:"merge_gate_step"`
	MarkerSteps   []struct {
		Name string `yaml:"name"`
		If   string `yaml:"if"`
	} `yaml:"marker_steps"`
	InheritedJobs []struct {
		Job string `yaml:"job"`
		If  string `yaml:"if"`
	} `yaml:"inherited_jobs"`
	KeptJobs []struct {
		Job    string `yaml:"job"`
		Reason string `yaml:"reason"`
	} `yaml:"kept_jobs"`
	ReleaseJobs           []string                     `yaml:"release_jobs"`
	ClassifierJob         string                       `yaml:"classifier_job"`
	PrecheckStep          string                       `yaml:"precheck_step"`
	ClassifyStep          string                       `yaml:"classify_step"`
	InheritStep           string                       `yaml:"inherit_step"`
	ToolIf                string                       `yaml:"tool_if"`
	ToolSteps             []string                     `yaml:"tool_steps"`
	ClassifierPermissions map[string]string            `yaml:"classifier_permissions"`
	PrecheckCases         []verdictPrecheckCase        `yaml:"precheck_cases"`
	ClassifyCases         []verdictClassifyCase        `yaml:"classify_cases"`
	ResultProfiles        map[string]map[string]string `yaml:"result_profiles"`
	MergeGateCases        []verdictMergeGateCase       `yaml:"merge_gate_cases"`
}

type verdictPrecheckCase struct {
	Name          string `yaml:"name"`
	Event         string `yaml:"event"`
	GroupRef      string `yaml:"group_ref"`
	PRAuthorType  string `yaml:"pr_author_type"`
	PRHeadRef     string `yaml:"pr_head_ref"`
	GHHead        string `yaml:"gh_head"`
	GHExit        int    `yaml:"gh_exit"`
	ExpectedGH    string `yaml:"expected_gh"`
	ExpectedBuild bool   `yaml:"expected_build"`
	ExpectedExit  int    `yaml:"expected_exit"`
}

type verdictClassifyCase struct {
	Name              string   `yaml:"name"`
	Event             string   `yaml:"event"`
	StubKind          string   `yaml:"stub_kind"`
	ExpectedRelease   string   `yaml:"expected_release"`
	ExpectedCandidate string   `yaml:"expected_candidate"`
	ExpectedCalls     []string `yaml:"expected_calls"`
}

type verdictMergeGateCase struct {
	Name             string            `yaml:"name"`
	Event            string            `yaml:"event"`
	Code             *string           `yaml:"code"`
	Release          *string           `yaml:"release"`
	ReleaseCandidate *string           `yaml:"release_candidate"`
	ReleaseOnly      *string           `yaml:"release_only"`
	Inherit          *string           `yaml:"inherit"`
	Profile          string            `yaml:"profile"`
	Overrides        map[string]string `yaml:"overrides"`
	ExpectedError    string            `yaml:"expected_error"`
	ExpectedMarker   string            `yaml:"expected_marker"`
}

const (
	verdictStubRepository = "peasant-labs/peasant"
	verdictStubGroupRef   = "refs/heads/gh-readonly-queue/develop/pr-1092-0123456789abcdef0123456789abcdef01234567"
	verdictStubAuthor     = "app/peasant-labs-releaser"
	verdictStubBase       = "1111111111111111111111111111111111111111"
	verdictStubHead       = "2222222222222222222222222222222222222222"
)

func loadVerdictInheritanceFixture(t *testing.T) verdictInheritanceFixture {
	t.Helper()
	var fixture verdictInheritanceFixture
	decoder := yaml.NewDecoder(bytes.NewReader(verdictInheritanceFixtureBytes))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("verdict inheritance: parse the embedded fixture: %v; fix the fixture", err)
	}
	if fixture.VerdictMarkers.Version != 1 || len(fixture.VerdictMarkers.Names) == 0 {
		t.Fatalf("verdict inheritance: the fixture mirrors marker set version %d with %d names; infra's pinned set is version 1", fixture.VerdictMarkers.Version, len(fixture.VerdictMarkers.Names))
	}
	return fixture
}

// requireVerdictCaseNames checks a fixture case list against its
// required-name manifest: exact membership, no count.
func requireVerdictCaseNames(t *testing.T, manifestBytes []byte, names []string, label string) {
	t.Helper()
	manifest, err := testutil.DecodeRequiredNamesManifest(manifestBytes, label)
	if err != nil {
		t.Fatal(err)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, label); err != nil {
		t.Fatal(err)
	}
}

func verdictWorkflowJobs(t *testing.T, fixture verdictInheritanceFixture) *yaml.Node {
	t.Helper()
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), fixture.WorkflowFile))
	jobs := yamlMappingValue(doc, "jobs")
	if jobs == nil {
		t.Fatalf("verdict inheritance: %s has no jobs", fixture.WorkflowFile)
	}
	return jobs
}

func verdictJobStepByID(t *testing.T, jobs *yaml.Node, job, id string) *yaml.Node {
	t.Helper()
	steps := yamlMappingValue(yamlMappingValue(jobs, job), "steps")
	if steps == nil {
		t.Fatalf("verdict inheritance: jobs.%s has no steps", job)
	}
	for _, step := range steps.Content {
		if node := yamlMappingValue(step, "id"); node != nil && node.Value == id {
			return step
		}
	}
	t.Fatalf("verdict inheritance: jobs.%s has no step with id %s", job, id)
	return nil
}

func verdictScalar(node *yaml.Node, key string) string {
	if child := yamlMappingValue(node, key); child != nil {
		return child.Value
	}
	return ""
}

// normalizeCondition collapses the line breaks a folded `if: >-` keeps
// around more-indented lines, so a condition compares by its tokens.
func normalizeCondition(condition string) string {
	return strings.Join(strings.Fields(condition), " ")
}

func verdictValue(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
}

// runVerdictStep runs one workflow step's run script under bash and returns
// its exit code and combined output.
func runVerdictStep(t *testing.T, script, dir string, env []string) (int, string) {
	t.Helper()
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		var failure *exec.ExitError
		if !errors.As(err, &failure) {
			t.Fatalf("verdict inheritance: execute step: %v\n%s", err, out)
		}
		return failure.ExitCode(), string(out)
	}
	return 0, string(out)
}

// readVerdictStepOutputs parses a GITHUB_OUTPUT file of key=value lines.
func readVerdictStepOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("verdict inheritance: read step outputs: %v", err)
	}
	outputs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			outputs[key] = value
		}
	}
	return outputs
}

func requireVerdictTools(t *testing.T) {
	t.Helper()
	// The pool runner image and GitHub-hosted runners both ship bash and jq;
	// a missing tool fails rather than skipping the gate's predicates.
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("verdict inheritance: the workflow steps need %s on PATH: %v", tool, err)
		}
	}
}

// verdictStubEnv returns an environment whose gh, git and release-intent are
// exported bash functions that log their arguments under logs. Functions,
// not stub files: a test-written executable can fail with ETXTBSY when a
// parallel test forks while its write descriptor is still open. Every step
// runs under bash and scripts/release-group-entry runs as `bash <script>`, so
// each nested shell imports the same stubs.
func verdictStubEnv(t *testing.T, dir string, extra ...string) (env []string, logs string) {
	t.Helper()
	logs = filepath.Join(dir, "logs")
	runnerTemp := filepath.Join(dir, "runner")
	for _, d := range []string{logs, runnerTemp} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stubs := map[string]string{
		"gh": `printf '%s\n' "$*" >> "$STUB_LOGS/gh"
  [ "$STUB_GH_EXIT" = 0 ] || return "$STUB_GH_EXIT"
  case "$1" in
    api) printf '%s\n' "$STUB_GH_HEAD" ;;
    pr) printf '{"author":{"login":"%s"},"headRefName":"%s"}\n' "$STUB_PR_AUTHOR" "$STUB_GH_HEAD" ;;
    *) return 97 ;;
  esac`,
		"git": `printf '%s\n' "$*" >> "$STUB_LOGS/git"
  return 97`,
		"release-intent": `printf '%s\n' "$*" >> "$STUB_LOGS/release-intent"
  case "$1" in
    group-pr) echo pr=1092 ;;
    classify) printf '%s\n' "$STUB_KIND" ;;
    *) return 97 ;;
  esac`,
	}
	env = os.Environ()
	for name, body := range stubs {
		env = append(env, "BASH_FUNC_"+name+"%%=() {\n  "+body+"\n}")
	}
	env = append(env,
		"STUB_LOGS="+logs,
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_REPOSITORY="+verdictStubRepository,
	)
	return append(env, extra...), logs
}

// TestMergeGateVerdict runs the actual merge-gate shell step, with its own
// INHERITED_JOBS and RELEASE_JOBS from the workflow, over fixture needs
// results, and checks that on every passing state exactly one marker step's
// condition, read from the workflow, holds.
func TestMergeGateVerdict(t *testing.T) {
	t.Parallel()
	requireVerdictTools(t)
	fixture := loadVerdictInheritanceFixture(t)
	names := make([]string, 0, len(fixture.MergeGateCases))
	for _, c := range fixture.MergeGateCases {
		names = append(names, c.Name)
	}
	requireVerdictCaseNames(t, verdictInheritanceMergeGateManifest, names, "merge-gate verdict cases")

	jobs := verdictWorkflowJobs(t, fixture)
	aggregator := yamlMappingValue(jobs, fixture.AggregatorJob)
	steps := yamlMappingValue(aggregator, "steps")
	if steps == nil {
		t.Fatalf("verdict inheritance: jobs.%s has no steps", fixture.AggregatorJob)
	}
	needs := workflowNeedsValues(yamlMappingValue(aggregator, "needs"))
	var gate *yaml.Node
	markerIfs := make(map[string]string)
	for _, step := range steps.Content {
		name := verdictScalar(step, "name")
		if name == fixture.MergeGateStep {
			gate = step
		}
		if slices.Contains(fixture.VerdictMarkers.Names, name) {
			markerIfs[name] = verdictScalar(step, "if")
		}
	}
	script := verdictScalar(gate, "run")
	if script == "" {
		t.Fatalf("verdict inheritance: jobs.%s has no step %q with a run script", fixture.AggregatorJob, fixture.MergeGateStep)
	}
	if len(markerIfs) != len(fixture.VerdictMarkers.Names) {
		t.Fatalf("verdict inheritance: jobs.%s carries %d of the %d marker steps", fixture.AggregatorJob, len(markerIfs), len(fixture.VerdictMarkers.Names))
	}
	gateEnv := yamlMappingValue(gate, "env")
	inheritedJobs := verdictScalar(gateEnv, "INHERITED_JOBS")
	releaseJobs := verdictScalar(gateEnv, "RELEASE_JOBS")

	for profile, results := range fixture.ResultProfiles {
		jobsInProfile := make([]string, 0, len(results))
		for job := range results {
			jobsInProfile = append(jobsInProfile, job)
		}
		if !sameStringSet(jobsInProfile, needs) {
			t.Fatalf("verdict inheritance: result profile %q names %q; it must name exactly merge-gate's needs %q", profile, jobsInProfile, needs)
		}
	}

	for _, c := range fixture.MergeGateCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			profile, ok := fixture.ResultProfiles[c.Profile]
			if !ok {
				t.Fatalf("case names unknown result profile %q", c.Profile)
			}
			results := make(map[string]map[string]string, len(profile))
			for job, result := range profile {
				results[job] = map[string]string{"result": result}
			}
			for job, result := range c.Overrides {
				if _, needed := profile[job]; !needed {
					t.Fatalf("override names %q, which merge-gate does not need", job)
				}
				results[job] = map[string]string{"result": result}
			}
			resultsJSON, err := json.Marshal(results)
			if err != nil {
				t.Fatal(err)
			}
			event := c.Event
			if event == "" {
				event = "merge_group"
			}
			inherit := verdictValue(c.Inherit, "false")
			exit, out := runVerdictStep(t, script, t.TempDir(), append(os.Environ(),
				"RESULTS="+string(resultsJSON),
				"EVENT="+event,
				"CODE="+verdictValue(c.Code, "true"),
				"RELEASE="+verdictValue(c.Release, "false"),
				"RELEASE_CANDIDATE="+verdictValue(c.ReleaseCandidate, "false"),
				"RELEASE_ONLY="+verdictValue(c.ReleaseOnly, "false"),
				"INHERIT="+inherit,
				"INHERITED_JOBS="+inheritedJobs,
				"RELEASE_JOBS="+releaseJobs,
			))
			if c.ExpectedError != "" {
				if c.ExpectedMarker != "" {
					t.Fatal("a reject row names its expected_error and no expected_marker")
				}
				if exit == 0 {
					t.Fatalf("merge-gate accepted a state it must refuse with %q:\n%s", c.ExpectedError, out)
				}
				if !strings.Contains(out, "::error::merge-gate: "+c.ExpectedError) {
					t.Fatalf("merge-gate output lacks the refusal %q:\n%s", c.ExpectedError, out)
				}
				return
			}
			if exit != 0 {
				t.Fatalf("merge-gate exit = %d, want 0:\n%s", exit, out)
			}
			if c.ExpectedMarker == "" {
				t.Fatal("an accepting row names the marker step that must run")
			}
			state := map[string]string{"needs.changes.outputs.inherit": inherit}
			for job, r := range results {
				state["needs."+job+".result"] = r["result"]
			}
			var running []string
			for _, name := range fixture.VerdictMarkers.Names {
				holds, err := evalVerdictMarkerCondition(markerIfs[name], state)
				if err != nil {
					t.Fatalf("marker step %q: %v", name, err)
				}
				if holds {
					running = append(running, name)
				}
			}
			if len(running) != 1 || running[0] != c.ExpectedMarker {
				t.Fatalf("marker steps whose condition holds = %q, want exactly [%q]", running, c.ExpectedMarker)
			}
		})
	}
}

// evalVerdictMarkerCondition evaluates the only expression shape the marker
// steps use: `&&`-joined comparisons of a needs context value with a quoted
// literal. Anything else is an error, so a reshaped condition fails the test
// instead of being misread.
func evalVerdictMarkerCondition(condition string, state map[string]string) (bool, error) {
	if strings.TrimSpace(condition) == "" {
		return false, fmt.Errorf("has no if; every marker step needs its own condition")
	}
	holds := true
	for _, clause := range strings.Split(condition, "&&") {
		fields := strings.Fields(clause)
		if len(fields) != 3 || (fields[1] != "==" && fields[1] != "!=") {
			return false, fmt.Errorf("clause %q is not `<needs value> ==|!= '<literal>'`", strings.TrimSpace(clause))
		}
		got, ok := state[fields[0]]
		if !ok {
			return false, fmt.Errorf("clause %q reads %s, which the fixture state does not carry", strings.TrimSpace(clause), fields[0])
		}
		literal := strings.Trim(fields[2], "'")
		if (got == literal) != (fields[1] == "==") {
			holds = false
		}
	}
	return holds, nil
}

// TestChangesPreCheck runs the real pre-check step with a stub gh, and for
// every run it filters out, the real classify and inherit steps after it:
// those must record no release group, no candidate and no inheritance without
// calling release-intent or git, so an ordinary run never builds the tool.
func TestChangesPreCheck(t *testing.T) {
	t.Parallel()
	requireVerdictTools(t)
	fixture := loadVerdictInheritanceFixture(t)
	names := make([]string, 0, len(fixture.PrecheckCases))
	for _, c := range fixture.PrecheckCases {
		names = append(names, c.Name)
	}
	requireVerdictCaseNames(t, verdictInheritancePrecheckManifest, names, "changes pre-check cases")

	jobs := verdictWorkflowJobs(t, fixture)
	precheck := verdictJobStepByID(t, jobs, fixture.ClassifierJob, fixture.PrecheckStep)
	classify := verdictJobStepByID(t, jobs, fixture.ClassifierJob, fixture.ClassifyStep)
	inherit := verdictJobStepByID(t, jobs, fixture.ClassifierJob, fixture.InheritStep)
	releaseBranch := verdictScalar(yamlMappingValue(precheck, "env"), "RELEASE_BRANCH")
	if releaseBranch == "" {
		t.Fatalf("verdict inheritance: step %s sets no RELEASE_BRANCH", fixture.PrecheckStep)
	}
	root := releaseWorkflowRepoRoot(t)

	for _, c := range fixture.PrecheckCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			event := c.Event
			if event == "" {
				event = "merge_group"
			}
			env, logs := verdictStubEnv(t, dir,
				"STUB_GH_HEAD="+c.GHHead,
				"STUB_GH_EXIT="+strconv.Itoa(c.GHExit),
				"STUB_PR_AUTHOR="+verdictStubAuthor,
				"EVENT="+event,
				"GROUP_REF="+c.GroupRef,
				"PR_AUTHOR_TYPE="+c.PRAuthorType,
				"PR_HEAD_REF="+c.PRHeadRef,
				"PR_NUMBER=7",
				"RELEASE_BRANCH="+releaseBranch,
				"BASE="+verdictStubBase,
				"HEAD="+verdictStubHead,
			)
			stepEnv := func(output string, extra ...string) []string {
				return append(append(slices.Clone(env), "GITHUB_OUTPUT="+output), extra...)
			}

			precheckOut := filepath.Join(dir, "precheck.out")
			exit, out := runVerdictStep(t, verdictScalar(precheck, "run"), root, stepEnv(precheckOut))
			if exit != c.ExpectedExit {
				t.Fatalf("pre-check exit = %d, want %d:\n%s", exit, c.ExpectedExit, out)
			}
			ghLog, _ := os.ReadFile(filepath.Join(logs, "gh"))
			if got := strings.TrimSpace(string(ghLog)); got != c.ExpectedGH {
				t.Errorf("pre-check gh calls = %q, want %q", got, c.ExpectedGH)
			}
			if c.ExpectedExit != 0 {
				return
			}
			if got := readVerdictStepOutputs(t, precheckOut)["build"]; got != strconv.FormatBool(c.ExpectedBuild) {
				t.Fatalf("pre-check build = %q, want %t:\n%s", got, c.ExpectedBuild, out)
			}
			if c.ExpectedBuild {
				return
			}

			classifyOut := filepath.Join(dir, "classify.out")
			if exit, out := runVerdictStep(t, verdictScalar(classify, "run"), root, stepEnv(classifyOut, "BUILD=false")); exit != 0 {
				t.Fatalf("classify step exit = %d:\n%s", exit, out)
			}
			entry := readVerdictStepOutputs(t, classifyOut)
			inheritOut := filepath.Join(dir, "inherit.out")
			if exit, out := runVerdictStep(t, verdictScalar(inherit, "run"), root, stepEnv(inheritOut, "CANDIDATE="+entry["candidate"])); exit != 0 {
				t.Fatalf("inherit step exit = %d:\n%s", exit, out)
			}
			outputs := readVerdictStepOutputs(t, inheritOut)
			outputs["release"] = entry["release"]
			outputs["candidate"] = entry["candidate"]
			for _, key := range []string{"release", "candidate", "release_only", "inherit"} {
				if recorded, err := strconv.ParseBool(outputs[key]); err != nil || recorded {
					t.Errorf("a filtered-out run records %s=%q, want false", key, outputs[key])
				}
			}
			for _, tool := range []string{"release-intent", "git", "gh"} {
				if tool == "gh" && c.ExpectedGH != "" {
					continue
				}
				if _, err := os.Stat(filepath.Join(logs, tool)); err == nil {
					t.Errorf("a filtered-out run called %s", tool)
				}
			}
		})
	}
}

// TestReleaseEntryClassification runs the real classify step after a
// pre-check that built the tool, with a stub release-intent. A merge group
// goes through scripts/release-group-entry; only a release merge group is a
// release group, and a release PR is only a candidate.
func TestReleaseEntryClassification(t *testing.T) {
	t.Parallel()
	requireVerdictTools(t)
	fixture := loadVerdictInheritanceFixture(t)
	names := make([]string, 0, len(fixture.ClassifyCases))
	for _, c := range fixture.ClassifyCases {
		names = append(names, c.Name)
	}
	requireVerdictCaseNames(t, verdictInheritanceClassifyManifest, names, "release entry classify cases")

	jobs := verdictWorkflowJobs(t, fixture)
	classify := verdictJobStepByID(t, jobs, fixture.ClassifierJob, fixture.ClassifyStep)
	root := releaseWorkflowRepoRoot(t)
	for _, c := range fixture.ClassifyCases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			env, logs := verdictStubEnv(t, dir,
				"STUB_GH_HEAD=automation/release",
				"STUB_GH_EXIT=0",
				"STUB_PR_AUTHOR="+verdictStubAuthor,
				"STUB_KIND="+c.StubKind,
				"GITHUB_REF="+verdictStubGroupRef,
				"BUILD=true",
				"EVENT="+c.Event,
				"PR_NUMBER=1105",
				"BASE="+verdictStubBase,
			)
			output := filepath.Join(dir, "classify.out")
			exit, out := runVerdictStep(t, verdictScalar(classify, "run"), root, append(env, "GITHUB_OUTPUT="+output))
			if exit != 0 {
				t.Fatalf("classify step exit = %d:\n%s", exit, out)
			}
			outputs := readVerdictStepOutputs(t, output)
			if outputs["release"] != c.ExpectedRelease || outputs["candidate"] != c.ExpectedCandidate {
				t.Fatalf("classify records release=%q candidate=%q, want release=%q candidate=%q", outputs["release"], outputs["candidate"], c.ExpectedRelease, c.ExpectedCandidate)
			}
			calls, err := os.ReadFile(filepath.Join(logs, "release-intent"))
			if err != nil {
				t.Fatalf("classify never called release-intent: %v", err)
			}
			if got := strings.Split(strings.TrimSpace(string(calls)), "\n"); !slices.Equal(got, c.ExpectedCalls) {
				t.Fatalf("release-intent calls = %q, want %q", got, c.ExpectedCalls)
			}
		})
	}
}

// TestVerdictInheritanceWiring pins the tests.yml wiring that inheritance
// relies on: the aggregator's final steps equal infra's marker set with their
// conditions; every job the inherited verdict replaces is skipped exactly
// when it is inherited and is the merge-gate's INHERITED_JOBS; every other
// needed job is a kept job that never reads the inheritance; the classifier,
// its pre-check and its proof step are wired as designed; and the queue
// policy opts in with the same aggregator and full-suite job.
func TestVerdictInheritanceWiring(t *testing.T) {
	t.Parallel()
	fixture := loadVerdictInheritanceFixture(t)
	root := releaseWorkflowRepoRoot(t)
	jobs := verdictWorkflowJobs(t, fixture)

	markerNames := make([]string, 0, len(fixture.MarkerSteps))
	for _, m := range fixture.MarkerSteps {
		markerNames = append(markerNames, m.Name)
	}
	if !slices.Equal(markerNames, fixture.VerdictMarkers.Names) {
		t.Fatalf("marker_steps %q differ from infra's marker set version %d %q", markerNames, fixture.VerdictMarkers.Version, fixture.VerdictMarkers.Names)
	}
	aggregator := yamlMappingValue(jobs, fixture.AggregatorJob)
	if aggregator == nil {
		t.Fatalf("jobs.%s is missing", fixture.AggregatorJob)
	}
	if yamlMappingValue(aggregator, "continue-on-error") != nil {
		t.Errorf("jobs.%s must not continue on error", fixture.AggregatorJob)
	}
	steps := yamlMappingValue(aggregator, "steps")
	if steps == nil || len(steps.Content) < len(fixture.MarkerSteps) {
		t.Fatalf("jobs.%s has fewer steps than the %d marker steps", fixture.AggregatorJob, len(fixture.MarkerSteps))
	}
	tail := steps.Content[len(steps.Content)-len(fixture.MarkerSteps):]
	for i, m := range fixture.MarkerSteps {
		if got := verdictScalar(tail[i], "name"); got != m.Name {
			t.Errorf("jobs.%s final step %d is %q, want %q", fixture.AggregatorJob, i, got, m.Name)
		}
		if got := verdictScalar(tail[i], "if"); got != m.If {
			t.Errorf("marker step %q if = %q, want %q", m.Name, got, m.If)
		}
	}

	inheritedNames := make([]string, 0, len(fixture.InheritedJobs))
	for _, h := range fixture.InheritedJobs {
		inheritedNames = append(inheritedNames, h.Job)
	}
	requireVerdictCaseNames(t, verdictInheritanceJobsManifest, inheritedNames, "inherited jobs")
	if !slices.Contains(inheritedNames, fixture.FullSuiteJob) {
		t.Errorf("the full-suite job %s is not an inherited job", fixture.FullSuiteJob)
	}
	var gate *yaml.Node
	for _, step := range steps.Content {
		if verdictScalar(step, "name") == fixture.MergeGateStep {
			gate = step
		}
	}
	gateEnv := yamlMappingValue(gate, "env")
	if got := strings.Fields(verdictScalar(gateEnv, "INHERITED_JOBS")); !slices.Equal(got, inheritedNames) {
		t.Errorf("merge-gate INHERITED_JOBS = %q, want the fixture's inherited_jobs %q", got, inheritedNames)
	}
	if got := strings.Fields(verdictScalar(gateEnv, "RELEASE_JOBS")); !slices.Equal(got, fixture.ReleaseJobs) {
		t.Errorf("merge-gate RELEASE_JOBS = %q, want the fixture's release_jobs %q", got, fixture.ReleaseJobs)
	}
	for _, h := range fixture.InheritedJobs {
		job := yamlMappingValue(jobs, h.Job)
		if job == nil {
			t.Errorf("jobs.%s is missing", h.Job)
			continue
		}
		if got := normalizeCondition(verdictScalar(job, "if")); got != h.If {
			t.Errorf("jobs.%s if = %q, want %q", h.Job, got, h.If)
		}
	}

	keptNames := make([]string, 0, len(fixture.KeptJobs))
	for _, k := range fixture.KeptJobs {
		if k.Reason == "" {
			t.Errorf("kept job %s gives no reason it runs despite an inherited verdict", k.Job)
		}
		keptNames = append(keptNames, k.Job)
		job := yamlMappingValue(jobs, k.Job)
		if job == nil {
			t.Errorf("jobs.%s is missing", k.Job)
			continue
		}
		if strings.Contains(verdictScalar(job, "if"), "outputs.inherit") {
			t.Errorf("jobs.%s is a kept job but its if reads the inheritance output", k.Job)
		}
	}
	for _, job := range fixture.ReleaseJobs {
		if !slices.Contains(keptNames, job) {
			t.Errorf("release job %s is not a kept job; a release group must run it", job)
		}
	}
	wantNeeds := append(append([]string{fixture.ClassifierJob}, inheritedNames...), keptNames...)
	if needs := workflowNeedsValues(yamlMappingValue(aggregator, "needs")); !sameStringSet(needs, wantNeeds) {
		t.Errorf("jobs.%s needs %q; want exactly the classifier, the inherited jobs and the kept jobs %q", fixture.AggregatorJob, needs, wantNeeds)
	}

	classifier := yamlMappingValue(jobs, fixture.ClassifierJob)
	if classifier == nil {
		t.Fatalf("jobs.%s is missing", fixture.ClassifierJob)
	}
	if yamlMappingValue(classifier, "if") != nil || yamlMappingValue(classifier, "continue-on-error") != nil {
		t.Errorf("jobs.%s must always run: no if and no continue-on-error", fixture.ClassifierJob)
	}
	permissions := yamlMappingValue(classifier, "permissions")
	for scope, level := range fixture.ClassifierPermissions {
		if got := verdictScalar(permissions, scope); got != level {
			t.Errorf("jobs.%s permission %s = %q, want %q", fixture.ClassifierJob, scope, got, level)
		}
	}
	toolSteps := make(map[string]bool, len(fixture.ToolSteps))
	for _, name := range fixture.ToolSteps {
		toolSteps[name] = false
	}
	for _, step := range yamlMappingValue(classifier, "steps").Content {
		name := verdictScalar(step, "name")
		buildsTool := strings.Contains(verdictScalar(step, "run"), "go build") ||
			verdictScalar(yamlMappingValue(step, "with"), "repository") == "peasant-labs/infra" ||
			strings.Contains(verdictScalar(step, "uses"), "setup-go")
		_, listed := toolSteps[name]
		switch {
		case listed && !buildsTool:
			t.Errorf("jobs.%s step %q is a tool_steps row but neither fetches nor builds the tool", fixture.ClassifierJob, name)
		case !listed && buildsTool:
			t.Errorf("jobs.%s step %q fetches or builds the tool but is not a tool_steps row gated on the pre-check", fixture.ClassifierJob, name)
		case listed:
			toolSteps[name] = true
			if got := verdictScalar(step, "if"); got != fixture.ToolIf {
				t.Errorf("jobs.%s step %q if = %q, want the pre-check %q", fixture.ClassifierJob, name, got, fixture.ToolIf)
			}
		}
	}
	for name, present := range toolSteps {
		if !present {
			t.Errorf("jobs.%s has no tool step %q", fixture.ClassifierJob, name)
		}
	}

	configData, err := os.ReadFile(filepath.Join(root, fixture.ReleaseConfig))
	if err != nil {
		t.Fatalf("read %s: %v", fixture.ReleaseConfig, err)
	}
	var config struct {
		BotBranches struct {
			Release string `yaml:"release"`
		} `yaml:"bot_branches"`
	}
	if err := yaml.Unmarshal(configData, &config); err != nil {
		t.Fatalf("parse %s: %v", fixture.ReleaseConfig, err)
	}
	precheck := verdictJobStepByID(t, jobs, fixture.ClassifierJob, fixture.PrecheckStep)
	if got := verdictScalar(yamlMappingValue(precheck, "env"), "RELEASE_BRANCH"); config.BotBranches.Release == "" || got != config.BotBranches.Release {
		t.Errorf("pre-check RELEASE_BRANCH = %q, want %s bot_branches.release %q", got, fixture.ReleaseConfig, config.BotBranches.Release)
	}

	inherit := verdictJobStepByID(t, jobs, fixture.ClassifierJob, fixture.InheritStep)
	if yamlMappingValue(inherit, "if") != nil || yamlMappingValue(inherit, "continue-on-error") != nil {
		t.Errorf("jobs.%s step %s must have no if and no continue-on-error", fixture.ClassifierJob, fixture.InheritStep)
	}
	if !strings.Contains(verdictScalar(inherit, "run"), "release-intent\" inherit-verdict") {
		t.Errorf("jobs.%s step %s does not run release-intent inherit-verdict", fixture.ClassifierJob, fixture.InheritStep)
	}

	policyData, err := os.ReadFile(filepath.Join(root, fixture.PolicyFile))
	if err != nil {
		t.Fatalf("read %s: %v", fixture.PolicyFile, err)
	}
	var policy struct {
		Queue struct {
			InheritVerdict struct {
				Context      string `yaml:"context"`
				FullSuiteJob string `yaml:"full_suite_job"`
			} `yaml:"inherit_verdict"`
		} `yaml:"queue"`
	}
	if err := yaml.Unmarshal(policyData, &policy); err != nil {
		t.Fatalf("parse %s: %v", fixture.PolicyFile, err)
	}
	if got := policy.Queue.InheritVerdict; got.Context != fixture.AggregatorJob || got.FullSuiteJob != fixture.FullSuiteJob {
		t.Errorf("%s inherit_verdict = %+v, want context %s and full_suite_job %s", fixture.PolicyFile, got, fixture.AggregatorJob, fixture.FullSuiteJob)
	}
}
