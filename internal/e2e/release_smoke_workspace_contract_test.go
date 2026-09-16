package e2e

import (
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/workflows/release_workspace_contract.yaml
var releaseWorkspaceContractBytes []byte

type releaseWorkspaceContractFixture struct {
	Smoke struct {
		StagingStep          string          `yaml:"staging_step"`
		StagingDirExpression string          `yaml:"staging_dir_expression"`
		StagingRunContains   nonEmptyStrings `yaml:"staging_run_contains"`
		StepsInsideStaging   nonEmptyStrings `yaml:"steps_inside_staging"`
	} `yaml:"smoke"`
	Packaging struct {
		ResetStep    string          `yaml:"reset_step"`
		ResetRun     string          `yaml:"reset_run"`
		DownloadStep string          `yaml:"download_step"`
		Jobs         nonEmptyStrings `yaml:"jobs"`
	} `yaml:"packaging"`
}

func loadReleaseWorkspaceContractFixture(t *testing.T) releaseWorkspaceContractFixture {
	t.Helper()
	var fixture releaseWorkspaceContractFixture
	if err := yaml.Unmarshal(releaseWorkspaceContractBytes, &fixture); err != nil {
		t.Fatalf("parse testdata/workflows/release_workspace_contract.yaml: %v", err)
	}
	if strings.TrimSpace(fixture.Smoke.StagingStep) == "" ||
		strings.TrimSpace(fixture.Smoke.StagingDirExpression) == "" ||
		len(fixture.Smoke.StagingRunContains) == 0 ||
		len(fixture.Smoke.StepsInsideStaging) == 0 {
		t.Fatalf("release workspace fixture is incomplete: %+v", fixture.Smoke)
	}
	if strings.TrimSpace(fixture.Packaging.ResetStep) == "" ||
		strings.TrimSpace(fixture.Packaging.ResetRun) == "" ||
		strings.TrimSpace(fixture.Packaging.DownloadStep) == "" ||
		len(fixture.Packaging.Jobs) == 0 {
		t.Fatalf("release workspace fixture is incomplete: %+v", fixture.Packaging)
	}
	return fixture
}

func workflowJobSteps(t *testing.T, doc *yaml.Node, jobName, context string) []*yaml.Node {
	t.Helper()
	job := yamlMappingValue(yamlMappingValue(doc, "jobs"), jobName)
	if job == nil {
		t.Fatalf("%s: workflow job %q is missing", context, jobName)
	}
	steps := yamlMappingValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		t.Fatalf("%s: workflow job %q has no steps", context, jobName)
	}
	return steps.Content
}

// TestReleaseSmokeStagesCleanWorkspace pins the persistent-workspace contract
// for the post-publish smoke job. The self-hosted pool keeps a runner's
// workspace between jobs, so the archive download, extraction, and assertions
// must all happen in a fresh job-scoped directory: a same-version rerun or an
// older archive left behind by another job would otherwise make the download
// refuse the existing filename or make the archive glob match more than one
// file. Expectations live in testdata/workflows/release_workspace_contract.yaml.
func TestReleaseSmokeStagesCleanWorkspace(t *testing.T) {
	fixture := loadReleaseWorkspaceContractFixture(t)
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release.yml"))
	steps := workflowJobSteps(t, doc, "smoke", "release.yml")

	staging := workflowStepRun(t, steps, fixture.Smoke.StagingStep)
	for _, required := range fixture.Smoke.StagingRunContains {
		if !strings.Contains(staging, required) {
			t.Fatalf("release.yml: smoke staging step must create a job-scoped directory (%q missing)", required)
		}
	}

	for _, stepName := range fixture.Smoke.StepsInsideStaging {
		step := workflowStepNode(t, steps, stepName)
		workingDirectory := yamlMappingValue(step, "working-directory")
		if workingDirectory == nil || !strings.Contains(workingDirectory.Value, fixture.Smoke.StagingDirExpression) {
			t.Fatalf("release.yml: smoke step %q must run inside the staging directory, got %v", stepName, workingDirectory)
		}
	}

	assertStepBefore(t, steps, fixture.Smoke.StagingStep, fixture.Smoke.StepsInsideStaging[0])
}

// TestReleasePackagingResetsDistDirectory pins the persistent-workspace reset
// for the packaging jobs that consume the goreleaser artifact on the pool. The
// download action overwrites matching filenames but leaves differently named
// files from earlier jobs, so each job must clear its own dist/ directory
// before downloading; a package glob that then matches two versions fails the
// job. The reset step must exist and run before the download.
func TestReleasePackagingResetsDistDirectory(t *testing.T) {
	fixture := loadReleaseWorkspaceContractFixture(t)
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release-validate.yml"))
	for _, jobName := range fixture.Packaging.Jobs {
		steps := workflowJobSteps(t, doc, jobName, "release-validate.yml")
		reset := workflowStepRun(t, steps, fixture.Packaging.ResetStep)
		if strings.TrimSpace(reset) != fixture.Packaging.ResetRun {
			t.Fatalf("release-validate.yml: job %q must clear its owned dist/ directory, got %q", jobName, reset)
		}
		assertStepBefore(t, steps, fixture.Packaging.ResetStep, fixture.Packaging.DownloadStep)
	}
}
