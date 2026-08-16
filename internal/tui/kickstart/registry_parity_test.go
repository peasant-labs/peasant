package kickstart_test

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const (
	expectedParitySections          = 7
	expectedParityRows              = 6
	expectedForbiddenRegistryFields = 5
)

type parityOperation string

const (
	parityVisibility     parityOperation = "visibility"
	parityTransientDirty parityOperation = "transient-dirty"
	parityPersistedEdit  parityOperation = "persisted-edit"
	parityHiddenDrop     parityOperation = "hidden-drop"
	parityDriftFailure   parityOperation = "drift-failure"
)

func (o parityOperation) valid() bool {
	switch o {
	case parityVisibility, parityTransientDirty, parityPersistedEdit, parityHiddenDrop, parityDriftFailure:
		return true
	default:
		return false
	}
}

type paritySectionFixture struct {
	Key        string `yaml:"key"`
	Title      string `yaml:"title"`
	FieldKey   string `yaml:"fieldKey"`
	FieldKind  string `yaml:"fieldKind"`
	FieldText  string `yaml:"fieldText"`
	HasGuide   bool   `yaml:"hasGuide"`
	GuideIntro string `yaml:"guideIntro"`
	// FieldTextBeforeGuide distinguishes a field heading (true) from control
	// output used only as a mounted-presence assertion (false).
	FieldTextBeforeGuide bool `yaml:"fieldTextBeforeGuide"`
	HasExample           bool `yaml:"hasExample"`
	Conditional          bool `yaml:"conditional"`
}

type forbiddenRegistryOptionsFixture struct {
	ExpectedNameCount int      `yaml:"expectedNameCount"`
	Names             []string `yaml:"names"`
}

type parityRow struct {
	Name                    string               `yaml:"name"`
	Operation               parityOperation      `yaml:"operation"`
	SelectionMode           config.SelectionMode `yaml:"selectionMode"`
	VillageConnected        bool                 `yaml:"villageConnected"`
	ClaudeSessionsPresent   bool                 `yaml:"claudeSessionsPresent"`
	InitialRetention        int                  `yaml:"initialRetention"`
	SelectedRetention       int                  `yaml:"selectedRetention"`
	SelectedLicense         config.License       `yaml:"selectedLicense"`
	ExpectedAutoIngest      bool                 `yaml:"expectedAutoIngest"`
	ExpectedVisibleSections []string             `yaml:"expectedVisibleSections"`
}

type parityDocument struct {
	ExpectedSectionCount     int                             `yaml:"expectedSectionCount"`
	Sections                 []paritySectionFixture          `yaml:"sections"`
	ForbiddenRegistryOptions forbiddenRegistryOptionsFixture `yaml:"forbiddenRegistryOptions"`
	ExpectedRowCount         int                             `yaml:"expectedRowCount"`
	Rows                     []parityRow                     `yaml:"rows"`
}

//go:embed testdata/guided/registry_parity.yaml
var registryParityData []byte

