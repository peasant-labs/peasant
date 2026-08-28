package settings

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedScreenSeedRows       = 4
	expectedScreenBehaviorRows   = 9
	expectedScreenPopulateRows   = 2
	expectedFlowScreenParityRows = 2
)

//go:embed testdata/screen/seed.yaml
var screenSeedFixtureYAML []byte

//go:embed testdata/screen/behavior.yaml
var screenBehaviorFixtureYAML []byte

//go:embed testdata/screen/prepopulate.yaml
var screenPopulateFixtureYAML []byte

type screenSeedOperation string

const (
	screenSeedValid    screenSeedOperation = "valid"
	screenSeedNilDraft screenSeedOperation = "nil-draft"
	screenSeedNilGet   screenSeedOperation = "nil-get"
	screenSeedNilSet   screenSeedOperation = "nil-set"
)

func (o screenSeedOperation) valid() bool {
	switch o {
	case screenSeedValid, screenSeedNilDraft, screenSeedNilGet, screenSeedNilSet:
		return true
	}
	return false
}

type screenSeedFixture struct {
	Name      string              `yaml:"name"`
	Operation screenSeedOperation `yaml:"operation"`
	Initial   int                 `yaml:"initial"`
}

type screenSeedDocument struct {
	ExpectedRows int                 `yaml:"expectedRows"`
	Cases        []screenSeedFixture `yaml:"cases"`
}

type screenBehaviorOperation string

const (
	screenBehaviorVisibleTransient screenBehaviorOperation = "visible-transient-dirty"
	screenBehaviorHiddenSave       screenBehaviorOperation = "hidden-transient-save"
	screenBehaviorDiscard          screenBehaviorOperation = "discard"
	screenBehaviorSave             screenBehaviorOperation = "save"
	screenBehaviorJump             screenBehaviorOperation = "jump-navigation"
	screenBehaviorHiddenField      screenBehaviorOperation = "hidden-field-save"
	screenBehaviorHiddenCascade    screenBehaviorOperation = "hidden-field-cascade"
	screenBehaviorSavePending      screenBehaviorOperation = "save-pending-freeze"
	screenBehaviorHiddenValidation screenBehaviorOperation = "hidden-field-validation"
)

func (o screenBehaviorOperation) valid() bool {
	switch o {
	case screenBehaviorVisibleTransient, screenBehaviorHiddenSave, screenBehaviorDiscard,
		screenBehaviorSave, screenBehaviorJump, screenBehaviorHiddenField,
		screenBehaviorHiddenCascade, screenBehaviorSavePending, screenBehaviorHiddenValidation:
		return true
	}
	return false
}

type screenBehaviorFixture struct {
	Name              string                  `yaml:"name"`
	Operation         screenBehaviorOperation `yaml:"operation"`
	Connected         bool                    `yaml:"connected"`
	InitialRetention  int                     `yaml:"initialRetention"`
	SelectedRetention int                     `yaml:"selectedRetention"`
	SaveInstruction   string                  `yaml:"saveInstruction"`
	FlowScreenParity  bool                    `yaml:"flowScreenParity"`
}

type screenBehaviorDocument struct {
	ExpectedRows int                     `yaml:"expectedRows"`
	Cases        []screenBehaviorFixture `yaml:"cases"`
}

type screenPopulateFixture struct {
	Name             string               `yaml:"name"`
	Mode             config.SelectionMode `yaml:"mode"`
	SelectedSessions []string             `yaml:"selectedSessions"`
	ExpectMode       config.SelectionMode `yaml:"expectMode"`
	ExpectChecked    []string             `yaml:"expectChecked"`
}

type screenPopulateDocument struct {
	ExpectedRows int                     `yaml:"expectedRows"`
	Cases        []screenPopulateFixture `yaml:"cases"`
}

