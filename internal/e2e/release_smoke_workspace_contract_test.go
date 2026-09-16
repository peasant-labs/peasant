package e2e

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestReleaseSmokeStagesCleanWorkspace pins the persistent-workspace contract
// for the post-publish smoke job. The self-hosted pool keeps a runner's
// workspace between jobs, so the archive download and extraction must happen in
// a fresh job-scoped directory: a same-version rerun or an older archive left
// behind by another job would otherwise make the download refuse the existing
// filename or make the archive glob match more than one file.
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
	for _, required := range []string{"mktemp -d", "RUNNER_TEMP", "SMOKE_DIR", "GITHUB_ENV"} {
		if !strings.Contains(staging, required) {
			t.Fatalf("release.yml: smoke staging step must create a job-scoped directory (%q missing)", required)
		}
	}

	download := workflowStepNode(t, steps.Content, "Download the released archive")
	workingDirectory := yamlMappingValue(download, "working-directory")
	if workingDirectory == nil || !strings.Contains(workingDirectory.Value, "env.SMOKE_DIR") {
		t.Fatalf("release.yml: smoke download step must run inside the staging directory, got %v", workingDirectory)
	}

	assertStepBefore(t, steps.Content, "Stage a clean download directory", "Download the released archive")
}