func loadParityDocument(t *testing.T) parityDocument {
	t.Helper()
	var document parityDocument
	decodeSingleKnownFieldsDocument(t, "testdata/guided/registry_parity.yaml", registryParityData, &document)
	if document.ExpectedSectionCount != expectedParitySections || len(document.Sections) != expectedParitySections {
		t.Fatalf("parity section count: declared=%d actual=%d required=%d",
			document.ExpectedSectionCount, len(document.Sections), expectedParitySections)
	}
	if document.ExpectedRowCount != expectedParityRows || len(document.Rows) != expectedParityRows {
		t.Fatalf("parity row count: declared=%d actual=%d required=%d",
			document.ExpectedRowCount, len(document.Rows), expectedParityRows)
	}
	guard := document.ForbiddenRegistryOptions
	if guard.ExpectedNameCount != expectedForbiddenRegistryFields || len(guard.Names) != expectedForbiddenRegistryFields {
		t.Fatalf("forbidden registry option count: declared=%d actual=%d required=%d",
			guard.ExpectedNameCount, len(guard.Names), expectedForbiddenRegistryFields)
	}
	sectionKeys := map[string]bool{}
	for _, section := range document.Sections {
		if section.Key == "" || sectionKeys[section.Key] || section.Title == "" || section.FieldKey == "" ||
			section.FieldKind == "" || section.FieldText == "" || (section.HasGuide && section.GuideIntro == "") ||
			(!section.HasGuide && section.GuideIntro != "") {
			t.Fatalf("invalid or duplicate parity section fixture: %#v", section)
		}
		sectionKeys[section.Key] = true
	}
	rowNames := map[string]bool{}
	for _, row := range document.Rows {
		if row.Name == "" || rowNames[row.Name] || !row.Operation.valid() || !row.SelectionMode.IsValid() ||
			row.InitialRetention <= 0 || len(row.ExpectedVisibleSections) == 0 {
			t.Fatalf("invalid or duplicate parity row: %#v", row)
		}
		rowNames[row.Name] = true
		for _, key := range row.ExpectedVisibleSections {
			if !sectionKeys[key] {
				t.Fatalf("parity row %q expects unknown section %q", row.Name, key)
			}
		}
		switch row.Operation {
		case parityTransientDirty:
			if row.SelectedRetention <= 0 || row.SelectedRetention == row.InitialRetention {
				t.Fatalf("parity row %q does not declare a distinct retention edit", row.Name)
			}
		case parityPersistedEdit, parityDriftFailure:
			if !row.SelectedLicense.IsValid() {
				t.Fatalf("parity row %q declares invalid persisted license %q", row.Name, row.SelectedLicense)
			}
		}
	}
	return document
}

func newParityDraft(t *testing.T, row parityRow, suffix string) (*settings.Draft, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), suffix+".yaml")
	cfg := config.BaseConfig()
	cfg.Selection.Mode = row.SelectionMode
	if row.SelectionMode == config.SelectionModeSelected {
		cfg.Selection.AutoIngestNewBranches = true
		cfg.Selection.Harnesses = map[string]config.SelectionHarnessConfig{
			"claude-code": {Sessions: []string{"fixture-session"}},
		}
	}
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed %s parity config: %v", suffix, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s parity config: %v", suffix, err)
	}
	loaded, err := config.Parse(before)
	if err != nil {
		t.Fatalf("parse %s parity config: %v", suffix, err)
	}
	draft, err := settings.NewDraft(path, loaded)
	if err != nil {
		t.Fatalf("open %s parity draft: %v", suffix, err)
	}
	if err := kickstart.SeedRetentionInitial(draft, row.InitialRetention); err != nil {
		t.Fatalf("seed %s parity retention: %v", suffix, err)
	}
	return draft, path, before
}

func assertRegistryContract(t *testing.T, registry settings.Registry, fixtures []paritySectionFixture) {
	t.Helper()
	if len(registry.Sections) != len(fixtures) {
		t.Fatalf("canonical registry has %d sections, want %d", len(registry.Sections), len(fixtures))
	}
	for index, fixture := range fixtures {
		section := registry.Sections[index]
		if section.Key != fixture.Key || section.Title != fixture.Title {
			t.Errorf("section %d identity = %q/%q, want %q/%q",
				index, section.Key, section.Title, fixture.Key, fixture.Title)
		}
		if (section.When != nil) != fixture.Conditional {
			t.Errorf("section %q conditional = %t, want %t", section.Key, section.When != nil, fixture.Conditional)
		}
		if len(section.Fields) != 1 {
			t.Fatalf("section %q has %d fields, want one canonical field", section.Key, len(section.Fields))
		}
		field := section.Fields[0]
		if field.Key() != fixture.FieldKey || field.Kind().String() != fixture.FieldKind {
			t.Errorf("section %q field = %q/%s, want %q/%s",
				section.Key, field.Key(), field.Kind(), fixture.FieldKey, fixture.FieldKind)
		}
		if (section.Guide != nil) != fixture.HasGuide {
			t.Errorf("section %q guide presence = %t, want %t", section.Key, section.Guide != nil, fixture.HasGuide)
			continue
		}
		if fixture.HasGuide && (section.Guide.Intro != fixture.GuideIntro ||
			(section.Guide.Example != nil) != fixture.HasExample) {
			t.Errorf("section %q guide = %#v, want intro %q with example=%t",
				section.Key, section.Guide, fixture.GuideIntro, fixture.HasExample)
		}
	}
}