func loadScreenSeedFixtures(t *testing.T) []screenSeedFixture {
	t.Helper()
	var document screenSeedDocument
	if err := decodeScreenFixture("testdata/screen/seed.yaml", screenSeedFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedScreenSeedRows || len(document.Cases) != expectedScreenSeedRows {
		t.Fatalf("seed fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Cases), expectedScreenSeedRows)
	}
	for _, fixture := range document.Cases {
		if fixture.Name == "" || !fixture.Operation.valid() {
			t.Fatalf("invalid seed fixture: name=%q operation=%q", fixture.Name, fixture.Operation)
		}
	}
	return document.Cases
}

func loadScreenBehaviorFixtures(t *testing.T) []screenBehaviorFixture {
	t.Helper()
	var document screenBehaviorDocument
	if err := decodeScreenFixture("testdata/screen/behavior.yaml", screenBehaviorFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedScreenBehaviorRows || len(document.Cases) != expectedScreenBehaviorRows {
		t.Fatalf("behavior fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Cases), expectedScreenBehaviorRows)
	}
	parityRows := 0
	for _, fixture := range document.Cases {
		if fixture.Name == "" || !fixture.Operation.valid() {
			t.Fatalf("invalid behavior fixture: name=%q operation=%q", fixture.Name, fixture.Operation)
		}
		if fixture.FlowScreenParity {
			parityRows++
		}
	}
	if parityRows != expectedFlowScreenParityRows {
		t.Fatalf("flow/screen parity rows=%d want=%d", parityRows, expectedFlowScreenParityRows)
	}
	return document.Cases
}

func loadScreenPopulateFixtures(t *testing.T) []screenPopulateFixture {
	t.Helper()
	var document screenPopulateDocument
	if err := decodeScreenFixture("testdata/screen/prepopulate.yaml", screenPopulateFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedScreenPopulateRows || len(document.Cases) != expectedScreenPopulateRows {
		t.Fatalf("prepopulate fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Cases), expectedScreenPopulateRows)
	}
	for _, fixture := range document.Cases {
		if fixture.Name == "" || !fixture.Mode.IsValid() || !fixture.ExpectMode.IsValid() {
			t.Fatalf("invalid prepopulate fixture: name=%q mode=%q expectMode=%q", fixture.Name, fixture.Mode, fixture.ExpectMode)
		}
	}
	return document.Cases
}

func decodeScreenFixture(path string, data []byte, destination any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s with known fields: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s must contain exactly one YAML document", path)
		}
		return fmt.Errorf("decode trailing document in %s: %w", path, err)
	}
	return nil
}

func TestScreenFixtureDecoder_StrictAndSingleDocument(t *testing.T) {
	var unknown screenBehaviorDocument
	withUnknown := append([]byte("unexpectedField: true\n"), screenBehaviorFixtureYAML...)
	if err := decodeScreenFixture("behavior.yaml", withUnknown, &unknown); err == nil || !strings.Contains(err.Error(), "field unexpectedField not found") {
		t.Fatalf("unknown field error=%v want strict rejection", err)
	}

	var trailing screenBehaviorDocument
	withTrailing := append(append([]byte(nil), screenBehaviorFixtureYAML...), []byte("\n---\nexpectedRows: 0\n")...)
	if err := decodeScreenFixture("behavior.yaml", withTrailing, &trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error=%v want single-document rejection", err)
	}
}

func TestSeedInitial_Fixtures(t *testing.T) {
	for _, fixture := range loadScreenSeedFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			path, loaded, before := screenFixtureConfig(t, true)
			draft, err := NewDraft(path, loaded)
			if err != nil {
				t.Fatalf("NewDraft: %v", err)
			}
			accessor := screenRetentionAccessor()
			var target *Draft = draft
			switch fixture.Operation {
			case screenSeedNilDraft:
				target = nil
			case screenSeedNilGet:
				accessor.Get = nil
			case screenSeedNilSet:
				accessor.Set = nil
			}

			err = SeedInitial(target, accessor, fixture.Initial)
			if fixture.Operation != screenSeedValid {
				if err == nil {
					t.Fatal("SeedInitial accepted an invalid boundary")
				}
				assertActionableScreenError(t, err)
				if draft.Baseline().ClaudeRetentionDays != 0 || draft.Working().ClaudeRetentionDays != 0 {
					t.Fatalf("rejected seed mutated draft: baseline=%d working=%d", draft.Baseline().ClaudeRetentionDays, draft.Working().ClaudeRetentionDays)
				}
				return
			}
			if err != nil {
				t.Fatalf("SeedInitial: %v", err)
			}
			if got := draft.Baseline().ClaudeRetentionDays; got != fixture.Initial {
				t.Fatalf("baseline retention=%d want=%d", got, fixture.Initial)
			}
			if got := draft.Working().ClaudeRetentionDays; got != fixture.Initial {
				t.Fatalf("working retention=%d want=%d", got, fixture.Initial)
			}
			draft.Working().ClaudeRetentionDays++
			if err := draft.Discard(); err != nil {
				t.Fatalf("Discard: %v", err)
			}
			if got := draft.Working().ClaudeRetentionDays; got != fixture.Initial {
				t.Fatalf("discarded retention=%d want seeded=%d", got, fixture.Initial)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("read config after seed: %v", readErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("SeedInitial or Discard changed the expected on-disk snapshot")
			}
		})
	}
}

func TestScreen_BehaviorFixtures(t *testing.T) {
	for _, fixture := range loadScreenBehaviorFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			path, loaded, before := screenFixtureConfig(t, fixture.Connected)
			if fixture.Operation == screenBehaviorHiddenCascade {
				screenSetCascadeValues(loaded, false)
				if err := config.SaveAtomic(path, loaded); err != nil {
					t.Fatalf("seed cascade config: %v", err)
				}
				var readErr error
				before, readErr = os.ReadFile(path)
				if readErr != nil {
					t.Fatalf("read cascade config: %v", readErr)
				}
			}
			draft, err := NewDraft(path, loaded)
			if err != nil {
				t.Fatalf("NewDraft: %v", err)
			}
			if err := SeedInitial(draft, screenRetentionAccessor(), fixture.InitialRetention); err != nil {
				t.Fatalf("SeedInitial: %v", err)
			}
			registry := screenFixtureRegistry()
			if fixture.Operation == screenBehaviorHiddenField {
				registry = screenFieldVisibilityRegistry()
			}
			if fixture.Operation == screenBehaviorHiddenCascade {
				registry = screenCascadeRegistry()
				screenSetCascadeValues(draft.Working(), true)
				draft.Working().ClaudeRetentionDays = fixture.SelectedRetention
			}
			if fixture.Operation == screenBehaviorHiddenValidation {
				registry = screenHiddenValidationRegistry(t, draft)
			}
			screen := NewScreen(theme.New(theme.ModeDark), registry, draft)
			screen.SetSize(80, 20)
			if screen.Err() != nil {
				t.Fatalf("NewScreen: %v", screen.Err())
			}
			if strings.Contains(screen.View(), "guided-only framing") {
				t.Fatal("dense Screen rendered Section.Guide metadata")
			}
			if fixture.SaveInstruction != "" && !strings.Contains(screen.View(), fixture.SaveInstruction) {
				t.Fatalf("Screen.View missing save instruction %q", fixture.SaveInstruction)
			}

			switch fixture.Operation {
			case screenBehaviorVisibleTransient:
				screen = selectFixtureRetention(screen, fixture.InitialRetention, fixture.SelectedRetention)
				if !screen.Dirty() || !screen.sections[screen.section].dirty(draft) {
					t.Fatal("visible transient edit did not mark section and screen dirty")
				}
				if draft.Dirty() {
					t.Fatal("yaml-backed Draft.Dirty included transient retention")
				}
				if !strings.Contains(screen.View(), "[modified]") {
					t.Fatal("visible dirty indicator is absent from Screen.View")
				}
			case screenBehaviorHiddenSave:
				screen = selectFixtureRetention(screen, fixture.InitialRetention, fixture.SelectedRetention)
				screen = sendScreenKeys(screen, screenKey("tab"), screenKey("up"), screenKey("enter"), screenKey("space"))
				var cmd tea.Cmd
				screen, cmd = screen.Update(screenKey("ctrl+s"))
				message := runResult(cmd)
				if _, ok := message.(SavedMsg); !ok {
					t.Fatalf("save command message=%T want SavedMsg; err=%v", message, screen.Err())
				}
				if got := draft.Working().ClaudeRetentionDays; got != fixture.InitialRetention {
					t.Fatalf("hidden retention edit survived save: got=%d want=%d", got, fixture.InitialRetention)
				}
			case screenBehaviorDiscard:
				screen = selectFixtureRetention(screen, fixture.InitialRetention, fixture.SelectedRetention)
				screen = sendScreenKeys(screen, screenKey("esc"), screenKey("left"))
				var cmd tea.Cmd
				screen, cmd = screen.Update(screenKey("enter"))
				if cmd == nil {
					t.Fatal("confirmed discard did not issue quit")
				}
				if draft.Working().ClaudeRetentionDays != fixture.InitialRetention || screen.Dirty() {
					t.Fatalf("discard did not restore transient state: retention=%d dirty=%t", draft.Working().ClaudeRetentionDays, screen.Dirty())
				}
				after, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatalf("read after discard: %v", readErr)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("confirmed discard changed config bytes")
				}
			case screenBehaviorSave:
				screen = sendScreenKeys(screen, screenKey("enter"), screenKey("space"))
				if !strings.Contains(screen.View(), "[modified]") {
					t.Fatal("toggle edit has no field-level dirty indicator")
				}
				var cmd tea.Cmd
				screen, cmd = screen.Update(screenKey("ctrl+s"))
				message, ok := runResult(cmd).(SavedMsg)
				if !ok || message.Draft() != draft {
					t.Fatalf("save result=%T draft=%p want SavedMsg draft=%p; err=%v", message, message.Draft(), draft, screen.Err())
				}
				reloaded := screenParseConfig(t, path)
				if reloaded.Village.Connected == fixture.Connected {
					t.Fatalf("explicit save did not commit toggle: connected=%t", reloaded.Village.Connected)
				}
			case screenBehaviorJump:
				beforeView := screen.View()
				screen = sendScreenKeys(screen, screenKey("down"))
				if screen.sections[screen.section].Key != "retention" {
					t.Fatalf("jump navigation selected %q want retention", screen.sections[screen.section].Key)
				}
				if beforeView == screen.View() {
					t.Fatal("jump navigation did not change the mounted view")
				}
				if screen.Dirty() || draft.Dirty() {
					t.Fatal("jump navigation changed the draft")
				}
			case screenBehaviorHiddenField:
				screen = sendScreenKeys(screen, screenKey("enter"), screenKey("tab"), screenKey("down"), screenKey("space"), screenKey("shift+tab"), screenKey("space"))
				var cmd tea.Cmd
				screen, cmd = screen.Update(screenKey("ctrl+s"))
				if _, ok := runResult(cmd).(SavedMsg); !ok {
					t.Fatalf("hidden-field save did not emit SavedMsg: err=%v", screen.Err())
				}
				if got := draft.Working().ClaudeRetentionDays; got != fixture.InitialRetention {
					t.Fatalf("hidden field edit survived save: retention=%d want=%d", got, fixture.InitialRetention)
				}
			case screenBehaviorHiddenCascade:
				var cmd tea.Cmd
				screen, cmd = screen.Update(screenKey("ctrl+s"))
				if _, ok := runResult(cmd).(SavedMsg); !ok {
					t.Fatalf("cascade save did not emit SavedMsg: err=%v", screen.Err())
				}
				if screenCascadeValues(draft.Working()) != [4]bool{} {
					t.Fatalf("persisted cascade edits survived save: %v", screenCascadeValues(draft.Working()))
				}
				if got := draft.Working().ClaudeRetentionDays; got != fixture.InitialRetention {
					t.Fatalf("transient cascade edit survived save: retention=%d want=%d", got, fixture.InitialRetention)
				}
				reloaded := screenParseConfig(t, path)
				if screenCascadeValues(reloaded) != [4]bool{} {
					t.Fatalf("persisted cascade values reached disk: %v", screenCascadeValues(reloaded))
				}
			case screenBehaviorSavePending:
				screen = selectFixtureRetention(screen, fixture.InitialRetention, fixture.SelectedRetention)
				var savedCmd tea.Cmd
				screen, savedCmd = screen.Update(screenKey("ctrl+s"))
				if savedCmd == nil || !screen.savePending {
					t.Fatal("save did not enter pending state with a completion command")
				}
				var blockedCmd tea.Cmd
				screen, blockedCmd = screen.Update(screenKey("esc"))
				if blockedCmd != nil || screen.confirming {
					t.Fatal("pending save opened discard confirmation")
				}
				screen, _ = screen.Update(screenKey("up"))
				screen, _ = screen.Update(screenKey("space"))
				if got := draft.Working().ClaudeRetentionDays; got != fixture.SelectedRetention {
					t.Fatalf("pending save accepted retention mutation: got=%d want=%d", got, fixture.SelectedRetention)
				}
				message, ok := runResult(savedCmd).(SavedMsg)
				if !ok {
					t.Fatalf("save completion=%T want SavedMsg", message)
				}
				screen, blockedCmd = screen.Update(message)
				if blockedCmd == nil {
					t.Fatal("matching save completion did not quit")
				}
			case screenBehaviorHiddenValidation:
				var cmd tea.Cmd
				screen, cmd = screen.Update(screenKey("ctrl+s"))
				if _, ok := runResult(cmd).(SavedMsg); !ok {
					t.Fatalf("hidden-validation save did not emit SavedMsg: err=%v", screen.Err())
				}
			}
		})
	}
}

