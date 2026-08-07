package tui_test

import (
	"bytes"
	_ "embed"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui"
	"github.com/peasant-labs/peasant/internal/tuitest"
	"gopkg.in/yaml.v3"
)

// This is a program-level smoke test for the analytics app (peasant tui). It
// drives the mounted AppModel through the same Update loop the program runs,
// proving it launches, renders a non-empty first frame, changes what it draws
// as the user cycles tabs, and exits cleanly on the quit key — all on the
// v2 terminal stack.

//go:embed testdata/smoke/analytics.yaml
var analyticsWalkFixtureBytes []byte

// analyticsWalkFloor is the minimum number of scripted tab-walk scenarios the
// fixture must hold. It equals the current scenario count rather than a
// smaller hand-picked minimum, so a scenario deleted alongside its matching
// decrement of expectedScenarioCount is still caught: the exact-match check
// above passes on that edit alone, but the floor below does not move with
// the corpus.
const analyticsWalkFloor = 2

type analyticsWalkDocument struct {
	ExpectedScenarioCount int                     `yaml:"expectedScenarioCount"`
	Scenarios             []analyticsWalkScenario `yaml:"scenarios"`
}

type analyticsWalkScenario struct {
	Name    string   `yaml:"name"`
	Keys    []string `yaml:"keys"`
	QuitKey string   `yaml:"quitKey"`
}

func loadAnalyticsWalkFixture(t *testing.T) []analyticsWalkScenario {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(analyticsWalkFixtureBytes))
	decoder.KnownFields(true)
	var doc analyticsWalkDocument
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decode analytics walk fixture: %v", err)
	}
	if len(doc.Scenarios) == 0 || doc.ExpectedScenarioCount != len(doc.Scenarios) {
		t.Fatalf("declared and actual scenario counts must match and be non-zero: expectedScenarioCount=%d scenarios=%d",
			doc.ExpectedScenarioCount, len(doc.Scenarios))
	}
	if len(doc.Scenarios) < analyticsWalkFloor {
		t.Fatalf("analytics smoke fixture testdata/smoke/analytics.yaml holds %d scenario(s), below the floor of %d; "+
			"this fixture is the only scripted proof the analytics app's tab navigation and quit key still work, "+
			"so a scenario removed here (even with expectedScenarioCount edited to match) silently shrinks that "+
			"coverage; to fix, restore the missing scenario, or if the coverage drop is deliberate, lower "+
			"analyticsWalkFloor in this file and say in the fixture header which walk stopped being covered",
			len(doc.Scenarios), analyticsWalkFloor)
	}
	return doc.Scenarios
}

func TestAnalyticsApp_ScriptedTabWalk(t *testing.T) {
	for _, scenario := range loadAnalyticsWalkFixture(t) {
		t.Run(scenario.Name, func(t *testing.T) {
			var m tea.Model = tui.NewApp([]ingest.Session{testSession()})
			m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

			first := m.View().Content
			if first == "" {
				t.Fatal("first frame is empty; the app rendered nothing on launch")
			}

			frames := map[string]bool{first: true}
			for i, token := range scenario.Keys {
				updated, _ := m.Update(tuitest.Key(token))
				m = updated
				frame := m.View().Content
				if frame == "" {
					t.Fatalf("key %d (%q): frame is empty", i, token)
				}
				frames[frame] = true
			}

			// Cycling the tabs must change what is drawn — otherwise navigation is
			// silently broken even though nothing panicked.
			if len(frames) < 2 {
				t.Errorf("tab navigation produced only one distinct frame; expected the view to change across tabs")
			}

			_, quitCmd := m.Update(tuitest.Key(scenario.QuitKey))
			if quitCmd == nil {
				t.Errorf("quit key %q returned no command; expected the program to signal exit", scenario.QuitKey)
			}
		})
	}
}
