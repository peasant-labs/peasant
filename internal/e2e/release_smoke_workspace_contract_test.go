package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReleaseSmokeStagesCleanWorkspace pins the persistent-workspace contract
// for the post-publish smoke job. The self-hosted pool keeps a runner's
// workspace between jobs, so the archive download, extraction, and assertions
// must all happen in a fresh job-scoped directory: a same-version rerun or an
// older archive left behind by another job would otherwise make the download
// refuse the existing filename or make the archive glob match more than one
// file. Every step that reads the extracted archive is pinned to the staging
// directory so a partial revert cannot pass.
func TestReleaseSmokeStagesCleanWorkspace(t *testing.T) {
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release.yml"))
	job := yamlMappingValue(yamlMappingValue(doc, "jobs"), "smoke")
	if job == nil {
		t.Fatal("release.yml: workflow job \"smoke\" is missing")
	}
	steps := yamlMappingValue(job, "steps")
	if steps == nil || steps.Kind != yaml.SequenceNode {
		t.Fatal("release.yml: workflow job \"smoke\" has no steps")
	}

	staging := workflowStepRun(t, steps.Content, "Stage a clean download directory")
	for _, required := range []string{"mktemp -d", "RUNNER_TEMP", `SMOKE_DIR=$staging`, "GITHUB_ENV"} {
		if !strings.Contains(staging, required) {
			t.Fatalf("release.yml: smoke staging step must create a job-scoped directory (%q missing)", required)
		}
	}

	for _, stepName := range []string{
		"Download the released archive",
		"Assert the binary is statically linked",
		"Assert version matches the tag",
	} {
		step := workflowStepNode(t, steps.Content, stepName)
		workingDirectory := yamlMappingValue(step, "working-directory")
		if workingDirectory == nil || !strings.Contains(workingDirectory.Value, "env.SMOKE_DIR") {
			t.Fatalf("release.yml: smoke step %q must run inside the staging directory, got %v", stepName, workingDirectory)
		}
	}

	assertStepBefore(t, steps.Content, "Stage a clean download directory", "Download the released archive")
}

// TestReleasePackagingResetsDistDirectory pins the persistent-workspace reset
// for the packaging jobs that consume the goreleaser artifact on the pool. The
// download action overwrites matching filenames but leaves differently named
// files from earlier jobs, so each job must clear its own dist/ directory
// before downloading; a package glob that then matches two versions fails the
// job. The reset step must exist and run before the download.
func TestReleasePackagingResetsDistDirectory(t *testing.T) {
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release-validate.yml"))
	jobs := yamlMappingValue(doc, "jobs")
	for _, jobName := range []string{"deb", "rpm", "makepkg"} {
		job := yamlMappingValue(jobs, jobName)
		if job == nil {
			t.Fatalf("release-validate.yml: workflow job %q is missing", jobName)
		}
		steps := yamlMappingValue(job, "steps")
		if steps == nil || steps.Kind != yaml.SequenceNode {
			t.Fatalf("release-validate.yml: workflow job %q has no steps", jobName)
		}
		reset := workflowStepRun(t, steps.Content, "Reset dist/")
		if strings.TrimSpace(reset) != "rm -rf dist" {
			t.Fatalf("release-validate.yml: job %q must clear its owned dist/ directory, got %q", jobName, reset)
		}
		assertStepBefore(t, steps.Content, "Reset dist/", "Download dist/")
	}
}
