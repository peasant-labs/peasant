package kickstart_test

import (
	_ "embed"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

//go:embed testdata/selection_parity.yaml
var selectionParityData []byte

type selectionParityDocument struct {
	RequiredNames []string                   `yaml:"requiredNames"`
	Inventories   []selectionParityInventory `yaml:"inventories"`
	Cases         []selectionParityCase      `yaml:"cases"`
}

type selectionParityInventory struct {
	Name     string                  `yaml:"name"`
	Paths    []mountedPathFixture    `yaml:"paths"`
	Listings []mountedListingFixture `yaml:"listings"`
}

type selectionParityCase struct {
	Name      string                  `yaml:"name"`
	Inventory string                  `yaml:"inventory"`
	Actions   []mountedTouchedAction  `yaml:"actions"`
	Chosen    []string                `yaml:"chosen"`
	Expected  mountedSelectionFixture `yaml:"expected"`
}

func loadSelectionParityDocument(t *testing.T) selectionParityDocument {
	t.Helper()
	var document selectionParityDocument
	if err := testutil.DecodeNamedFixtureYAML(selectionParityData, &document); err != nil {
		t.Fatalf("decode kickstart testdata/selection_parity.yaml: %v", err)
	}
	inventories := map[string]bool{}
	for _, inventory := range document.Inventories {
		if inventories[inventory.Name] {
			t.Fatalf("duplicate selection parity inventory %q", inventory.Name)
		}
		inventories[inventory.Name] = true
	}
	for _, testCase := range document.Cases {
		if !inventories[testCase.Inventory] {
			t.Fatalf("selection parity case %q names inventory %q, which the fixture does not define", testCase.Name, testCase.Inventory)
		}
		if len(testCase.Actions) == 0 || len(testCase.Chosen) == 0 {
			t.Fatalf("selection parity case %q requires actions and the chosen sessions", testCase.Name)
		}
		for _, action := range testCase.Actions {
			if !validMountedTouchedAction(action) {
				t.Fatalf("selection parity case %q has unknown action %q", testCase.Name, action)
			}
		}
	}
	return document
}

func (d selectionParityDocument) inventory(name string) selectionParityInventory {
	for _, inventory := range d.Inventories {
		if inventory.Name == name {
			return inventory
		}
	}
	return selectionParityInventory{}
}

// TestTerminalAndWebSaveTheSameSelectionForTheSameChosenSessions proves one
// derivation serves both surfaces. The terminal ticks rows in the mounted
// program over a real scan. The web path is given only the ids of the sessions
// the terminal ended up choosing, as the setup page reports them, and derives
// through the shared onboarding model over the discovery forest. The two saved
// selections must marshal to the same YAML bytes and equal the fixture's rule.
func TestTerminalAndWebSaveTheSameSelectionForTheSameChosenSessions(t *testing.T) {
	t.Parallel()
	document := loadSelectionParityDocument(t)
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			inventory := document.inventory(testCase.Inventory)
			paths := materializeMountedPaths(t, inventory.Paths)
			listings := mountedListings(inventory.Listings, paths)

			terminal, chosen := terminalSelection(t, listings, testCase.Actions)
			want := append([]string(nil), testCase.Chosen...)
			sort.Strings(want)
			if !reflect.DeepEqual(chosen, want) {
				t.Fatalf("the terminal chose %v after %v, want %v", chosen, testCase.Actions, want)
			}

			web := webSelection(t, listings, testCase.Chosen)

			terminalYAML, webYAML := marshalSelection(t, terminal), marshalSelection(t, web)
			if terminalYAML != webYAML {
				t.Fatalf("the terminal and the web save different selections for the same chosen sessions\nterminal:\n%s\nweb:\n%s", terminalYAML, webYAML)
			}
			wantYAML := marshalSelection(t, mountedSelection(testCase.Expected, paths))
			if terminalYAML != wantYAML {
				t.Fatalf("both surfaces save a selection other than the fixture's\ngot:\n%s\nwant:\n%s", terminalYAML, wantYAML)
			}
		})
	}
}

// terminalSelection ticks rows in the mounted program and returns the selection
// the draft holds, with the ids of the sessions the tree shows as chosen once it
// is rescanned and restored from that selection.
func terminalSelection(t *testing.T, listings []ftue.SessionListing, actions []mountedTouchedAction) (config.SelectionConfig, []string) {
	t.Helper()
	configured := config.BaseConfig()
	configured.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.SaveAtomic(configPath, configured); err != nil {
		t.Fatalf("save the terminal's starting config: %v", err)
	}
	draft, err := settings.NewDraft(configPath, configured)
	if err != nil {
		t.Fatalf("open the terminal's draft: %v", err)
	}
	source := &mountedRecordingTreeSource{inner: kickstart.NewScannerTreeSource(
		listings,
		kickstart.WithPathIdentityResolver(ingest.NewPhysicalPathResolver()),
	)}
	program := kickstart.NewProgram(kickstart.ProgramDeps{Theme: theme.New(theme.ModeDark), Draft: draft, Source: source})
	program.SetSize(120, 28)
	program = declineOAuth(t, program)
	for _, action := range actions {
		program = pressAndDrain(program, mountedTouchedRune(action))
	}
	selection := draft.Working().Selection
	program = drainProgram(program, program.Init())
	return selection, checkedSessionIDs(source.latest())
}

// webSelection derives the selection the setup page's answer saves: the session
// ids alone, read against the forest discovery builds exactly as the server
// reads them (ApplyTreeAnswer), then derived through the shared onboarding
// model.
func webSelection(t *testing.T, listings []ftue.SessionListing, chosen []string) config.SelectionConfig {
	t.Helper()
	prepared := discovery.PrepareSessionListings(listings, ingest.NewPhysicalPathResolver())
	forest := discovery.BuildForest(prepared, nil, nil)
	// The server reads the ids a tab reports against its forest before it keeps
	// them: a row that names no session, or two, selects nothing, and a group id
	// selects the sessions beneath it. What it keeps is the answer the save
	// derives from.
	answer := onboarding.ApplyTreeAnswer(forest, chosen)
	if want := sortedCopy(chosen); !reflect.DeepEqual(sortedCopy(answer.SessionIDs), want) {
		t.Fatalf("the server keeps %v of the answered sessions %v; the answer names a session the forest does not hold", sortedCopy(answer.SessionIDs), want)
	}
	value, err := onboarding.SelectionIDsValue(answer.SessionIDs)
	if err != nil {
		t.Fatalf("encode the web answer: %v", err)
	}
	answers := onboarding.Answers{onboarding.FieldSelection: value}
	cfg := config.BaseConfig()
	cfg.Selection = config.SelectionConfig{Mode: config.SelectionModeSelected}
	model := onboarding.BuildModel(onboarding.Options{Scope: onboarding.ScopeSelection})
	if err := model.ApplyAnswers(cfg, answers, onboarding.WriteContext{Config: cfg, Forest: forest, Answers: answers}); err != nil {
		t.Fatalf("derive the selection from the web answer: %v", err)
	}
	return cfg.Selection
}

func marshalSelection(t *testing.T, selection config.SelectionConfig) string {
	t.Helper()
	data, err := yaml.Marshal(selection)
	if err != nil {
		t.Fatalf("marshal the selection: %v", err)
	}
	return string(data)
}
