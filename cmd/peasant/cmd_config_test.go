package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings/scannerfix"
)

const (
	expectedRetentionFixtureRows      = 6
	expectedConfigAliasFixtureRows    = 2
	expectedPartialSuccessFixtureRows = 1
	expectedConfigAuthorityRows       = 8
	expectedSaveOrderRows             = 1
)

//go:embed testdata/config-screen/retention.yaml
var configRetentionFixtureYAML []byte

//go:embed testdata/config-screen/aliases.yaml
var configAliasFixtureYAML []byte

//go:embed testdata/config-screen/partial-success.yaml
var configPartialSuccessFixtureYAML []byte

//go:embed testdata/config-screen/authority.yaml
var configAuthorityFixtureYAML []byte

//go:embed testdata/config-screen/save-order.yaml
var configSaveOrderFixtureYAML []byte

type configRetentionScenario string

const (
	configRetentionClean      configRetentionScenario = "clean-save"
	configRetentionDirty      configRetentionScenario = "retention-only-dirty"
	configRetentionDiscard    configRetentionScenario = "discard"
	configRetentionOrdered    configRetentionScenario = "ordered-save"
	configRetentionValidation configRetentionScenario = "validation-failure"
	configRetentionDrift      configRetentionScenario = "drift-failure"
)

func (s configRetentionScenario) valid() bool {
	switch s {
	case configRetentionClean, configRetentionDirty, configRetentionDiscard,
		configRetentionOrdered, configRetentionValidation, configRetentionDrift:
		return true
	}
	return false
}

type configRetentionFixture struct {
	Name              string                  `yaml:"name"`
	Scenario          configRetentionScenario `yaml:"scenario"`
	InitialRetention  int                     `yaml:"initialRetention"`
	SelectedRetention int                     `yaml:"selectedRetention"`
	EditLicense       bool                    `yaml:"editLicense"`
	ExpectScreenDirty bool                    `yaml:"expectScreenDirty"`
	ExpectDraftDirty  bool                    `yaml:"expectDraftDirty"`
	ExpectWriterCalls int                     `yaml:"expectWriterCalls"`
	ViewScanSteps     int                     `yaml:"viewScanSteps"`
	WantViewContains  []string                `yaml:"wantViewContains"`
	WantViewMissing   []string                `yaml:"wantViewMissing"`
}

type configRetentionDocument struct {
	ExpectedRows int                      `yaml:"expectedRows"`
	Cases        []configRetentionFixture `yaml:"cases"`
}

type configAliasFixture struct {
	Name    string `yaml:"name"`
	Command string `yaml:"command"`
}

type configAliasDocument struct {
	ExpectedRows int                  `yaml:"expectedRows"`
	Cases        []configAliasFixture `yaml:"cases"`
}

type configPartialSuccessFixture struct {
	Name              string `yaml:"name"`
	InitialRetention  int    `yaml:"initialRetention"`
	SelectedRetention int    `yaml:"selectedRetention"`
	WriterError       string `yaml:"writerError"`
}

type configPartialSuccessDocument struct {
	ExpectedRows int                           `yaml:"expectedRows"`
	Cases        []configPartialSuccessFixture `yaml:"cases"`
}

type configAuthorityDocument struct {
	ExpectedRows int      `yaml:"expectedRows"`
	Forbidden    []string `yaml:"forbidden"`
}

type configSaveOrderFixture struct {
	Name              string   `yaml:"name"`
	InitialRetention  int      `yaml:"initialRetention"`
	SelectedRetention int      `yaml:"selectedRetention"`
	ExpectWriterCalls int      `yaml:"expectWriterCalls"`
	ReorderedInput    []string `yaml:"reorderedInput"`
	SaveInstruction   string   `yaml:"saveInstruction"`
}

type configSaveOrderDocument struct {
	ExpectedRows int                      `yaml:"expectedRows"`
	Cases        []configSaveOrderFixture `yaml:"cases"`
}