func visibleRegistryKeys(registry settings.Registry, draft *settings.Draft) []string {
	var keys []string
	for _, section := range registry.Sections {
		if section.When == nil || section.When(draft) {
			keys = append(keys, section.Key)
		}
	}
	return keys
}

func fixtureSectionByKey(t *testing.T, fixtures []paritySectionFixture, key string) paritySectionFixture {
	t.Helper()
	for _, fixture := range fixtures {
		if fixture.Key == key {
			return fixture
		}
	}
	t.Fatalf("parity fixture has no section %q", key)
	return paritySectionFixture{}
}

func collectFlowEvidence(t *testing.T, flow settings.Flow, expected []string, fixtures []paritySectionFixture) {
	t.Helper()
	var keys []string
	for step := 0; step <= len(fixtures) && !flow.OnReceipt(); step++ {
		key := flow.CurrentSectionKey()
		keys = append(keys, key)
		view := stripRender(flow.View())
		fixture := fixtureSectionByKey(t, fixtures, key)
		if key == kickstart.SectionSelection {
			assertSimplifiedSelectionRender(t, view)
			if !strings.Contains(view, fixture.FieldText) {
				t.Errorf("Flow selection section does not render its field-local search %q:\n%s", fixture.FieldText, view)
			}
			flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
			continue
		}
		guideAt := strings.Index(view, fixture.GuideIntro)
		fieldAt := strings.Index(view, fixture.FieldText)
		if guideAt < 0 || fieldAt < 0 {
			t.Errorf("Flow section %q omitted guide or canonical field %q: guide=%d field=%d\n%s",
				key, fixture.FieldText, guideAt, fieldAt, view)
		} else if fixture.FieldTextBeforeGuide && fieldAt >= guideAt {
			t.Errorf("Flow section %q did not render field heading %q before its guide: guide=%d field=%d\n%s",
				key, fixture.FieldText, guideAt, fieldAt, view)
		} else if !fixture.FieldTextBeforeGuide && guideAt >= fieldAt {
			t.Errorf("Flow section %q did not render its guide before mounted field output %q: guide=%d field=%d\n%s",
				key, fixture.FieldText, guideAt, fieldAt, view)
		}
		flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	if !reflect.DeepEqual(keys, expected) {
		t.Errorf("Flow visible section keys = %v, want %v", keys, expected)
	}
}

func collectScreenEvidence(t *testing.T, screen settings.Screen, expected []string, fixtures []paritySectionFixture) {
	t.Helper()
	for index, key := range expected {
		view := stripRender(screen.View())
		fixture := fixtureSectionByKey(t, fixtures, key)
		if !strings.Contains(view, fixture.FieldText) {
			t.Errorf("Screen selected section %q does not render canonical field text %q:\n%s", key, fixture.FieldText, view)
		}
		if key == kickstart.SectionSelection {
			assertSimplifiedSelectionRender(t, view)
		}
		for _, section := range fixtures {
			if section.GuideIntro != "" && strings.Contains(view, section.GuideIntro) {
				t.Errorf("dense Screen rendered Guide intro %q while section %q was selected", section.GuideIntro, key)
			}
		}
		if index+1 < len(expected) {
			screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		}
	}
	navigation := screenNavigationText(stripRender(screen.View()))
	visible := map[string]bool{}
	for _, key := range expected {
		visible[key] = true
	}
	for _, section := range fixtures {
		shown := strings.Contains(navigation, parityNavigationNeedle(section.Title))
		if shown != visible[section.Key] {
			t.Errorf("Screen section %q visible = %t, want %t; navigation:\n%s", section.Key, shown, visible[section.Key], navigation)
		}
	}
}

// screenNavigationText extracts the dense Screen's left navigation column from
// the rendered production frame. Field bodies can repeat section words, so
// visibility evidence must come from the actual section list rather than a
// whole-screen substring.
func screenNavigationText(view string) string {
	var navigation []string
	for _, line := range strings.Split(view, "\n") {
		parts := strings.Split(line, "│")
		if len(parts) >= 4 {
			navigation = append(navigation, parts[1])
		}
	}
	return strings.Join(navigation, "\n")
}

func parityNavigationNeedle(title string) string {
	runes := []rune(title)
	const maxNeedleRunes = 14
	if len(runes) > maxNeedleRunes {
		runes = runes[:maxNeedleRunes]
	}
	return string(runes)
}

func advanceParityFlow(t *testing.T, flow settings.Flow, key string, sectionCount int) settings.Flow {
	t.Helper()
	for step := 0; step <= sectionCount && !flow.OnReceipt(); step++ {
		if flow.CurrentSectionKey() == key {
			return flow
		}
		flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	t.Fatalf("Flow never reached section %q", key)
	return flow
}

func selectParityScreenSection(t *testing.T, screen settings.Screen, visible []string, key string) settings.Screen {
	t.Helper()
	index := -1
	for candidate, visibleKey := range visible {
		if visibleKey == key {
			index = candidate
			break
		}
	}
	if index < 0 {
		t.Fatalf("Screen visibility fixture has no section %q", key)
	}
	for step := 0; step < index; step++ {
		screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return screen
}

func chooseNextFlowRadio(t *testing.T, flow settings.Flow, key string, sectionCount int) settings.Flow {
	t.Helper()
	flow = advanceParityFlow(t, flow, key, sectionCount)
	flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	return flow
}

func chooseNextScreenRadio(t *testing.T, screen settings.Screen, visible []string, key string) settings.Screen {
	t.Helper()
	screen = selectParityScreenSection(t, screen, visible, key)
	screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	screen, _ = screen.Update(tea.KeyPressMsg{Code: tea.KeySpace, Text: " "})
	return screen
}

func parityField(t *testing.T, registry settings.Registry, sectionKey, fieldKey string) settings.Field {
	t.Helper()
	for _, section := range registry.Sections {
		if section.Key != sectionKey {
			continue
		}
		for _, field := range section.Fields {
			if field.Key() == fieldKey {
				return field
			}
		}
	}
	t.Fatalf("canonical registry has no field %q/%q", sectionKey, fieldKey)
	return nil
}

func commitParityFlow(t *testing.T, flow settings.Flow, sectionCount int) settings.Flow {
	t.Helper()
	for step := 0; step <= sectionCount && !flow.OnReceipt(); step++ {
		flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	}
	if !flow.OnReceipt() {
		t.Fatal("Flow did not reach its final receipt")
	}
	flow, _ = flow.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	return flow
}

func saveParityScreen(screen settings.Screen) (settings.Screen, tea.Msg) {
	next, command := screen.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if command == nil {
		return next, nil
	}
	return next, command()
}

func parseParityConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read parity config %q: %v", path, err)
	}
	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse parity config %q: %v", path, err)
	}
	return parsed
}