func TestFlowScreen_FieldVisibilityParityFixtures(t *testing.T) {
	for _, fixture := range loadScreenBehaviorFixtures(t) {
		fixture := fixture
		if !fixture.FlowScreenParity {
			continue
		}
		t.Run(fixture.Name, func(t *testing.T) {
			flowDraft, flowPath := screenParityDraft(t, fixture, "flow")
			screenDraft, screenPath := screenParityDraft(t, fixture, "screen")
			flowRegistry := screenParityRegistry(t, fixture, flowDraft)
			screenRegistry := screenParityRegistry(t, fixture, screenDraft)
			if fixture.Operation == screenBehaviorHiddenCascade &&
				(!flowRegistry.dirty(flowDraft) || !screenRegistry.dirty(screenDraft)) {
				t.Fatal("cascade precondition is not visibly dirty in both presentations")
			}

			flow := NewFlow(theme.New(theme.ModeDark), flowRegistry, flowDraft)
			flow.SetSize(80, 20)
			for step := 0; step <= len(flowRegistry.Sections) && !flow.OnReceipt(); step++ {
				flow, _ = flow.Update(screenKey("tab"))
			}
			flow, _ = flow.Update(screenKey("enter"))

			screen := NewScreen(theme.New(theme.ModeDark), screenRegistry, screenDraft)
			screen.SetSize(80, 20)
			screen, cmd := screen.Update(screenKey("ctrl+s"))
			message, saved := runResult(cmd).(SavedMsg)
			if !flow.Committed() || flow.Err() != nil || !saved || message.Draft() != screenDraft || screen.Err() != nil {
				t.Fatalf("Flow/Screen save results committed/error/message = %t/%v/%T/%v",
					flow.Committed(), flow.Err(), message, screen.Err())
			}
			flowConfig := screenParseConfig(t, flowPath)
			screenConfig := screenParseConfig(t, screenPath)
			if !reflect.DeepEqual(flowConfig, screenConfig) {
				t.Fatalf("Flow and Screen persisted different config:\nFlow=%#v\nScreen=%#v", flowConfig, screenConfig)
			}

			switch fixture.Operation {
			case screenBehaviorHiddenCascade:
				if screenCascadeValues(flowDraft.Working()) != [4]bool{} ||
					screenCascadeValues(screenDraft.Working()) != [4]bool{} {
					t.Fatalf("field-level persisted edits survived: Flow=%v Screen=%v",
						screenCascadeValues(flowDraft.Working()), screenCascadeValues(screenDraft.Working()))
				}
				if flowDraft.Working().ClaudeRetentionDays != fixture.InitialRetention ||
					screenDraft.Working().ClaudeRetentionDays != fixture.InitialRetention {
					t.Fatalf("field-level transient edit survived: Flow=%d Screen=%d want=%d",
						flowDraft.Working().ClaudeRetentionDays, screenDraft.Working().ClaudeRetentionDays,
						fixture.InitialRetention)
				}
				if flowRegistry.dirty(flowDraft) || screenRegistry.dirty(screenDraft) {
					t.Fatal("a presentation remained dirty after converging hidden field edits")
				}
			case screenBehaviorHiddenValidation:
				if !reflect.DeepEqual(flowDraft.Working().Selection, flowDraft.Baseline().Selection) ||
					!reflect.DeepEqual(screenDraft.Working().Selection, screenDraft.Baseline().Selection) {
					t.Fatal("hidden invalid selection was not reset to baseline before save")
				}
			}
		})
	}
}