func loadConfigRetentionFixtures(t *testing.T) []configRetentionFixture {
	t.Helper()
	var document configRetentionDocument
	if err := decodeConfigScreenFixture("retention.yaml", configRetentionFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedRetentionFixtureRows || len(document.Cases) != expectedRetentionFixtureRows {
		t.Fatalf("retention fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Cases), expectedRetentionFixtureRows)
	}
	for _, fixture := range document.Cases {
		if fixture.Name == "" || !fixture.Scenario.valid() {
			t.Fatalf("invalid retention fixture: name=%q scenario=%q", fixture.Name, fixture.Scenario)
		}
	}
	return document.Cases
}

func loadConfigAliasFixtures(t *testing.T) []configAliasFixture {
	t.Helper()
	var document configAliasDocument
	if err := decodeConfigScreenFixture("aliases.yaml", configAliasFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedConfigAliasFixtureRows || len(document.Cases) != expectedConfigAliasFixtureRows {
		t.Fatalf("alias fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Cases), expectedConfigAliasFixtureRows)
	}
	return document.Cases
}

func loadConfigPartialSuccessFixtures(t *testing.T) []configPartialSuccessFixture {
	t.Helper()
	var document configPartialSuccessDocument
	if err := decodeConfigScreenFixture("partial-success.yaml", configPartialSuccessFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedPartialSuccessFixtureRows || len(document.Cases) != expectedPartialSuccessFixtureRows {
		t.Fatalf("partial-success fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Cases), expectedPartialSuccessFixtureRows)
	}
	return document.Cases
}

func loadConfigAuthorityFixture(t *testing.T) configAuthorityDocument {
	t.Helper()
	var document configAuthorityDocument
	if err := decodeConfigScreenFixture("authority.yaml", configAuthorityFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedConfigAuthorityRows || len(document.Forbidden) != expectedConfigAuthorityRows {
		t.Fatalf("authority fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Forbidden), expectedConfigAuthorityRows)
	}
	return document
}

func loadConfigSaveOrderFixtures(t *testing.T) []configSaveOrderFixture {
	t.Helper()
	var document configSaveOrderDocument
	if err := decodeConfigScreenFixture("save-order.yaml", configSaveOrderFixtureYAML, &document); err != nil {
		t.Fatal(err)
	}
	if document.ExpectedRows != expectedSaveOrderRows || len(document.Cases) != expectedSaveOrderRows {
		t.Fatalf("save-order fixture rows: header=%d actual=%d want=%d", document.ExpectedRows, len(document.Cases), expectedSaveOrderRows)
	}
	for _, fixture := range document.Cases {
		if fixture.Name == "" || fixture.InitialRetention <= 0 || fixture.SelectedRetention <= 0 || fixture.ExpectWriterCalls != 1 || len(fixture.ReorderedInput) == 0 || fixture.SaveInstruction == "" {
			t.Fatalf("invalid save-order fixture: %+v", fixture)
		}
	}
	return document.Cases
}

func decodeConfigScreenFixture(name string, data []byte, destination any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s with known fields: %w", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("%s must contain exactly one YAML document", name)
		}
		return fmt.Errorf("decode trailing %s document: %w", name, err)
	}
	return nil
}

func TestConfigScreenFixtureDecoder_StrictAndSingleDocument(t *testing.T) {
	var unknown configRetentionDocument
	withUnknown := append([]byte("unexpectedField: true\n"), configRetentionFixtureYAML...)
	if err := decodeConfigScreenFixture("retention.yaml", withUnknown, &unknown); err == nil || !strings.Contains(err.Error(), "field unexpectedField not found") {
		t.Fatalf("unknown field error=%v want strict rejection", err)
	}

	var trailing configRetentionDocument
	withTrailing := append(append([]byte(nil), configRetentionFixtureYAML...), []byte("\n---\nexpectedRows: 0\n")...)
	if err := decodeConfigScreenFixture("retention.yaml", withTrailing, &trailing); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing document error=%v want single-document rejection", err)
	}
}

func TestConfigCommand_RetentionFixtures(t *testing.T) {
	for _, fixture := range loadConfigRetentionFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			world := newConfigScreenWorld(t, fixture.InitialRetention)
			world.scenario = fixture.Scenario
			world.selectedRetention = fixture.SelectedRetention
			world.editLicense = fixture.EditLicense
			world.sourceName = "standard"
			world.viewScanSteps = fixture.ViewScanSteps
			if fixture.Scenario == configRetentionValidation {
				world.sourceName = "conflict"
				world.useSelectedConfig(t)
			}

			cmd := buildConfigCommand(world.dependencies(t))
			stdout, err := executeConfigScreenCommand(t, cmd, world, "config")
			if err != nil {
				t.Fatalf("config command: %v", err)
			}
			if world.observedScreenDirty != fixture.ExpectScreenDirty {
				t.Fatalf("Screen.Dirty=%t want=%t", world.observedScreenDirty, fixture.ExpectScreenDirty)
			}
			if world.observedDraftDirty != fixture.ExpectDraftDirty {
				t.Fatalf("Draft.Dirty=%t want=%t", world.observedDraftDirty, fixture.ExpectDraftDirty)
			}
			if world.writerCalls != fixture.ExpectWriterCalls {
				t.Fatalf("retention writer calls=%d want=%d", world.writerCalls, fixture.ExpectWriterCalls)
			}
			if len(world.writerValues) != fixture.ExpectWriterCalls {
				t.Fatalf("retention writer values=%v want %d calls", world.writerValues, fixture.ExpectWriterCalls)
			}
			world.assertScenarioOutcome(t, fixture, stdout)
		})
	}
}

func TestConfigCommand_AliasParityFixtures(t *testing.T) {
	for _, fixture := range loadConfigAliasFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			world := newConfigScreenWorld(t, 90)
			world.scenario = configRetentionDirty
			world.selectedRetention = 365
			world.sourceName = "standard"
			cmd := buildConfigCommand(world.dependencies(t))
			if _, err := executeConfigScreenCommand(t, cmd, world, fixture.Command); err != nil {
				t.Fatalf("execute %s: %v", fixture.Command, err)
			}
			if world.runnerCalls != 1 || !world.observedScreenDirty {
				t.Fatalf("%s mounted runner=%d dirty=%t; want the shared Screen path", fixture.Command, world.runnerCalls, world.observedScreenDirty)
			}
		})
	}
}

