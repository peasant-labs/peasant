package ftue

import (
	"bytes"
	_ "embed"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tuitest"
	"gopkg.in/yaml.v3"
)

// This is a program-level smoke test for the setup wizard (peasant kickstart).
// It drives the mounted WizardModel through the same Update loop the program
// runs, over a fixture discovery inventory, proving the wizard launches with a
// non-empty first frame, advances across more than one page under a scripted key
// walk, and exits cleanly on interrupt — all on the v2 terminal stack.

//go:embed testdata/smoke/wizard.yaml
var wizardWalkFixtureBytes []byte

// wizardWalkFloor is the minimum number of scripted page-walk scenarios the
// fixture must hold. It equals the current scenario count rather than a
// smaller hand-picked minimum, so a scenario deleted alongside its matching
// decrement of expectedScenarioCount is still caught: the exact-match check
// above passes on that edit alone, but the floor below does not move with
// the corpus.
const wizardWalkFloor = 1

type wizardWalkDocument struct {
	ExpectedScenarioCount int                  `yaml:"expectedScenarioCount"`
	Scenarios             []wizardWalkScenario `yaml:"scenarios"`
}

type wizardWalkScenario struct {
	Name             string           `yaml:"name"`
	Sessions         []SessionListing `yaml:"sessions"`
	Keys             []string         `yaml:"keys"`
	MinPagesAdvanced int              `yaml:"minPagesAdvanced"`
}

func loadWizardWalkFixture(t *testing.T) []wizardWalkScenario {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(wizardWalkFixtureBytes))
	decoder.KnownFields(true)
	var doc wizardWalkDocument
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode wizard walk fixture: %v", err)
	}
	if len(doc.Scenarios) == 0 || doc.ExpectedScenarioCount != len(doc.Scenarios) {
		t.Fatalf("declared and actual scenario counts must match and be non-zero: expectedScenarioCount=%d scenarios=%d",
			doc.ExpectedScenarioCount, len(doc.Scenarios))
	}
	if len(doc.Scenarios) < wizardWalkFloor {
		t.Fatalf("setup wizard smoke fixture testdata/smoke/wizard.yaml holds %d scenario(s), below the floor of %d; "+
			"this fixture is the only scripted proof the kickstart wizard's page navigation and interrupt still "+
			"work, so a scenario removed here (even with expectedScenarioCount edited to match) silently shrinks "+
			"that coverage; to fix, restore the missing scenario, or if the coverage drop is deliberate, lower "+
			"wizardWalkFloor in this file and say in the fixture header which walk stopped being covered",
			len(doc.Scenarios), wizardWalkFloor)
	}
	return doc.Scenarios
}

// inventoryFromSessions derives the provider discovery inventory the wizard
// consumes from a fixture session list: one operational entry per harness,
// counting the sessions it owns.
func inventoryFromSessions(sessions []SessionListing) ProviderInventory {
	counts := map[string]int{}
	for _, s := range sessions {
		counts[s.Harness]++
	}
	inv := ProviderInventory{}
	for harness, count := range counts {
		inv[defaults.Harness(harness)] = ProviderDiscovery{SessionCount: count, Enabled: true}
	}
	return inv
}

func TestSetupWizard_ScriptedPageWalk(t *testing.T) {
	for _, scenario := range loadWizardWalkFixture(t) {
		t.Run(scenario.Name, func(t *testing.T) {
			var m tea.Model = NewWizard(
				WithSessions(scenario.Sessions),
				WithProviderInventory(inventoryFromSessions(scenario.Sessions)),
			)
			m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

			if got := m.(WizardModel).View().Content; got == "" {
				t.Fatal("first frame is empty; the wizard rendered nothing on launch")
			}

			startPage := m.(WizardModel).current
			maxPage := startPage
			for i, token := range scenario.Keys {
				updated, _ := m.Update(tuitest.Key(token))
				next, ok := updated.(WizardModel)
				if !ok {
					t.Fatalf("key %d (%q): Update returned %T, not a WizardModel", i, token, updated)
				}
				m = next
				if next.current > maxPage {
					maxPage = next.current
				}
				if frame := next.View().Content; frame == "" {
					t.Fatalf("key %d (%q): frame is empty", i, token)
				}
			}

			if advanced := maxPage - startPage; advanced < scenario.MinPagesAdvanced {
				t.Errorf("walk advanced %d page(s) (from %d to %d), want at least %d",
					advanced, startPage, maxPage, scenario.MinPagesAdvanced)
			}

			// Interrupt exits immediately with a quit command.
			updated, quitCmd := m.Update(tuitest.Key("ctrl+c"))
			final := updated.(WizardModel)
			if !final.quitting {
				t.Error("ctrl+c did not set the wizard quitting")
			}
			if quitCmd == nil {
				t.Error("ctrl+c returned no command; expected the program to signal exit")
			}
		})
	}
}