func screenParityDraft(t *testing.T, fixture screenBehaviorFixture, suffix string) (*Draft, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), suffix+".yaml")
	cfg := config.BaseConfig()
	cfg.Village.Connected = fixture.Connected
	if fixture.Operation == screenBehaviorHiddenCascade {
		screenSetCascadeValues(cfg, false)
	}
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed parity config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read parity config: %v", err)
	}
	loaded, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse parity config: %v", err)
	}
	draft, err := NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open parity Draft: %v", err)
	}
	if err := SeedInitial(draft, screenRetentionAccessor(), fixture.InitialRetention); err != nil {
		t.Fatalf("seed parity retention: %v", err)
	}
	if fixture.Operation == screenBehaviorHiddenCascade {
		screenSetCascadeValues(draft.Working(), true)
		draft.Working().ClaudeRetentionDays = fixture.SelectedRetention
	}
	return draft, path
}

func screenParityRegistry(t *testing.T, fixture screenBehaviorFixture, draft *Draft) Registry {
	t.Helper()
	switch fixture.Operation {
	case screenBehaviorHiddenCascade:
		return screenCascadeRegistry()
	case screenBehaviorHiddenValidation:
		return screenHiddenValidationRegistry(t, draft)
	default:
		t.Fatalf("behavior %q is not a Flow/Screen parity operation", fixture.Operation)
		return Registry{}
	}
}