func TestConfigCommand_ProductionRegistrationFixtures(t *testing.T) {
	for _, fixture := range loadConfigAliasFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			root := buildRootCommand()
			primary, _, findErr := root.Find([]string{"config"})
			if findErr != nil || primary == nil || primary.Name() != "config" || primary.RunE == nil {
				t.Fatalf("production config command is not mounted: command=%#v error=%v", primary, findErr)
			}
			resolved, remaining, findErr := root.Find([]string{fixture.Command})
			if findErr != nil {
				t.Fatalf("find production command %q: %v", fixture.Command, findErr)
			}
			if len(remaining) != 0 {
				t.Fatalf("production command %q left unresolved args %v", fixture.Command, remaining)
			}
			if resolved != primary {
				t.Fatalf("production command %q resolved to %#v, want mounted config", fixture.Command, resolved)
			}
		})
	}
}

func TestConfigCommand_SavePendingFreezesMountedModel(t *testing.T) {
	for _, fixture := range loadConfigSaveOrderFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			world := newConfigScreenWorld(t, fixture.InitialRetention)
			world.selectedRetention = fixture.SelectedRetention
			world.editLicense = true
			world.sourceName = "standard"
			deps := world.dependencies(t)
			deps.run = func(model tea.Model) (tea.Model, error) {
				model = configScreenDrain(model, model.Init())
				model = configScreenUpdate(model, tea.WindowSizeMsg{Width: 100, Height: 28})
				if view := model.(*configScreenModel).screen.View(); !strings.Contains(view, fixture.SaveInstruction) {
					t.Fatalf("mounted config screen missing save instruction %q", fixture.SaveInstruction)
				}
				model = world.editRetention(model)
				model = world.editLicenseFromRetention(model)
				model = configScreenUpdate(model, configScreenKey("tab"))
				model = configScreenUpdate(model, configScreenKey("down"))
				model = configScreenUpdate(model, configScreenKey("enter"))

				model, savedCmd := model.Update(configScreenKey("ctrl+s"))
				if savedCmd == nil {
					t.Fatal("mounted save returned no completion command")
				}
				for _, key := range fixture.ReorderedInput {
					message := configScreenKey(key)
					var blocked tea.Cmd
					model, blocked = model.Update(message)
					if blocked != nil {
						t.Fatalf("pending save accepted reordered %T input", message)
					}
				}
				host := model.(*configScreenModel)
				if got := host.draft.Working().ClaudeRetentionDays; got != fixture.SelectedRetention {
					t.Fatalf("pending mounted save retention=%d want committed=%d", got, fixture.SelectedRetention)
				}
				completion := savedCmd()
				model, _ = model.Update(completion)
				return model, nil
			}

			if _, err := executeConfigScreenCommand(t, buildConfigCommand(deps), world, "config"); err != nil {
				t.Fatalf("mounted reordered save: %v", err)
			}
			if world.writerCalls != fixture.ExpectWriterCalls || len(world.writerValues) != fixture.ExpectWriterCalls {
				t.Fatalf("retention attempts: calls=%d values=%v want=%d", world.writerCalls, world.writerValues, fixture.ExpectWriterCalls)
			}
			if world.writerValues[0] != fixture.SelectedRetention {
				t.Fatalf("retention attempt=%d want committed=%d", world.writerValues[0], fixture.SelectedRetention)
			}
			if got, _ := ftue.ReadClaudeCleanupDaysAt(world.claudePath); got != fixture.SelectedRetention {
				t.Fatalf("persisted cleanupPeriodDays=%d want=%d", got, fixture.SelectedRetention)
			}
		})
	}
}

