// Package branchruleoverlay is the one loader of the overlay on the captured
// legacy wizard goldens.
//
// The legacy wizard saved a project chosen at project level with no branch list.
// The setup now saves the branches the person chose, so a captured scenario that
// chooses a project at project level is measured against its overlay entry
// instead: the captured golden with the branch list added and nothing else
// changed. The captured goldens (internal/tui/ftue/testdata/equivalence) record
// the old wizard and are not edited.
//
// Two test packages measure the rebuild against the captures, the terminal's
// (internal/tui/kickstart) and the neutral model's (internal/onboarding). Both
// load the overlay here, so the overlay is decoded strictly once and held to its
// rules once: an entry names a captured scenario, says why, differs from the
// capture by a branch list only, and every required entry is present.
package branchruleoverlay

import (
	_ "embed"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/legacy_branch_rule.yaml
var legacyBranchRuleData []byte

type document struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []entry  `yaml:"cases"`
}

type entry struct {
	Name     string                 `yaml:"name"`
	Reason   string                 `yaml:"reason"`
	Expected config.SelectionConfig `yaml:"expected"`
}

// Overlay is the decoded, checked overlay.
type Overlay struct {
	expected map[string]config.SelectionConfig
}

// Load decodes the embedded overlay strictly and checks every entry against
// captured, the selection each captured scenario's golden holds, by scenario
// name. It returns an actionable error for an unknown field, a duplicate or
// missing required name, an entry for a scenario that was not captured, an entry
// with no reason, an entry that differs from its capture by more than a branch
// list, and an entry equal to its capture, which would overlay nothing.
func Load(captured map[string]config.SelectionConfig) (Overlay, error) {
	return load(legacyBranchRuleData, captured)
}

func load(data []byte, captured map[string]config.SelectionConfig) (Overlay, error) {
	var doc document
	if err := testutil.DecodeNamedFixtureYAML(data, &doc); err != nil {
		return Overlay{}, fmt.Errorf("decode the legacy branch rule overlay: %w", err)
	}
	overlay := Overlay{expected: make(map[string]config.SelectionConfig, len(doc.Cases))}
	for _, c := range doc.Cases {
		golden, known := captured[c.Name]
		if !known {
			return Overlay{}, fmt.Errorf("the legacy branch rule overlay names %q, which the captured goldens do not hold: remove the entry or capture the scenario", c.Name)
		}
		if strings.TrimSpace(c.Reason) == "" {
			return Overlay{}, fmt.Errorf("the legacy branch rule overlay entry %q gives no reason: say why the capture is overlaid", c.Name)
		}
		if !reflect.DeepEqual(canonical(c.Expected, false), canonical(golden, false)) {
			return Overlay{}, fmt.Errorf("the legacy branch rule overlay entry %q differs from its capture by more than a branch list:\n overlay  = %+v\n captured = %+v", c.Name, c.Expected, golden)
		}
		if reflect.DeepEqual(canonical(c.Expected, true), canonical(golden, true)) {
			return Overlay{}, fmt.Errorf("the legacy branch rule overlay entry %q equals its capture, so it overlays nothing: remove the entry", c.Name)
		}
		overlay.expected[c.Name] = c.Expected
	}
	return overlay, nil
}

// Want is the selection the rebuild must save for a captured scenario: the
// overlay's entry when there is one, else the capture itself.
func (o Overlay) Want(name string, captured config.SelectionConfig) config.SelectionConfig {
	if expected, overlaid := o.expected[name]; overlaid {
		return expected
	}
	return captured
}

// canonical orders every list of a selection so two selections that hold the
// same choices compare equal, and drops the branch lists when withBranches is
// false.
func canonical(selection config.SelectionConfig, withBranches bool) config.SelectionConfig {
	out := config.SelectionConfig{Mode: selection.Mode, AutoIngestNewBranches: selection.AutoIngestNewBranches}
	if len(selection.Harnesses) == 0 {
		return out
	}
	out.Harnesses = make(map[string]config.SelectionHarnessConfig, len(selection.Harnesses))
	for name, harness := range selection.Harnesses {
		projects := make([]config.ProjectSelection, len(harness.Projects))
		for index, project := range harness.Projects {
			project.ClonePaths = sortedOrNil(project.ClonePaths)
			if withBranches {
				project.Branches = sortedOrNil(project.Branches)
			} else {
				project.Branches = nil
			}
			projects[index] = project
		}
		sort.Slice(projects, func(i, j int) bool {
			if projects[i].GitRemote != projects[j].GitRemote {
				return projects[i].GitRemote < projects[j].GitRemote
			}
			if projects[i].Name != projects[j].Name {
				return projects[i].Name < projects[j].Name
			}
			return strings.Join(projects[i].ClonePaths, "\x00") < strings.Join(projects[j].ClonePaths, "\x00")
		})
		if len(projects) == 0 {
			projects = nil
		}
		out.Harnesses[name] = config.SelectionHarnessConfig{Projects: projects, Sessions: sortedOrNil(harness.Sessions), Exclusions: harness.Exclusions}
	}
	return out
}

func sortedOrNil(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return sorted
}
