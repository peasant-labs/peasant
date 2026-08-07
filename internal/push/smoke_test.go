package push

import (
	"bytes"
	_ "embed"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/tuitest"
	"gopkg.in/yaml.v3"
)

// This is a program-level smoke test: it drives the push wizard end to end
// through the exact Update loop the mounted program runs, on the hermetic
// NewPushWizard([]PushWizardSession) seam (fixture sessions only — no village,
// auth, or network). It proves the wizard launches, renders a non-empty first
// frame, processes a scripted key walk across all four pages without panicking,
// and hands back the confirmation and selection the walk implies.

//go:embed testdata/smoke/walk.yaml
var pushWalkFixtureBytes []byte

// pushWalkFloor is the minimum number of scripted walk scenarios the fixture
// must hold. It equals the current scenario count rather than a smaller
// hand-picked minimum, so a scenario deleted alongside its matching decrement
// of expectedScenarioCount is still caught: the exact-match check above passes
// on that edit alone, but the floor below does not move with the corpus.
const pushWalkFloor = 3

type pushWalkDocument struct {
	ExpectedScenarioCount int                `yaml:"expectedScenarioCount"`
	Scenarios             []pushWalkScenario `yaml:"scenarios"`
}

type pushWalkScenario struct {
	Name            string   `yaml:"name"`
	Keys            []string `yaml:"keys"`
	WantConfirmed   bool     `yaml:"wantConfirmed"`
	WantSelectedIds []string `yaml:"wantSelectedIds"`
}

func loadPushWalkFixture(t *testing.T) []pushWalkScenario {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(pushWalkFixtureBytes))
	decoder.KnownFields(true)
	var doc pushWalkDocument
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode push walk fixture: %v", err)
	}
	if len(doc.Scenarios) == 0 || doc.ExpectedScenarioCount != len(doc.Scenarios) {
		t.Fatalf("declared and actual scenario counts must match and be non-zero: expectedScenarioCount=%d scenarios=%d",
			doc.ExpectedScenarioCount, len(doc.Scenarios))
	}
	if len(doc.Scenarios) < pushWalkFloor {
		t.Fatalf("push smoke fixture testdata/smoke/walk.yaml holds %d scenario(s), below the floor of %d; "+
			"this fixture is the only scripted proof the push wizard's 4-page walk still confirms and selects "+
			"sessions correctly, so a scenario removed here (even with expectedScenarioCount edited to match) "+
			"silently shrinks that coverage; to fix, restore the missing scenario, or if the coverage drop is "+
			"deliberate, lower pushWalkFloor in this file and say in the fixture header which walk stopped being covered",
			len(doc.Scenarios), pushWalkFloor)
	}
	return doc.Scenarios
}

func TestPushWizard_ScriptedFourPageWalk(t *testing.T) {
	for _, scenario := range loadPushWalkFixture(t) {
		t.Run(scenario.Name, func(t *testing.T) {
			m := NewPushWizard(testSessions())

			// A launched program must render something to read.
			if got := m.View().Content; got == "" {
				t.Fatal("first frame is empty; the wizard rendered nothing on launch")
			}

			for i, token := range scenario.Keys {
				updated, _ := m.Update(tuitest.Key(token))
				next, ok := updated.(PushWizardModel)
				if !ok {
					t.Fatalf("key %d (%q): Update returned %T, not a PushWizardModel", i, token, updated)
				}
				m = next
			}

			if got := m.Confirmed(); got != scenario.WantConfirmed {
				t.Errorf("Confirmed() = %v, want %v after walk %v", got, scenario.WantConfirmed, scenario.Keys)
			}
			if got := sortedCopy(m.SelectedSessionIDs()); !equalStrings(got, sortedCopy(scenario.WantSelectedIds)) {
				t.Errorf("SelectedSessionIDs() = %v, want %v", got, scenario.WantSelectedIds)
			}
		})
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