func TestConfigCommand_PartialSuccessIsActionable(t *testing.T) {
	for _, fixture := range loadConfigPartialSuccessFixtures(t) {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			world := newConfigScreenWorld(t, fixture.InitialRetention)
			world.scenario = configRetentionOrdered
			world.selectedRetention = fixture.SelectedRetention
			world.editLicense = true
			world.sourceName = "standard"
			world.writerErr = errors.New(fixture.WriterError)

			_, err := executeConfigScreenCommand(t, buildConfigCommand(world.dependencies(t)), world, "config")
			if err == nil {
				t.Fatal("post-commit retention failure returned success")
			}
			assertActionableConfigError(t, err)
			for _, value := range []string{world.configPath, world.claudePath, fixture.WriterError, "remains committed", "did not change"} {
				if !strings.Contains(err.Error(), value) {
					t.Fatalf("partial-success error missing %q: %v", value, err)
				}
			}
			if got := configScreenLicense(t, world.configPath); got == "" {
				t.Fatal("config did not remain committed: license is empty")
			}
			if got, _ := ftue.ReadClaudeCleanupDaysAt(world.claudePath); got != fixture.InitialRetention {
				t.Fatalf("Claude retention changed after writer failure: got=%d want=%d", got, fixture.InitialRetention)
			}
			if world.writerCalls != 1 || len(world.writerValues) != 1 || world.writerValues[0] != fixture.SelectedRetention {
				t.Fatalf("retention attempts: calls=%d values=%v want one %d", world.writerCalls, world.writerValues, fixture.SelectedRetention)
			}
		})
	}
}

func TestConfigCommand_AuthorityBoundaryFixture(t *testing.T) {
	document := loadConfigAuthorityFixture(t)
	source, err := os.ReadFile("cmd_config.go")
	if err != nil {
		t.Fatalf("read cmd_config.go: %v", err)
	}
	for _, forbidden := range document.Forbidden {
		if bytes.Contains(source, []byte(forbidden)) {
			t.Fatalf("config command gained forbidden authority %q", forbidden)
		}
	}
}

type configScreenWorld struct {
	dir        string
	configPath string
	claudePath string

	initialConfig []byte
	initialClaude []byte
	externalEdit  []byte

	scenario          configRetentionScenario
	selectedRetention int
	editLicense       bool
	sourceName        string
	writerErr         error

	runnerCalls         int
	writerCalls         int
	writerValues        []int
	observedScreenDirty bool
	observedDraftDirty  bool
	observedScreenErr   error
	writerSawCommitted  bool
	observedViews       string
	viewScanSteps       int
}