func TestBuildRegistryFlowScreenParity(t *testing.T) {
	document := loadParityDocument(t)
	th := theme.New(theme.ModeDark)
	for _, row := range document.Rows {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			flowDraft, flowPath, flowBefore := newParityDraft(t, row, "flow")
			screenDraft, screenPath, screenBefore := newParityDraft(t, row, "screen")
			registry := kickstart.BuildRegistry(kickstart.Options{
				Source:                scannerfix.NewFixtureTreeSource("standard"),
				VillageConnected:      row.VillageConnected,
				ClaudeSessionsPresent: row.ClaudeSessionsPresent,
			})
			assertRegistryContract(t, registry, document.Sections)

			flow := settings.NewFlow(th, registry, flowDraft)
			flow.SetSize(120, 40)
			screen := settings.NewScreen(th, registry, screenDraft)
			screen.SetSize(120, 40)
			if screen.Err() != nil {
				t.Fatalf("mount dense Screen: %v", screen.Err())
			}

			if got := visibleRegistryKeys(registry, flowDraft); !reflect.DeepEqual(got, row.ExpectedVisibleSections) {
				t.Fatalf("canonical registry visibility = %v, want %v", got, row.ExpectedVisibleSections)
			}
			collectFlowEvidence(t, flow, row.ExpectedVisibleSections, document.Sections)
			collectScreenEvidence(t, screen, row.ExpectedVisibleSections, document.Sections)

			flow = settings.NewFlow(th, registry, flowDraft)
			flow.SetSize(120, 40)
			screen = settings.NewScreen(th, registry, screenDraft)
			screen.SetSize(120, 40)
			flow = drainSettingsFlowInit(flow, flow.Init())
			screen = drainParityScreenInit(screen)

			switch row.Operation {
			case parityVisibility:
				return
			case parityTransientDirty:
				flow = chooseNextFlowRadio(t, flow, kickstart.SectionRetention, len(document.Sections))
				screen = chooseNextScreenRadio(t, screen, row.ExpectedVisibleSections, kickstart.SectionRetention)
				if flowDraft.Working().ClaudeRetentionDays != row.SelectedRetention ||
					screenDraft.Working().ClaudeRetentionDays != row.SelectedRetention {
					t.Errorf("retention edits = %d/%d, want %d in both presentations",
						flowDraft.Working().ClaudeRetentionDays, screenDraft.Working().ClaudeRetentionDays, row.SelectedRetention)
				}
				field := parityField(t, registry, kickstart.SectionRetention, kickstart.FieldRetention)
				if !field.Dirty(flowDraft) || !field.Dirty(screenDraft) || !screen.Dirty() {
					t.Error("transient retention edit did not share field and Screen dirty semantics")
				}
				if flowDraft.Dirty() || screenDraft.Dirty() {
					t.Error("transient retention edit leaked into YAML-backed Draft.Dirty")
				}
			case parityPersistedEdit:
				flow = chooseNextFlowRadio(t, flow, kickstart.SectionLicense, len(document.Sections))
				screen = chooseNextScreenRadio(t, screen, row.ExpectedVisibleSections, kickstart.SectionLicense)
				flow = commitParityFlow(t, flow, len(document.Sections))
				var message tea.Msg
				screen, message = saveParityScreen(screen)
				saved, ok := message.(settings.SavedMsg)
				if !ok || saved.Draft() != screenDraft || screen.Err() != nil || !flow.Committed() || flow.Err() != nil {
					t.Fatalf("save results Flow committed/err=%t/%v Screen message/err=%T/%v",
						flow.Committed(), flow.Err(), message, screen.Err())
				}
				flowConfig := parseParityConfig(t, flowPath)
				screenConfig := parseParityConfig(t, screenPath)
				if flowConfig.Push.License != row.SelectedLicense || screenConfig.Push.License != row.SelectedLicense ||
					!reflect.DeepEqual(flowConfig, screenConfig) {
					t.Errorf("persisted configs diverged or missed license %q:\nFlow=%#v\nScreen=%#v",
						row.SelectedLicense, flowConfig, screenConfig)
				}
			case parityHiddenDrop:
				flowDraft.Working().Selection.AutoIngestNewBranches = false
				screenDraft.Working().Selection.AutoIngestNewBranches = false
				field := parityField(t, registry, kickstart.SectionAutoIngest, kickstart.FieldAutoIngest)
				if !field.Dirty(flowDraft) || !field.Dirty(screenDraft) {
					t.Fatal("precondition: hidden-section field edit is not dirty")
				}
				flowDraft.Working().Selection.Mode = config.SelectionModeAll
				flowDraft.Working().Selection.Harnesses = nil
				screenDraft.Working().Selection.Mode = config.SelectionModeAll
				screenDraft.Working().Selection.Harnesses = nil
				flow = commitParityFlow(t, flow, len(document.Sections))
				var message tea.Msg
				screen, message = saveParityScreen(screen)
				if _, ok := message.(settings.SavedMsg); !ok || screen.Err() != nil || !flow.Committed() || flow.Err() != nil {
					t.Fatalf("hidden-drop saves failed: Flow committed/err=%t/%v Screen message/err=%T/%v",
						flow.Committed(), flow.Err(), message, screen.Err())
				}
				flowConfig := parseParityConfig(t, flowPath)
				screenConfig := parseParityConfig(t, screenPath)
				if flowConfig.Selection.AutoIngestNewBranches != row.ExpectedAutoIngest ||
					screenConfig.Selection.AutoIngestNewBranches != row.ExpectedAutoIngest ||
					!reflect.DeepEqual(flowConfig, screenConfig) {
					t.Errorf("hidden edit drop diverged: Flow=%#v Screen=%#v", flowConfig.Selection, screenConfig.Selection)
				}
			case parityDriftFailure:
				flow = chooseNextFlowRadio(t, flow, kickstart.SectionLicense, len(document.Sections))
				screen = chooseNextScreenRadio(t, screen, row.ExpectedVisibleSections, kickstart.SectionLicense)
				flowExternal := append(append([]byte(nil), flowBefore...), []byte("\n# external flow edit\n")...)
				screenExternal := append(append([]byte(nil), screenBefore...), []byte("\n# external screen edit\n")...)
				if err := os.WriteFile(flowPath, flowExternal, 0o600); err != nil {
					t.Fatalf("write external Flow edit: %v", err)
				}
				if err := os.WriteFile(screenPath, screenExternal, 0o600); err != nil {
					t.Fatalf("write external Screen edit: %v", err)
				}
				flow = commitParityFlow(t, flow, len(document.Sections))
				var message tea.Msg
				screen, message = saveParityScreen(screen)
				if flow.Committed() || flow.Err() == nil || screen.Err() == nil || message != nil {
					t.Errorf("drift did not fail closed: Flow committed/err=%t/%v Screen message/err=%T/%v",
						flow.Committed(), flow.Err(), message, screen.Err())
				}
				flowAfter, _ := os.ReadFile(flowPath)
				screenAfter, _ := os.ReadFile(screenPath)
				if !bytes.Equal(flowAfter, flowExternal) || !bytes.Equal(screenAfter, screenExternal) {
					t.Error("a presentation overwrote the external drift bytes")
				}
			}
		})
	}
}

func drainParityScreenInit(screen settings.Screen) settings.Screen {
	for _, message := range collectMsgs(screen.Init()) {
		screen, _ = screen.Update(message)
	}
	return screen
}

func TestBuildRegistryHasNoPresentationOptions(t *testing.T) {
	document := loadParityDocument(t)
	typ := reflect.TypeOf(kickstart.Options{})
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		descriptor := strings.ToLower(field.Name + " " + field.Type.String())
		for _, forbidden := range document.ForbiddenRegistryOptions.Names {
			if strings.Contains(descriptor, strings.ToLower(forbidden)) {
				t.Errorf("kickstart.Options field %s introduces forbidden presentation control %q", field.Name, forbidden)
			}
		}
	}
}
