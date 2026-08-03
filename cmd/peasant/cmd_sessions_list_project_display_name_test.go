package main

import (
	_ "embed"
	"testing"

	"github.com/peasant-labs/schema/testcase"
	"github.com/peasant-labs/schema/testcase/assert"
)

//go:embed testdata/project_display_name.yaml
var projectDisplayNameYAML []byte

type projectDisplayNameInput struct {
	Remote string `yaml:"remote"`
	CWD    string `yaml:"cwd"`
}

type projectDisplayNameExpected struct {
	Project string `yaml:"project"`
}

// expectedProjectDisplayNameCaseCount is a floor (assert.RequireMin), not an
// exact count, so the corpus can grow without this guard needing an update.
const expectedProjectDisplayNameCaseCount = 5

// TestProjectDisplayName_UsesSharedFormatter proves the CLI's `peasant
// sessions list` --project column renders through the SAME
// internal/projectlabel.Label formatter every other surface (Home/Map
// picker, breadcrumbs, TUI) uses, instead of a third, CLI-only
// implementation that dropped the host prefix and truncated GitLab subgroup
// paths (a real cross-surface naming divergence flagged in review). The
// no-remote basename fallback stays CLI-specific (a terminal table column
// favors brevity over the picker's full path) — that is the one deliberate,
// documented difference.
func TestProjectDisplayName_UsesSharedFormatter(t *testing.T) {
	corpus, err := testcase.LoadCorpus[projectDisplayNameInput, projectDisplayNameExpected](projectDisplayNameYAML)
	if err != nil {
		t.Fatalf("load project display name fixture: %v", err)
	}
	assert.RequireMin(t, corpus, expectedProjectDisplayNameCaseCount)
	assert.RequireValid(t, corpus)

	for _, fixtureCase := range corpus.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			var remote *string
			if fixtureCase.Input.Remote != "" {
				remote = &fixtureCase.Input.Remote
			}
			got := projectDisplayName(remote, fixtureCase.Input.CWD)
			if got != fixtureCase.Expected.Project {
				t.Errorf("projectDisplayName(%v, %q) = %q, want %q", remote, fixtureCase.Input.CWD, got, fixtureCase.Expected.Project)
			}
		})
	}
}