func newConfigScreenWorld(t *testing.T, retention int) *configScreenWorld {
	t.Helper()
	dir := t.TempDir()
	world := &configScreenWorld{
		dir:        dir,
		configPath: filepath.Join(dir, "config.yaml"),
		claudePath: filepath.Join(dir, ".claude", "settings.json"),
	}
	cfg := config.BaseConfig()
	if err := config.SaveAtomic(world.configPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(world.claudePath), defaults.PrivateDirPerm); err != nil {
		t.Fatalf("create Claude settings dir: %v", err)
	}
	claude := fmt.Sprintf("{\n  \"cleanupPeriodDays\": %d,\n  \"theme\": \"dark\",\n  \"nested\": {\"keep\": true}\n}\n", retention)
	if err := os.WriteFile(world.claudePath, []byte(claude), defaults.PrivateFilePerm); err != nil {
		t.Fatalf("seed Claude settings: %v", err)
	}
	world.initialConfig = mustReadConfigScreenFile(t, world.configPath)
	world.initialClaude = mustReadConfigScreenFile(t, world.claudePath)
	return world
}

func (w *configScreenWorld) useSelectedConfig(t *testing.T) {
	t.Helper()
	data := mustReadConfigScreenFile(t, w.configPath)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse config before selected-mode fixture: %v", err)
	}
	cfg.Selection.Mode = config.SelectionModeSelected
	if err := config.SaveAtomic(w.configPath, cfg); err != nil {
		t.Fatalf("seed selected-mode config: %v", err)
	}
	w.initialConfig = mustReadConfigScreenFile(t, w.configPath)
}

func (w *configScreenWorld) dependencies(t *testing.T) configCommandDeps {
	t.Helper()
	return configCommandDeps{
		discover: func(context.Context, string, string) configDiscovery {
			var source kit.TreeSource = kickstart.NewScannerTreeSource([]ftue.SessionListing{
				{
					Harness:     string(defaults.HarnessClaudeCode),
					ProjectName: "fixture project",
					SessionID:   "fixture-session",
				},
			})
			if w.sourceName == "conflict" {
				source = scannerfix.NewFixtureTreeSource(w.sourceName)
			}
			return configDiscovery{
				inventory: ftue.ProviderInventory{
					defaults.HarnessClaudeCode: {SessionCount: 1, Enabled: true},
				},
				source: source,
			}
		},
		retentionPath: func() (string, error) { return w.claudePath, nil },
		readRetention: ftue.ReadClaudeCleanupDaysAt,
		retentionWriter: kickstart.RetentionWriterFunc(func(days int) error {
			w.writerCalls++
			w.writerValues = append(w.writerValues, days)
			w.writerSawCommitted = configScreenLicense(t, w.configPath) != ""
			if w.writerErr != nil {
				return w.writerErr
			}
			return ftue.WriteClaudeCleanupDaysAt(w.claudePath, days)
		}),
		run: w.run,
	}
}

func (w *configScreenWorld) run(model tea.Model) (tea.Model, error) {
	w.runnerCalls++
	model = configScreenDrain(model, model.Init())
	model = configScreenUpdate(model, tea.WindowSizeMsg{Width: 100, Height: 28})

	switch w.scenario {
	case configRetentionClean:
		model = w.observe(model)
		model = configScreenUpdate(model, configScreenKey("ctrl+s"))
	case configRetentionDirty:
		model = w.editRetention(model)
		model = w.observe(model)
	case configRetentionDiscard:
		model = w.editRetention(model)
		model = w.editLicenseFromRetention(model)
		model = w.observe(model)
		model = configScreenUpdate(model, configScreenKey("esc"))
		model = configScreenUpdate(model, configScreenKey("left"))
		model = configScreenUpdate(model, configScreenKey("enter"))
	case configRetentionOrdered:
		model = w.editRetention(model)
		model = w.editLicenseFromRetention(model)
		model = w.observe(model)
		model = configScreenUpdate(model, configScreenKey("ctrl+s"))
	case configRetentionValidation:
		model = w.observe(model)
		model = configScreenUpdate(model, configScreenKey("ctrl+s"))
		model = w.observeError(model)
	case configRetentionDrift:
		model = w.editRetention(model)
		model = w.editLicenseFromRetention(model)
		model = w.observe(model)
		external := config.BaseConfig()
		external.User.Email = "external-edit@example.test"
		if err := config.SaveAtomic(w.configPath, external); err != nil {
			return model, err
		}
		var err error
		w.externalEdit, err = os.ReadFile(w.configPath)
		if err != nil {
			return model, err
		}
		model = configScreenUpdate(model, configScreenKey("ctrl+s"))
		model = w.observeError(model)
	}
	return model, nil
}

