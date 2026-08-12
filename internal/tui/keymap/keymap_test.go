package keymap_test

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/tui/keymap"
)

//go:embed testdata/actions.yaml
var actionsFixtureData []byte

type actionsDocument struct {
	ExpectedActionCount int            `yaml:"expectedActionCount"`
	Actions             []actionsEntry `yaml:"actions"`
}

type actionsEntry struct {
	Name string `yaml:"name"`
}

func loadActionsFixture(data []byte) (actionsDocument, error) {
	var doc actionsDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/actions.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/actions.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedActionCount != len(doc.Actions) || len(doc.Actions) == 0 {
		return doc, fmt.Errorf(
			"testdata/actions.yaml: expectedActionCount=%d but found %d actions (and must be non-zero)",
			doc.ExpectedActionCount, len(doc.Actions))
	}
	return doc, nil
}

// TestActionID_ClosedSet is the closed-set guard tying testdata/actions.yaml
// to the full keymap.ActionID enum: it asserts the fixture's row count
// equals len(keymap.AllActions()), then walks AllActions() in declaration
// order asserting each action's String() matches the fixture row at the
// same index and that IsValid() is true. Adding a new ActionID to the enum
// without adding its row here changes AllActions()'s length, which fails
// the count check below rather than silently shipping unverified.
func TestActionID_ClosedSet(t *testing.T) {
	t.Parallel()
	doc, err := loadActionsFixture(actionsFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	all := keymap.AllActions()
	if doc.ExpectedActionCount != len(all) {
		t.Fatalf("testdata/actions.yaml expectedActionCount=%d but keymap.AllActions() has %d entries - the fixture "+
			"is out of sync with the ActionID enum", doc.ExpectedActionCount, len(all))
	}
	for i, action := range all {
		t.Run(action.String(), func(t *testing.T) {
			t.Parallel()
			want := doc.Actions[i].Name
			if got := action.String(); got != want {
				t.Errorf("AllActions()[%d].String() = %q, want %q (fixture row %d)", i, got, want, i)
			}
			if !action.IsValid() {
				t.Errorf("AllActions()[%d] (%s) reports IsValid() = false, want true", i, action)
			}
		})
	}
}

// TestActionID_UnknownIsInvalid proves the zero-value ActionUnknown sentinel
// never passes IsValid, and stringifies distinctly from every real action -
// the mutation proof that a forgotten map entry (which reads back as the
// zero value) cannot silently alias a real, valid ActionID.
func TestActionID_UnknownIsInvalid(t *testing.T) {
	t.Parallel()
	if keymap.ActionUnknown.IsValid() {
		t.Fatal("ActionUnknown.IsValid() = true, want false - the zero value must never be a valid dispatch target")
	}
	if got := keymap.ActionUnknown.String(); got != "unknown" {
		t.Fatalf(`ActionUnknown.String() = %q, want "unknown"`, got)
	}
	for _, a := range keymap.AllActions() {
		if a == keymap.ActionUnknown {
			t.Fatalf("keymap.AllActions() contains ActionUnknown; the closed set must only hold real actions")
		}
	}
}

//go:embed testdata/default_bindings.yaml
var defaultBindingsFixtureData []byte

type defaultBindingsDocument struct {
	ExpectedBindingCount int                  `yaml:"expectedBindingCount"`
	Bindings             []defaultBindingCase `yaml:"bindings"`
}

type defaultBindingCase struct {
	Action   string   `yaml:"action"`
	Keys     []string `yaml:"keys"`
	HelpKey  string   `yaml:"helpKey"`
	HelpDesc string   `yaml:"helpDesc"`
}

func loadDefaultBindingsFixture(data []byte) (defaultBindingsDocument, error) {
	var doc defaultBindingsDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/default_bindings.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/default_bindings.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedBindingCount != len(doc.Bindings) || len(doc.Bindings) == 0 {
		return doc, fmt.Errorf(
			"testdata/default_bindings.yaml: expectedBindingCount=%d but found %d bindings (and must be non-zero)",
			doc.ExpectedBindingCount, len(doc.Bindings))
	}
	seen := map[string]bool{}
	for _, b := range doc.Bindings {
		if b.Action == "" || seen[b.Action] {
			return doc, fmt.Errorf("testdata/default_bindings.yaml: action %q is missing or duplicated", b.Action)
		}
		seen[b.Action] = true
	}
	return doc, nil
}

// TestDefault_Bindings is the fixture-driven proof that keymap.Default()
// defines exactly the keys and help text testdata/default_bindings.yaml
// pins, for every ActionID in keymap.AllActions() - both directions: the
// fixture's row count is pinned to len(AllActions()), AND every action in
// AllActions() must resolve a fixture row by name (a missing row fails the
// lookup directly, independent of the count check).
func TestDefault_Bindings(t *testing.T) {
	t.Parallel()
	doc, err := loadDefaultBindingsFixture(defaultBindingsFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	all := keymap.AllActions()
	if doc.ExpectedBindingCount != len(all) {
		t.Fatalf("testdata/default_bindings.yaml expectedBindingCount=%d but keymap.AllActions() has %d entries",
			doc.ExpectedBindingCount, len(all))
	}
	byAction := make(map[string]defaultBindingCase, len(doc.Bindings))
	for _, b := range doc.Bindings {
		byAction[b.Action] = b
	}

	km := keymap.Default()
	for _, action := range all {
		t.Run(action.String(), func(t *testing.T) {
			t.Parallel()
			row, ok := byAction[action.String()]
			if !ok {
				t.Fatalf("testdata/default_bindings.yaml has no row for action %q", action.String())
			}
			binding, ok := km[action]
			if !ok {
				t.Fatalf("keymap.Default() has no entry for %s - every ActionID must have a binding", action)
			}
			if !reflect.DeepEqual(binding.Keys(), row.Keys) {
				t.Errorf("Default()[%s].Keys() = %v, want %v", action, binding.Keys(), row.Keys)
			}
			help := binding.Help()
			if help.Key != row.HelpKey || help.Desc != row.HelpDesc {
				t.Errorf("Default()[%s].Help() = %+v, want {Key:%q Desc:%q}", action, help, row.HelpKey, row.HelpDesc)
			}
			if !binding.Enabled() {
				t.Errorf("Default()[%s] is not Enabled() - every default binding must start enabled", action)
			}
		})
	}
}

//go:embed testdata/match_dispatch.yaml
var matchDispatchFixtureData []byte

const (
	requiredMatchDispatchCaseCount       = 42
	requiredRemovedPrevScopeCaseName     = "removed-prev-scope-key-does-not-dispatch"
	requiredRemovedNextScopeCaseName     = "removed-next-scope-key-does-not-dispatch"
	requiredGlobalSearchDispatchCaseName = "dispatches-search"
)

type matchDispatchDocument struct {
	ExpectedCaseCount int                 `yaml:"expectedCaseCount"`
	Cases             []matchDispatchCase `yaml:"cases"`
}

type matchDispatchCase struct {
	Name       string   `yaml:"name"`
	Available  []string `yaml:"available"`
	PressedKey string   `yaml:"pressedKey"`
	WantOK     bool     `yaml:"wantOK"`
	WantAction string   `yaml:"wantAction"`
}

func loadMatchDispatchFixture(data []byte) (matchDispatchDocument, error) {
	var doc matchDispatchDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/match_dispatch.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("testdata/match_dispatch.yaml must hold exactly one YAML document: %w", err)
	}
	if doc.ExpectedCaseCount != requiredMatchDispatchCaseCount || len(doc.Cases) != requiredMatchDispatchCaseCount {
		return doc, fmt.Errorf(
			"testdata/match_dispatch.yaml: expectedCaseCount=%d, found=%d, required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), requiredMatchDispatchCaseCount)
	}
	seen := map[string]bool{}
	for _, c := range doc.Cases {
		if c.Name == "" || seen[c.Name] {
			return doc, fmt.Errorf("testdata/match_dispatch.yaml: case name %q is missing or duplicated", c.Name)
		}
		seen[c.Name] = true
	}
	if !seen[requiredRemovedPrevScopeCaseName] {
		return doc, fmt.Errorf("testdata/match_dispatch.yaml: required case %q is missing", requiredRemovedPrevScopeCaseName)
	}
	if !seen[requiredRemovedNextScopeCaseName] {
		return doc, fmt.Errorf("testdata/match_dispatch.yaml: required case %q is missing", requiredRemovedNextScopeCaseName)
	}
	if !seen[requiredGlobalSearchDispatchCaseName] {
		return doc, fmt.Errorf("testdata/match_dispatch.yaml: required case %q is missing", requiredGlobalSearchDispatchCaseName)
	}
	return doc, nil
}

func TestMatchDispatchFixtureRejectsCoordinatedRemovedScopeRowRemoval(t *testing.T) {
	mutated := mutateFixtureFragment(t, "testdata/match_dispatch.yaml", matchDispatchFixtureData,
		[]byte("expectedCaseCount: 42"), []byte("expectedCaseCount: 41"))
	mutated = mutateFixtureFragment(t, "testdata/match_dispatch.yaml", mutated,
		[]byte("  - name: removed-prev-scope-key-does-not-dispatch\n    available: [search]\n    pressedKey: \"[\"\n    wantOK: false\n    wantAction: \"\"\n"), nil)
	if _, err := loadMatchDispatchFixture(mutated); err == nil {
		t.Fatal("match-dispatch fixture accepted removal of a retired-scope regression row coordinated with its declared count")
	}
}

// TestMatch_DispatchTable drives keymap.Match against every fixture case,
// then separately asserts the full-enum-coverage guard: the union of
// wantAction across every wantOK=true case must equal keymap.AllActions()
// exactly, so an ActionID added to the enum without a passing dispatch row
// fails here even if every existing row still passes.
func TestMatch_DispatchTable(t *testing.T) {
	t.Parallel()
	doc, err := loadMatchDispatchFixture(matchDispatchFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	km := keymap.Default()
	covered := map[keymap.ActionID]bool{}

	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			avail := availabilityFromNames(t, c.Available)
			msg := msgForKeyString(t, c.PressedKey)
			gotAction, gotOK := keymap.Match(km, msg, avail)
			if gotOK != c.WantOK {
				t.Fatalf("Match(...) ok = %v, want %v (case %q)", gotOK, c.WantOK, c.Name)
			}
			if !c.WantOK {
				return
			}
			want := actionByName(t, c.WantAction)
			if gotAction != want {
				t.Errorf("Match(...) action = %s, want %s (case %q)", gotAction, want, c.Name)
			}
		})
	}

	for _, c := range doc.Cases {
		if c.WantOK {
			covered[actionByName(t, c.WantAction)] = true
		}
	}
	for _, action := range keymap.AllActions() {
		if !covered[action] {
			t.Errorf("testdata/match_dispatch.yaml has no passing (wantOK: true) case dispatching %s - every "+
				"ActionID must have at least one row proving Match can actually resolve it", action)
		}
	}
}