func TestApplyExistingSelection_ProjectFirstFixtures(t *testing.T) {
	for _, fixture := range loadScreenPopulateFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			roots := screenProjectFirstForest()
			selection := config.SelectionConfig{Mode: fixture.Mode}
			if fixture.Mode == config.SelectionModeSelected {
				selection.Harnesses = map[string]config.SelectionHarnessConfig{
					"claude-code": {Sessions: fixture.SelectedSessions},
				}
			}
			ApplyExistingSelection(roots, selection)

			checked := screenCheckedSessionIDs(roots)
			if !sameSet(checked, fixture.ExpectChecked) {
				t.Fatalf("checked sessions=%v want=%v", checked, fixture.ExpectChecked)
			}
			derived := FromTreeNodes(roots)
			if derived.Mode != fixture.ExpectMode {
				t.Fatalf("derived mode=%q want=%q", derived.Mode, fixture.ExpectMode)
			}
			if fixture.ExpectMode == config.SelectionModeSelected && !sameSet(derived.Harnesses["claude-code"].Sessions, fixture.SelectedSessions) {
				t.Fatalf("derived sessions=%v want=%v", derived.Harnesses["claude-code"].Sessions, fixture.SelectedSessions)
			}
		})
	}
}

func screenFixtureConfig(t *testing.T, connected bool) (string, *config.Config, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := config.BaseConfig()
	cfg.Village.Connected = connected
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	loaded, err := config.Parse(before)
	if err != nil {
		t.Fatalf("parse seeded config: %v", err)
	}
	return path, loaded, before
}