func (w *configScreenWorld) editRetention(model tea.Model) tea.Model {
	model = configScreenUpdate(model, configScreenKey("down"))
	model = configScreenUpdate(model, configScreenKey("down"))
	model = configScreenUpdate(model, configScreenKey("down"))
	model = configScreenUpdate(model, configScreenKey("enter"))
	current := ftueCleanupIndex(w.initialRetention())
	target := ftueCleanupIndex(w.selectedRetention)
	for current < target {
		model = configScreenUpdate(model, configScreenKey("down"))
		current++
	}
	for current > target {
		model = configScreenUpdate(model, configScreenKey("up"))
		current--
	}
	return configScreenUpdate(model, configScreenKey("space"))
}

func (w *configScreenWorld) editLicenseFromRetention(model tea.Model) tea.Model {
	if !w.editLicense {
		return model
	}
	model = configScreenUpdate(model, configScreenKey("tab"))
	model = configScreenUpdate(model, configScreenKey("up"))
	model = configScreenUpdate(model, configScreenKey("enter"))
	model = configScreenUpdate(model, configScreenKey("down"))
	return configScreenUpdate(model, configScreenKey("space"))
}

func (w *configScreenWorld) initialRetention() int {
	days, _ := ftue.ReadClaudeCleanupDaysAt(w.claudePath)
	return days
}

func (w *configScreenWorld) observe(model tea.Model) tea.Model {
	if host, ok := model.(*configScreenModel); ok {
		w.observedScreenDirty = host.screen.Dirty()
		w.observedDraftDirty = host.draft.Dirty()
		w.observedScreenErr = host.screen.Err()
		model, w.observedViews = configScreenVisibleViews(model, w.viewScanSteps)
	}
	return model
}

func configScreenVisibleViews(model tea.Model, steps int) (tea.Model, string) {
	if steps < 1 {
		steps = 1
	}
	views := make([]string, 0, steps)
	current := model
	for step := 0; step < steps; step++ {
		views = append(views, ansiPattern.ReplaceAllString(current.View().Content, ""))
		if step+1 < steps {
			current = configScreenUpdate(current, configScreenKey("down"))
		}
	}
	return current, strings.Join(views, "\n")
}

func (w *configScreenWorld) observeError(model tea.Model) tea.Model {
	return w.observe(model)
}

