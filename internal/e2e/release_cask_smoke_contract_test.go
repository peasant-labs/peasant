package e2e

import (
	_ "embed"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/workflows/release_cask_smoke.yaml
var releaseCaskSmokeBytes []byte

type releaseCaskSmokeStep struct {
	Step        string          `yaml:"step"`
	RunContains nonEmptyStrings `yaml:"run_contains"`
}

type releaseCaskSmokeFixture struct {
	Job      string `yaml:"job"`
	Checkout struct {
		Step               string `yaml:"step"`
		Repository         string `yaml:"repository"`
		PersistCredentials string `yaml:"persist_credentials"`
	} `yaml:"checkout"`
	Render         releaseCaskSmokeStep `yaml:"render"`
	Install        releaseCaskSmokeStep `yaml:"install"`
	AssertStep     string               `yaml:"assert_step"`
	ForbiddenInJob nonEmptyStrings      `yaml:"forbidden_in_job"`
}

func loadReleaseCaskSmokeFixture(t *testing.T) releaseCaskSmokeFixture {
	t.Helper()
	var fixture releaseCaskSmokeFixture
	if err := yaml.Unmarshal(releaseCaskSmokeBytes, &fixture); err != nil {
		t.Fatalf("parse testdata/workflows/release_cask_smoke.yaml: %v", err)
	}
	if strings.TrimSpace(fixture.Job) == "" ||
		strings.TrimSpace(fixture.Checkout.Step) == "" ||
		strings.TrimSpace(fixture.Checkout.Repository) == "" ||
		strings.TrimSpace(fixture.Checkout.PersistCredentials) == "" ||
		strings.TrimSpace(fixture.Render.Step) == "" || len(fixture.Render.RunContains) == 0 ||
		strings.TrimSpace(fixture.Install.Step) == "" || len(fixture.Install.RunContains) == 0 ||
		strings.TrimSpace(fixture.AssertStep) == "" || len(fixture.ForbiddenInJob) == 0 {
		t.Fatalf("release cask smoke fixture is incomplete: %+v", fixture)
	}
	return fixture
}

// TestReleaseCaskSmokeRendersFromTheTapTemplate pins the final-tag macOS cask
// smoke to a local render of the tap's template against this release's
// checksums, installed from a throwaway tap. Waiting on the tap's scheduled
// publisher failed every final release once GitHub throttled its cron, so no
// step may read or poll the live tap's published cask.
func TestReleaseCaskSmokeRendersFromTheTapTemplate(t *testing.T) {
	t.Parallel()
	fixture := loadReleaseCaskSmokeFixture(t)
	doc := readWorkflowDoc(t, filepath.Join(releaseWorkflowRepoRoot(t), ".github", "workflows", "release.yml"))
	steps := workflowJobSteps(t, doc, fixture.Job, "release.yml")

	with := yamlMappingValue(workflowStepNode(t, steps, fixture.Checkout.Step), "with")
	assertYAMLMappingValue(t, with, "repository", fixture.Checkout.Repository, "release.yml: cask smoke tap checkout")
	assertYAMLMappingValue(t, with, "persist-credentials", fixture.Checkout.PersistCredentials, "release.yml: cask smoke tap checkout")

	for _, step := range []releaseCaskSmokeStep{fixture.Render, fixture.Install} {
		run := workflowStepRun(t, steps, step.Step)
		for _, required := range step.RunContains {
			if !strings.Contains(run, required) {
				t.Fatalf("release.yml: cask smoke step %q must contain %q", step.Step, required)
			}
		}
	}

	assertStepBefore(t, steps, fixture.Checkout.Step, fixture.Render.Step)
	assertStepBefore(t, steps, fixture.Render.Step, fixture.Install.Step)
	assertStepBefore(t, steps, fixture.Install.Step, fixture.AssertStep)

	for _, step := range steps {
		run := yamlMappingValue(step, "run")
		if run == nil {
			continue
		}
		for _, forbidden := range fixture.ForbiddenInJob {
			if strings.Contains(run.Value, forbidden) {
				t.Fatalf("release.yml: cask smoke must not depend on the live tap; a step contains %q", forbidden)
			}
		}
	}
}
