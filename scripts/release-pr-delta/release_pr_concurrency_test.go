package main

import (
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/release_pr_concurrency.yaml
var releasePRConcurrencyFixtureBytes []byte

type releasePRConcurrencyExpectation struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type releasePRConcurrencyFixture struct {
	Expectations []releasePRConcurrencyExpectation `yaml:"expectations"`
}

type releasePRConcurrencyWorkflow struct {
	On struct {
		PullRequest struct {
			Types []string `yaml:"types"`
		} `yaml:"pull_request"`
	} `yaml:"on"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress string `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]struct {
		If string `yaml:"if"`
	} `yaml:"jobs"`
}

func loadReleasePRConcurrencyFixture(t *testing.T) map[string]string {
	t.Helper()
	var fixture releasePRConcurrencyFixture
	if err := yaml.Unmarshal(releasePRConcurrencyFixtureBytes, &fixture); err != nil {
		t.Fatalf("parse testdata/release_pr_concurrency.yaml: %v", err)
	}
	wantNames := map[string]bool{"group": false, "cancel-in-progress": false, "validate-if": false, "required-trigger": false}
	got := make(map[string]string, len(fixture.Expectations))
	for _, expectation := range fixture.Expectations {
		if strings.TrimSpace(expectation.Name) == "" || strings.TrimSpace(expectation.Value) == "" {
			t.Fatalf("testdata/release_pr_concurrency.yaml entry %+v must name a non-empty value", expectation)
		}
		if _, known := wantNames[expectation.Name]; !known || got[expectation.Name] != "" {
			t.Fatalf("testdata/release_pr_concurrency.yaml has unknown or repeated entry %q", expectation.Name)
		}
		got[expectation.Name] = expectation.Value
	}
	for name := range wantNames {
		if got[name] == "" {
			t.Fatalf("testdata/release_pr_concurrency.yaml is missing entry %q", name)
		}
	}
	return got
}

func normalizeReleasePRCondition(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// TestReleasePRConcurrencyGuard pins the release-pr.yml concurrency contract:
// the merge run has its own group and is never cancelled, a body edit queues
// behind running gates, and validate only runs on an open PR.
func TestReleasePRConcurrencyGuard(t *testing.T) {
	expect := loadReleasePRConcurrencyFixture(t)
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release-pr.yml"))
	if err != nil {
		t.Fatalf("read release-pr.yml: %v", err)
	}
	var workflow releasePRConcurrencyWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatalf("parse release-pr.yml: %v", err)
	}
	if workflow.Concurrency.Group != expect["group"] {
		t.Fatalf("release-pr.yml concurrency.group = %q, want %q", workflow.Concurrency.Group, expect["group"])
	}
	if workflow.Concurrency.CancelInProgress != expect["cancel-in-progress"] {
		t.Fatalf("release-pr.yml concurrency.cancel-in-progress = %q, want %q", workflow.Concurrency.CancelInProgress, expect["cancel-in-progress"])
	}
	validate, ok := workflow.Jobs["validate"]
	if !ok {
		t.Fatal("release-pr.yml has no job validate")
	}
	if normalizeReleasePRCondition(validate.If) != normalizeReleasePRCondition(expect["validate-if"]) {
		t.Fatalf("release-pr.yml jobs.validate.if = %q, want %q", validate.If, expect["validate-if"])
	}
	for _, trigger := range workflow.On.PullRequest.Types {
		if trigger == expect["required-trigger"] {
			return
		}
	}
	t.Fatalf("release-pr.yml on.pull_request.types = %v, want it to contain %q", workflow.On.PullRequest.Types, expect["required-trigger"])
}