func (w *configScreenWorld) assertScenarioOutcome(t *testing.T, fixture configRetentionFixture, stdout string) {
	t.Helper()
	for _, want := range fixture.WantViewContains {
		if !strings.Contains(w.observedViews, want) {
			t.Errorf("config Screen views do not contain %q:\n%s", want, w.observedViews)
		}
	}
	for _, missing := range fixture.WantViewMissing {
		if strings.Contains(w.observedViews, missing) {
			t.Errorf("config Screen views contain guided-only instruction %q:\n%s", missing, w.observedViews)
		}
	}
	configAfter := mustReadConfigScreenFile(t, w.configPath)
	claudeAfter := mustReadConfigScreenFile(t, w.claudePath)
	switch fixture.Scenario {
	case configRetentionClean, configRetentionDirty, configRetentionDiscard, configRetentionValidation:
		if !bytes.Equal(configAfter, w.initialConfig) || !bytes.Equal(claudeAfter, w.initialClaude) {
			t.Fatalf("%s changed persisted bytes", fixture.Scenario)
		}
	case configRetentionOrdered:
		if bytes.Equal(configAfter, w.initialConfig) || bytes.Equal(claudeAfter, w.initialClaude) {
			t.Fatal("ordered save did not change both persisted files")
		}
		if !w.writerSawCommitted {
			t.Fatal("retention writer ran before the YAML-backed config commit")
		}
		if w.writerCalls != 1 || len(w.writerValues) != 1 || w.writerValues[0] != fixture.SelectedRetention {
			t.Fatalf("retention write: calls=%d values=%v want one %d", w.writerCalls, w.writerValues, fixture.SelectedRetention)
		}
		if got, _ := ftue.ReadClaudeCleanupDaysAt(w.claudePath); got != fixture.SelectedRetention {
			t.Fatalf("persisted cleanupPeriodDays=%d want=%d", got, fixture.SelectedRetention)
		}
		if !bytes.Contains(claudeAfter, []byte(`"theme": "dark"`)) || !bytes.Contains(claudeAfter, []byte(`"keep": true`)) {
			t.Fatal("merge-preserving retention writer dropped unrelated Claude settings")
		}
		if !strings.Contains(stdout, w.configPath) || !strings.Contains(stdout, w.claudePath) {
			t.Fatalf("save summary does not name both persisted paths: %q", stdout)
		}
	case configRetentionDrift:
		if !bytes.Equal(configAfter, w.externalEdit) || !bytes.Equal(claudeAfter, w.initialClaude) {
			t.Fatal("drift failure did not preserve the external config and Claude bytes")
		}
	}
	if fixture.Scenario == configRetentionValidation || fixture.Scenario == configRetentionDrift {
		if w.observedScreenErr == nil {
			t.Fatalf("%s did not surface a visible Screen error", fixture.Scenario)
		}
		assertActionableConfigError(t, w.observedScreenErr)
	}
}

func executeConfigScreenCommand(t *testing.T, command *cobra.Command, world *configScreenWorld, name string) (string, error) {
	t.Helper()
	root := &cobra.Command{Use: "peasant", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("config-dir", "", "")
	root.PersistentFlags().String("data-dir", "", "")
	root.AddCommand(command)
	var stdout bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"--config", world.configPath, "--data-dir", world.dir, name})
	err := root.ExecuteContext(context.Background())
	return stdout.String(), err
}

func configScreenUpdate(model tea.Model, message tea.Msg) tea.Model {
	updated, cmd := model.Update(message)
	return configScreenDrain(updated, cmd)
}

func configScreenDrain(model tea.Model, cmd tea.Cmd) tea.Model {
	for _, message := range configScreenMessages(cmd) {
		var next tea.Cmd
		model, next = model.Update(message)
		model = configScreenDrain(model, next)
	}
	return model
}

func configScreenMessages(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	message := cmd()
	if batch, ok := message.(tea.BatchMsg); ok {
		var messages []tea.Msg
		for _, child := range batch {
			messages = append(messages, configScreenMessages(child)...)
		}
		return messages
	}
	return []tea.Msg{message}
}

func configScreenKey(name string) tea.KeyPressMsg {
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
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	default:
		return tea.KeyPressMsg{}
	}
}

func ftueCleanupIndex(days int) int {
	switch days {
	case 30:
		return 0
	case 90:
		return 1
	case 365:
		return 2
	default:
		return 3
	}
}

func configScreenLicense(t *testing.T, path string) config.License {
	t.Helper()
	return configScreenConfig(t, path).Push.License
}

func configScreenConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	data := mustReadConfigScreenFile(t, path)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return cfg
}

func mustReadConfigScreenFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertActionableConfigError(t *testing.T, err error) {
	t.Helper()
	for _, part := range []string{"what:", "why:", "where:", "when:", "means:", "fix:"} {
		if !strings.Contains(err.Error(), part) {
			t.Fatalf("error missing %q: %v", part, err)
		}
	}
}
