package keymap_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
)

//go:embed testdata/help_dispatchable.yaml
var helpDispatchableFixtureData []byte

type helpDispatchableDocument struct {
	ExpectedScenarioCount int                    `yaml:"expectedScenarioCount"`
	Scenarios             []helpDispatchableCase `yaml:"scenarios"`
}

type helpDispatchableCase struct {
	Name      string   `yaml:"name"`
	Available []string `yaml:"available"`
}

func loadHelpDispatchableFixture(data []byte) (helpDispatchableDocument, error) {
	var doc helpDispatchableDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/help_dispatchable.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/help_dispatchable.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedScenarioCount != len(doc.Scenarios) || len(doc.Scenarios) == 0 {
		return doc, fmt.Errorf(
			"testdata/help_dispatchable.yaml: expectedScenarioCount=%d but found %d scenarios (and must be non-zero)",
			doc.ExpectedScenarioCount, len(doc.Scenarios))
	}
	seen := map[string]bool{}
	for _, s := range doc.Scenarios {
		if s.Name == "" || seen[s.Name] {
			return doc, fmt.Errorf("testdata/help_dispatchable.yaml: scenario name %q is missing or duplicated", s.Name)
		}
		seen[s.Name] = true
	}
	return doc, nil
}

// TestHelpEntries_EqualsDispatchableSet is the fixture-driven proof that
// keymap.HelpEntries lists EXACTLY the actions keymap.Match can actually
// dispatch for the same Keymap+Availability - not a separate, potentially
// drifted definition. For every scenario it:
//
//  1. asserts HelpEntries returns one entry per available action, in the
//     same order (nothing missing, nothing extra, nothing reordered);
//  2. for each entry, independently re-derives dispatchability by building
//     a real tea.KeyPressMsg for the entry's help key and calling the
//     production Match function - the SAME function a kit component calls
//     to dispatch a real key press - and asserts Match resolves it back to
//     that exact action.
//
// This is the architectural fix for the drift the audit found in
// internal/tui/ftue/help.go (help rows built from key.NewBinding calls with
// no WithKeys, so the help overlay could show a key no press could ever
// dispatch): here, every row is verified against the real dispatcher.
func TestHelpEntries_EqualsDispatchableSet(t *testing.T) {
	t.Parallel()
	doc, err := loadHelpDispatchableFixture(helpDispatchableFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	km := keymap.Default()

	for _, scenario := range doc.Scenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			t.Parallel()
			avail := availabilityFromNames(t, scenario.Available)
			entries := keymap.HelpEntries(km, avail)

			if len(entries) != len(avail) {
				t.Fatalf("HelpEntries returned %d entries, want %d (one per available action in scenario %q); "+
					"every action in this scenario is bound and enabled in keymap.Default(), so none should be "+
					"dropped", len(entries), len(avail), scenario.Name)
			}
			for i, entry := range entries {
				wantAction := avail[i]
				if entry.Action != wantAction {
					t.Fatalf("HelpEntries()[%d] = %s, want %s (scenario %q; entries must preserve Availability order)",
						i, entry.Action, wantAction, scenario.Name)
				}
				binding := km[entry.Action]
				if entry.Key != binding.Help().Key || entry.Desc != binding.Help().Desc {
					t.Errorf("HelpEntries()[%d] help text = {%q %q}, want {%q %q} (scenario %q)",
						i, entry.Key, entry.Desc, binding.Help().Key, binding.Help().Desc, scenario.Name)
				}

				// Independent re-derivation: press the entry's own primary
				// key through the REAL Match function and confirm it
				// resolves back to this exact action.
				msg := msgForKeyString(t, binding.Keys()[0])
				gotAction, ok := keymap.Match(km, msg, avail)
				if !ok {
					t.Fatalf("Match(...) could not dispatch the primary key %q for help entry %s (scenario %q) - "+
						"HelpEntries listed an action Match cannot actually dispatch", binding.Keys()[0], entry.Action, scenario.Name)
				}
				if gotAction != entry.Action {
					t.Errorf("Match(...) for key %q resolved to %s, want %s (scenario %q) - help and dispatch "+
						"disagree on what this key does", binding.Keys()[0], gotAction, entry.Action, scenario.Name)
				}
			}
		})
	}
}