func screenFixtureRegistry() Registry {
	return Registry{Sections: []Section{
		{
			Key:   "general",
			Title: "general",
			Guide: &Guide{Intro: "guided-only framing"},
			Fields: []Field{
				Toggle("connected", "connected", connectedAccessor()),
			},
		},
		{
			Key:   "retention",
			Title: "retention",
			When:  func(d *Draft) bool { return d.Working().Village.Connected },
			Fields: []Field{
				Radio("retention-days", "retention days", screenRetentionAccessor(),
					Option[int]{Label: "30 days", Value: 30},
					Option[int]{Label: "90 days", Value: 90},
					Option[int]{Label: "365 days", Value: 365},
					Option[int]{Label: "never", Value: 99999}),
			},
		},
	}}
}

func screenFieldVisibilityRegistry() Registry {
	retention := Radio("retention-days", "retention days", screenRetentionAccessor(),
		Option[int]{Label: "30 days", Value: 30},
		Option[int]{Label: "90 days", Value: 90},
	).(*radioField[int])
	retention.when = func(d *Draft) bool { return d.Working().Village.Connected }
	return Registry{Sections: []Section{
		{
			Key:   "general",
			Title: "general",
			Fields: []Field{
				Toggle("connected", "connected", connectedAccessor()),
				retention,
			},
		},
	}}
}

func screenHiddenValidationRegistry(t *testing.T, draft *Draft) Registry {
	t.Helper()
	selection := Tree("hidden-selection", "hidden selection", selectionAccessor(), scannerfix.NewFixtureTreeSource("conflict")).(*treeField)
	selection.when = func(*Draft) bool { return false }
	selection.mount(theme.New(theme.ModeDark))
	for _, message := range runAll(selection.initCmd()) {
		selection.handleAsync(draft, message)
	}
	if err := selection.Validate(draft); err == nil {
		t.Fatal("hidden validation fixture did not load a real conflicting tree")
	}
	return Registry{Sections: []Section{{
		Key:    "hidden-validation",
		Title:  "hidden validation",
		Fields: []Field{selection},
	}}}
}

func screenCascadeRegistry() Registry {
	first := Toggle("cascade-first", "cascade first", screenCascadeAccessor(0)).(*toggleField)
	first.when = func(*Draft) bool { return false }
	second := Toggle("cascade-second", "cascade second", screenCascadeAccessor(1)).(*toggleField)
	second.when = func(d *Draft) bool { return screenCascadeAccessor(0).Get(d.Working()) }
	third := Toggle("cascade-third", "cascade third", screenCascadeAccessor(2)).(*toggleField)
	third.when = func(d *Draft) bool { return screenCascadeAccessor(1).Get(d.Working()) }
	fourth := Toggle("cascade-fourth", "cascade fourth", screenCascadeAccessor(3)).(*toggleField)
	fourth.when = func(d *Draft) bool { return screenCascadeAccessor(2).Get(d.Working()) }
	retention := Radio("cascade-retention", "cascade retention", screenRetentionAccessor(),
		Option[int]{Label: "30 days", Value: 30},
		Option[int]{Label: "90 days", Value: 90},
	).(*radioField[int])
	retention.when = func(d *Draft) bool { return screenCascadeAccessor(3).Get(d.Working()) }
	return Registry{Sections: []Section{{
		Key:    "cascade",
		Title:  "cascade",
		Fields: []Field{first, second, third, fourth, retention},
	}}}
}

func screenCascadeAccessor(index int) Accessor[bool] {
	return Accessor[bool]{
		Get: func(cfg *config.Config) bool { return screenCascadeValues(cfg)[index] },
		Set: func(cfg *config.Config, value bool) {
			switch index {
			case 0:
				cfg.Village.Connected = value
			case 1:
				cfg.Sources.ClaudeCode.Enabled = value
			case 2:
				cfg.Sources.Codex.Enabled = value
			case 3:
				cfg.Push.Fields.GitBranch = value
			}
		},
	}
}

func screenCascadeValues(cfg *config.Config) [4]bool {
	return [4]bool{
		cfg.Village.Connected,
		cfg.Sources.ClaudeCode.Enabled,
		cfg.Sources.Codex.Enabled,
		cfg.Push.Fields.GitBranch,
	}
}

func screenSetCascadeValues(cfg *config.Config, value bool) {
	for index := range screenCascadeValues(cfg) {
		screenCascadeAccessor(index).Set(cfg, value)
	}
}

func screenProjectFirstForest() []*kit.TreeNode {
	return []*kit.TreeNode{
		{
			ID:    "git@example.test:team/project.git",
			Label: "example:team/project",
			Meta:  map[string]string{MetaRemote: "git@example.test:team/project.git"},
			Children: []*kit.TreeNode{
				{
					ID:    "main",
					Label: "main",
					Meta:  map[string]string{MetaBranch: "main"},
					Children: []*kit.TreeNode{
						{ID: "session-one", Label: "session one", Meta: map[string]string{MetaHarness: "claude-code"}},
						{ID: "session-two", Label: "session two", Meta: map[string]string{MetaHarness: "claude-code"}},
					},
				},
			},
		},
	}
}

func screenCheckedSessionIDs(roots []*kit.TreeNode) []string {
	var checked []string
	var walk func(*kit.TreeNode)
	walk = func(node *kit.TreeNode) {
		if node.Meta[MetaHarness] != "" && node.State == kit.Checked {
			checked = append(checked, node.ID)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return checked
}

func screenRetentionAccessor() Accessor[int] {
	return Accessor[int]{
		Get: func(cfg *config.Config) int { return cfg.ClaudeRetentionDays },
		Set: func(cfg *config.Config, value int) { cfg.ClaudeRetentionDays = value },
	}
}

func selectFixtureRetention(screen Screen, initial, selected int) Screen {
	screen = sendScreenKeys(screen, screenKey("down"), screenKey("enter"))
	step := 1
	if selected < initial {
		step = -1
	}
	for current := initial; current != selected; {
		if step > 0 {
			screen = sendScreenKeys(screen, screenKey("down"))
		} else {
			screen = sendScreenKeys(screen, screenKey("up"))
		}
		current = nextRetentionFixtureValue(current, step)
	}
	return sendScreenKeys(screen, screenKey("space"))
}

func nextRetentionFixtureValue(current, direction int) int {
	switch {
	case direction > 0 && current == 30:
		return 90
	case direction > 0 && current == 90:
		return 365
	case direction > 0 && current == 365:
		return 99999
	case direction < 0 && current == 99999:
		return 365
	case direction < 0 && current == 365:
		return 90
	case direction < 0 && current == 90:
		return 30
	default:
		return current
	}
}

func sendScreenKeys(screen Screen, keys ...tea.KeyPressMsg) Screen {
	for _, message := range keys {
		screen, _ = screen.Update(message)
	}
	return screen
}

func screenKey(name string) tea.KeyPressMsg {
	switch name {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	default:
		return tea.KeyPressMsg{}
	}
}

func screenParseConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return cfg
}

func assertActionableScreenError(t *testing.T, err error) {
	t.Helper()
	for _, part := range []string{"what:", "why:", "where:", "when:", "means:", "fix:"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error missing %q: %v", part, err)
		}
	}
}
