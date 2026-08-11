package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
)

const (
	expectedSelectionCommandCaseCount          = 1
	expectedSelectionCommandSessionCount       = 2
	expectedSelectionCommandMutationProbeCount = 1
	expectedSelectionCommandRenderCaseCount    = 8
)

//go:embed testdata/kickstart_selection_command.yaml
var selectionCommandData []byte

type selectionCommandAction string

const (
	selectionCommandSearch     selectionCommandAction = "search"
	selectionCommandKeepFilter selectionCommandAction = "keep-filter"
	selectionCommandClear      selectionCommandAction = "clear-filter"
	selectionCommandDown       selectionCommandAction = "down"
	selectionCommandToggle     selectionCommandAction = "toggle"
)

func (a selectionCommandAction) valid() bool {
	switch a {
	case selectionCommandSearch, selectionCommandKeepFilter,
		selectionCommandClear, selectionCommandDown, selectionCommandToggle:
		return true
	default:
		return false
	}
}

type selectionCommandInput struct {
	Action selectionCommandAction `yaml:"action"`
	Text   string                 `yaml:"text"`
}

type selectionCommandRowAssertion struct {
	Label        string   `yaml:"label"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

type selectionCommandCase struct {
	Name                  string                         `yaml:"name"`
	MutationProbe         bool                           `yaml:"mutationProbe"`
	Listings              []ftue.SessionListing          `yaml:"listings"`
	InitialSelection      config.SelectionConfig         `yaml:"initialSelection"`
	ExpectedSelection     config.SelectionConfig         `yaml:"expectedSelection"`
	SelectionInputs       []selectionCommandInput        `yaml:"selectionInputs"`
	BeforeConsentContains []string                       `yaml:"beforeConsentContains"`
	RowAssertions         []selectionCommandRowAssertion `yaml:"rowAssertions"`
	ParityRowAssertions   []selectionCommandRowAssertion `yaml:"parityRowAssertions"`
}

type selectionCommandSurface string

const (
	selectionCommandSurfaceConfig    selectionCommandSurface = "config"
	selectionCommandSurfaceKickstart selectionCommandSurface = "kickstart"
)

func (s selectionCommandSurface) valid() bool {
	return s == selectionCommandSurfaceConfig || s == selectionCommandSurfaceKickstart
}

func allSelectionCommandSurfaces() []selectionCommandSurface {
	return []selectionCommandSurface{selectionCommandSurfaceConfig, selectionCommandSurfaceKickstart}
}

type selectionCommandLayout string

const (
	selectionCommandLayoutNarrow selectionCommandLayout = "narrow"
	selectionCommandLayoutWide   selectionCommandLayout = "wide"
)

func (l selectionCommandLayout) valid() bool {
	return l == selectionCommandLayoutNarrow || l == selectionCommandLayoutWide
}

func (l selectionCommandLayout) dimensions() (int, int) {
	switch l {
	case selectionCommandLayoutNarrow:
		return 80, 24
	case selectionCommandLayoutWide:
		return 120, 40
	default:
		return 0, 0
	}
}

func allSelectionCommandLayouts() []selectionCommandLayout {
	return []selectionCommandLayout{selectionCommandLayoutNarrow, selectionCommandLayoutWide}
}

func allSelectionCommandThemes() []config.Theme {
	return []config.Theme{config.ThemeDark, config.ThemeLight}
}

type selectionCommandRenderCase struct {
	Name    string                  `yaml:"name"`
	Surface selectionCommandSurface `yaml:"surface"`
	Theme   config.Theme            `yaml:"theme"`
	Layout  selectionCommandLayout  `yaml:"layout"`
	Width   int                     `yaml:"width"`
	Height  int                     `yaml:"height"`
}

type selectionCommandDocument struct {
	ExpectedCaseCount          int                          `yaml:"expectedCaseCount"`
	ExpectedSessionCount       int                          `yaml:"expectedSessionCount"`
	ExpectedMutationProbeCount int                          `yaml:"expectedMutationProbeCount"`
	ExpectedRenderCaseCount    int                          `yaml:"expectedRenderCaseCount"`
	Cases                      []selectionCommandCase       `yaml:"cases"`
	RenderCases                []selectionCommandRenderCase `yaml:"renderCases"`
}

func selectionCommandValuesPresent(values ...[]string) bool {
	for _, group := range values {
		for _, value := range group {
			if strings.TrimSpace(value) == "" {
				return false
			}
		}
	}
	return true
}

func decodeSelectionCommand(data []byte) (selectionCommandDocument, error) {
	var doc selectionCommandDocument
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/kickstart_selection_command.yaml: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("kickstart_selection_command.yaml must hold exactly one document: %w", err)
	}
	if doc.ExpectedCaseCount != expectedSelectionCommandCaseCount || len(doc.Cases) != expectedSelectionCommandCaseCount {
		return doc, fmt.Errorf("kickstart selection command cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedSelectionCommandCaseCount)
	}
	if doc.ExpectedRenderCaseCount != expectedSelectionCommandRenderCaseCount || len(doc.RenderCases) != expectedSelectionCommandRenderCaseCount {
		return doc, fmt.Errorf("selection command render cases: declared=%d actual=%d required=%d",
			doc.ExpectedRenderCaseCount, len(doc.RenderCases), expectedSelectionCommandRenderCaseCount)
	}
	seenCases := map[string]bool{}
	seenSessions := map[string]bool{}
	mutationProbes := 0
	sessions := 0
	for _, c := range doc.Cases {
		if c.Name == "" || seenCases[c.Name] || !c.InitialSelection.Mode.IsValid() || !c.ExpectedSelection.Mode.IsValid() {
			return doc, fmt.Errorf("kickstart selection command fixture contains an invalid or duplicate case %q", c.Name)
		}
		seenCases[c.Name] = true
		if c.MutationProbe {
			mutationProbes++
		}
		if len(c.SelectionInputs) == 0 || len(c.BeforeConsentContains) == 0 || len(c.RowAssertions) == 0 || len(c.ParityRowAssertions) == 0 ||
			!selectionCommandValuesPresent(c.BeforeConsentContains) {
			return doc, fmt.Errorf("kickstart selection command case %q has no interaction, consent, or row assertion", c.Name)
		}
		for _, input := range c.SelectionInputs {
			if (input.Action == "") == (input.Text == "") || (input.Action != "" && !input.Action.valid()) || len([]rune(input.Text)) > 1 {
				return doc, fmt.Errorf("kickstart selection command case %q has invalid input %#v", c.Name, input)
			}
		}
		for _, row := range c.RowAssertions {
			if strings.TrimSpace(row.Label) == "" || len(row.WantContains)+len(row.WantMissing) == 0 ||
				!selectionCommandValuesPresent(row.WantContains, row.WantMissing) {
				return doc, fmt.Errorf("kickstart selection command case %q has an empty row assertion", c.Name)
			}
		}
		for _, row := range c.ParityRowAssertions {
			if strings.TrimSpace(row.Label) == "" || len(row.WantContains)+len(row.WantMissing) == 0 ||
				!selectionCommandValuesPresent(row.WantContains, row.WantMissing) {
				return doc, fmt.Errorf("kickstart selection command case %q has an empty parity row assertion", c.Name)
			}
		}
		for _, listing := range c.Listings {
			if !selectionCommandValuesPresent([]string{listing.SessionID, listing.Harness, listing.ProjectName, listing.GitRemote, listing.Branch, listing.Title}) ||
				seenSessions[listing.SessionID] {
				return doc, fmt.Errorf("kickstart selection command fixture contains an invalid or duplicate session %q", listing.SessionID)
			}
			if _, err := ingest.NewSessionID(listing.SessionID); err != nil {
				return doc, fmt.Errorf("kickstart selection command fixture session %q is invalid: %w", listing.SessionID, err)
			}
			seenSessions[listing.SessionID] = true
			sessions++
		}
	}
	if doc.ExpectedSessionCount != expectedSelectionCommandSessionCount || sessions != expectedSelectionCommandSessionCount {
		return doc, fmt.Errorf("kickstart selection command sessions: declared=%d actual=%d required=%d",
			doc.ExpectedSessionCount, sessions, expectedSelectionCommandSessionCount)
	}
	if doc.ExpectedMutationProbeCount != expectedSelectionCommandMutationProbeCount || mutationProbes != expectedSelectionCommandMutationProbeCount {
		return doc, fmt.Errorf("kickstart selection command mutation probes: declared=%d actual=%d required=%d",
			doc.ExpectedMutationProbeCount, mutationProbes, expectedSelectionCommandMutationProbeCount)
	}
	renderNames := map[string]bool{}
	renderCombinations := map[string]int{}
	for _, renderCase := range doc.RenderCases {
		expectedWidth, expectedHeight := renderCase.Layout.dimensions()
		if strings.TrimSpace(renderCase.Name) == "" || renderNames[renderCase.Name] || !renderCase.Surface.valid() ||
			!renderCase.Theme.IsValid() || !renderCase.Layout.valid() || renderCase.Width != expectedWidth || renderCase.Height != expectedHeight {
			return doc, fmt.Errorf("kickstart selection command fixture contains an invalid or duplicate render case %#v", renderCase)
		}
		renderNames[renderCase.Name] = true
		combination := fmt.Sprintf("%s/%s/%s", renderCase.Surface, renderCase.Theme, renderCase.Layout)
		renderCombinations[combination]++
	}
	for _, surface := range allSelectionCommandSurfaces() {
		for _, commandTheme := range allSelectionCommandThemes() {
			for _, layout := range allSelectionCommandLayouts() {
				combination := fmt.Sprintf("%s/%s/%s", surface, commandTheme, layout)
				if renderCombinations[combination] != 1 {
					return doc, fmt.Errorf("kickstart selection command fixture has %d render cases for %s, want exactly one",
						renderCombinations[combination], combination)
				}
			}
		}
	}
	return doc, nil
}

func loadSelectionCommand(t *testing.T) selectionCommandDocument {
	t.Helper()
	doc, err := decodeSelectionCommand(selectionCommandData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func selectionCommandMessage(t *testing.T, input selectionCommandInput) tea.KeyPressMsg {
	t.Helper()
	if input.Text != "" {
		r := []rune(input.Text)[0]
		return tea.KeyPressMsg{Code: r, Text: input.Text}
	}
	switch input.Action {
	case selectionCommandSearch:
		return tea.KeyPressMsg{Code: '/'}
	case selectionCommandKeepFilter:
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case selectionCommandClear:
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case selectionCommandDown:
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case selectionCommandToggle:
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	default:
		t.Fatalf("unsupported selection command action %q", input.Action)
		return tea.KeyPressMsg{}
	}
}

func drainSelectionCommandModel(t *testing.T, model tea.Model, commands ...tea.Cmd) tea.Model {
	t.Helper()
	queue := append([]tea.Cmd(nil), commands...)
	const maximumCommands = 512
	for steps := 0; len(queue) > 0 && steps < maximumCommands; steps++ {
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		message := cmd()
		if message == nil {
			continue
		}
		if batch, ok := message.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		var next tea.Cmd
		model, next = model.Update(message)
		if next != nil {
			queue = append(queue, next)
		}
	}
	if len(queue) > 0 {
		t.Fatalf("selection command model did not settle within %d commands", maximumCommands)
	}
	return model
}

func updateSelectionCommandModel(t *testing.T, model tea.Model, message tea.Msg) tea.Model {
	t.Helper()
	next, cmd := model.Update(message)
	return drainSelectionCommandModel(t, next, cmd)
}

func selectionCommandView(model tea.Model) string {
	return ansiPattern.ReplaceAllString(model.View().Content, "")
}

func selectionCommandLine(view, label string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, label) {
			return line
		}
	}
	return ""
}

func assertSelectionCommandRows(t *testing.T, view string, rows []selectionCommandRowAssertion) {
	t.Helper()
	for _, row := range rows {
		line := selectionCommandLine(view, row.Label)
		if line == "" {
			t.Errorf("mounted selection row %q is absent:\n%s", row.Label, view)
			continue
		}
		for _, want := range row.WantContains {
			if !strings.Contains(line, want) {
				t.Errorf("mounted selection row %q must contain %q: %s", row.Label, want, line)
			}
		}
		for _, missing := range row.WantMissing {
			if strings.Contains(line, missing) {
				t.Errorf("mounted selection row %q must not contain %q: %s", row.Label, missing, line)
			}
		}
	}
}

func assertSelectionParityView(t *testing.T, surface, view string, rows []selectionCommandRowAssertion) {
	t.Helper()
	if view == "" {
		t.Fatalf("%s selection parity mount produced no view", surface)
	}
	assertMountedSelectionSearch(t, view)
	assertSelectionCommandRows(t, view, rows)
}

func selectionKickstartDeps(t *testing.T, c selectionCommandCase, runModel func(tea.Model) error) kickstartCommandDeps {
	t.Helper()
	deps := defaultKickstartCommandDeps()
	deps.discover = func(context.Context, string, string, *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing) {
		return ftue.ProviderInventory{}, c.Listings
	}
	deps.existingUser = func(string) string { return "" }
	deps.readRetention = func() (int, bool) { return 0, false }
	deps.alreadyConnected = func(string) bool { return false }
	deps.localIngest = func(*cobra.Command, string, []ftue.SessionListing) (kickstart.IngestFunc, kickstart.ProgressSource) {
		progress := ingest.NewProgressState()
		return func(context.Context) (*ftue.IngestResult, error) {
			return &ftue.IngestResult{}, nil
		}, progress
	}
	deps.runModel = runModel
	return deps
}

func seedSelectionCommandConfig(t *testing.T, dir string, selection config.SelectionConfig) (string, []byte) {
	t.Helper()
	path := defaults.ResolveConfigFilePathWith(dir).String()
	cfg := config.BaseConfig()
	cfg.Selection = selection
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed kickstart selection config: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read kickstart selection config: %v", err)
	}
	return path, data
}

func seedSelectionCommandRenderConfig(
	t *testing.T,
	dir string,
	selection config.SelectionConfig,
	commandTheme config.Theme,
) (string, []byte) {
	t.Helper()
	path := defaults.ResolveConfigFilePathWith(dir).String()
	cfg := config.BaseConfig()
	cfg.Selection = selection
	cfg.Display.Theme = commandTheme
	if err := config.SaveAtomic(path, cfg); err != nil {
		t.Fatalf("seed %s selection command render config: %v", commandTheme, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s selection command render config: %v", commandTheme, err)
	}
	return path, data
}

func driveSelectionCommandInputs(t *testing.T, model tea.Model, inputs []selectionCommandInput) tea.Model {
	t.Helper()
	for _, input := range inputs {
		model = updateSelectionCommandModel(t, model, selectionCommandMessage(t, input))
	}
	return model
}

func advanceSelectionCommandToConsent(t *testing.T, model tea.Model) tea.Model {
	t.Helper()
	const maximumSteps = 16
	for step := 0; step < maximumSteps; step++ {
		if strings.Contains(selectionCommandView(model), "review your changes") {
			return model
		}
		mounted, ok := model.(kickstart.Model)
		if !ok {
			t.Fatalf("selection command mounted %T, want kickstart.Model", model)
		}
		key := tea.KeyPressMsg{Code: tea.KeyTab}
		if mounted.Program().Phase() == kickstart.PhaseVisibility {
			key = tea.KeyPressMsg{Code: tea.KeyEnter}
		}
		model = updateSelectionCommandModel(t, model, key)
	}
	t.Fatalf("selection command did not reach consent within %d steps:\n%s", maximumSteps, selectionCommandView(model))
	return model
}

func TestKickstartCommandMountsNonEmptySelectionInteraction(t *testing.T) {
	doc := loadSelectionCommand(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			configPath, before := seedSelectionCommandConfig(t, dir, c.InitialSelection)
			terminalCalls := 0
			deps := selectionKickstartDeps(t, c, func(model tea.Model) error {
				terminalCalls++
				if _, ok := model.(kickstart.Model); !ok {
					t.Fatalf("selection command mounted %T, want kickstart.Model", model)
				}
				model = updateSelectionCommandModel(t, model, tea.WindowSizeMsg{Width: 132, Height: 30})
				model = updateSelectionCommandModel(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
				if mounted := model.(kickstart.Model).Program(); mounted.Phase() != kickstart.PhaseFlow {
					t.Fatalf("selection command phase = %s after declining connection, want flow", mounted.Phase())
				}
				initialView := selectionCommandView(model)
				assertMountedSelectionSearch(t, initialView)
				for _, listing := range c.Listings {
					if !strings.Contains(initialView, listing.Title) {
						t.Fatalf("non-empty command mount omitted discovered session %q:\n%s", listing.Title, initialView)
					}
				}

				model = driveSelectionCommandInputs(t, model, c.SelectionInputs)
				selectionView := selectionCommandView(model)
				assertMountedSelectionSearch(t, selectionView)
				assertSelectionCommandRows(t, selectionView, c.RowAssertions)
				if after, err := os.ReadFile(configPath); err != nil || !bytes.Equal(after, before) {
					t.Fatalf("buffered selection changed config before consent: readErr=%v", err)
				}

				model = advanceSelectionCommandToConsent(t, model)
				consent := selectionCommandView(model)
				for _, want := range c.BeforeConsentContains {
					if !strings.Contains(consent, want) {
						t.Errorf("selection consent must contain %q:\n%s", want, consent)
					}
				}
				if after, err := os.ReadFile(configPath); err != nil || !bytes.Equal(after, before) {
					t.Fatalf("selection consent changed config before confirmation: readErr=%v", err)
				}

				model = updateSelectionCommandModel(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
				if !model.(kickstart.Model).Program().Committed() {
					t.Fatal("selection command did not commit after explicit consent")
				}
				persistedBytes, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatalf("read committed selection config: %v", err)
				}
				persisted, err := config.Parse(persistedBytes)
				if err != nil {
					t.Fatalf("parse committed selection config: %v", err)
				}
				if !reflect.DeepEqual(persisted.Selection, c.ExpectedSelection) {
					t.Fatalf("committed selection = %#v, want %#v", persisted.Selection, c.ExpectedSelection)
				}
				return nil
			})

			if _, err := executeWithDataDir(t, buildKickstartCommand(deps), dir, nil); err != nil {
				t.Fatalf("run mounted kickstart selection command: %v", err)
			}
			if terminalCalls != 1 {
				t.Fatalf("selection command terminal calls = %d, want 1", terminalCalls)
			}
		})
	}
}

func TestConfigCommandMountsGlobalSearchToggleAndSave(t *testing.T) {
	t.Parallel()

	doc := loadSelectionCommand(t)
	for _, c := range doc.Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			configPath, before := seedSelectionCommandConfig(t, dir, c.InitialSelection)
			terminalCalls := 0
			deps := defaultConfigCommandDeps()
			deps.discover = func(context.Context, string, string) configDiscovery {
				return configDiscovery{source: kickstart.NewScannerTreeSource(c.Listings)}
			}
			deps.openRetention = func() (configRetentionFile, error) {
				return selectionCommandRetentionFile{path: filepath.Join(dir, "claude-settings.json")}, nil
			}
			deps.run = func(model tea.Model) (tea.Model, error) {
				terminalCalls++
				model = drainSelectionCommandModel(t, model, model.Init())
				model = updateSelectionCommandModel(t, model, tea.WindowSizeMsg{Width: 120, Height: 40})
				model = updateSelectionCommandModel(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
				model = driveSelectionCommandInputs(t, model, c.SelectionInputs)
				view := selectionCommandView(model)
				assertMountedSelectionSearch(t, view)
				assertSelectionCommandRows(t, view, c.RowAssertions)
				if after, err := os.ReadFile(configPath); err != nil || !bytes.Equal(after, before) {
					t.Fatalf("buffered config selection changed bytes before save: readErr=%v", err)
				}
				return updateSelectionCommandModel(t, model, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}), nil
			}

			if _, err := executeWithDataDir(t, buildConfigCommand(deps), dir, nil); err != nil {
				t.Fatalf("run mounted config selection command: %v", err)
			}
			if terminalCalls != 1 {
				t.Fatalf("config selection terminal calls = %d, want 1", terminalCalls)
			}
			persistedBytes, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read saved config selection: %v", err)
			}
			persisted, err := config.Parse(persistedBytes)
			if err != nil {
				t.Fatalf("parse saved config selection: %v", err)
			}
			if !reflect.DeepEqual(persisted.Selection, c.ExpectedSelection) {
				t.Fatalf("saved config selection = %#v, want %#v", persisted.Selection, c.ExpectedSelection)
			}
		})
	}
}

type selectionCommandRetentionFile struct{ path string }

func (f selectionCommandRetentionFile) Path() string             { return f.path }
func (selectionCommandRetentionFile) CleanupDays() (int, bool)   { return 90, true }
func (selectionCommandRetentionFile) WriteCleanupDays(int) error { return nil }

var _ configRetentionFile = selectionCommandRetentionFile{}

func TestConfigAndKickstartShareCanonicalSelectionInitialization(t *testing.T) {
	for _, c := range loadSelectionCommand(t).Cases {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			_, before := seedSelectionCommandConfig(t, dir, c.ExpectedSelection)
			views := map[string]string{}

			configDeps := defaultConfigCommandDeps()
			configDeps.discover = func(context.Context, string, string) configDiscovery {
				return configDiscovery{source: kickstart.NewScannerTreeSource(c.Listings)}
			}
			configDeps.openRetention = func() (configRetentionFile, error) {
				return selectionCommandRetentionFile{path: filepath.Join(dir, "claude-settings.json")}, nil
			}
			configDeps.run = func(model tea.Model) (tea.Model, error) {
				model = drainSelectionCommandModel(t, model, model.Init())
				model = updateSelectionCommandModel(t, model, tea.WindowSizeMsg{Width: 132, Height: 30})
				views["config"] = selectionCommandView(model)
				return model, nil
			}
			if _, err := executeWithDataDir(t, buildConfigCommand(configDeps), dir, nil); err != nil {
				t.Fatalf("run config selection parity mount: %v", err)
			}

			kickstartDeps := selectionKickstartDeps(t, c, func(model tea.Model) error {
				model = updateSelectionCommandModel(t, model, tea.WindowSizeMsg{Width: 132, Height: 30})
				model = updateSelectionCommandModel(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
				views["kickstart"] = selectionCommandView(model)
				return nil
			})
			if _, err := executeWithDataDir(t, buildKickstartCommand(kickstartDeps), dir, nil); err != nil {
				t.Fatalf("run kickstart selection parity mount: %v", err)
			}

			assertSelectionParityView(t, "config", views["config"], c.ParityRowAssertions)
			assertSelectionParityView(t, "kickstart", views["kickstart"], c.ParityRowAssertions)
			after, err := os.ReadFile(defaults.ResolveConfigFilePathWith(dir).String())
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("selection parity mounts changed config bytes: readErr=%v", err)
			}
		})
	}
}

func assertSelectionCommandRender(t *testing.T, renderCase selectionCommandRenderCase, view string) {
	t.Helper()
	plain := ansiPattern.ReplaceAllString(view, "")
	assertMountedSelectionSearch(t, plain)
	for _, want := range []string{"choose sessions to import", "keep sess", "toggle target"} {
		if !strings.Contains(plain, want) {
			t.Errorf("%s mounted selection render omits %q:\n%s", renderCase.Surface, want, plain)
		}
	}
	lines := strings.Split(view, "\n")
	if len(lines) != renderCase.Height {
		t.Errorf("%s mounted selection render height = %d, want %d", renderCase.Surface, len(lines), renderCase.Height)
	}
	maximumWidth := 0
	for _, line := range lines {
		width := lipgloss.Width(line)
		if width > renderCase.Width {
			t.Errorf("%s mounted selection line width = %d, exceeds %d: %q", renderCase.Surface, width, renderCase.Width, line)
		}
		if width > maximumWidth {
			maximumWidth = width
		}
	}
	if maximumWidth != renderCase.Width {
		t.Errorf("%s mounted selection maximum width = %d, want %d", renderCase.Surface, maximumWidth, renderCase.Width)
	}
}

func TestSelectionCommands_RenderGolden(t *testing.T) {
	t.Parallel()

	doc := loadSelectionCommand(t)
	selectionCase := doc.Cases[0]
	for _, renderCase := range doc.RenderCases {
		renderCase := renderCase
		t.Run(renderCase.Name, func(t *testing.T) {
			dir := t.TempDir()
			_, before := seedSelectionCommandRenderConfig(t, dir, selectionCase.ExpectedSelection, renderCase.Theme)
			var rendered string
			terminalCalls := 0

			switch renderCase.Surface {
			case selectionCommandSurfaceConfig:
				deps := defaultConfigCommandDeps()
				deps.discover = func(context.Context, string, string) configDiscovery {
					return configDiscovery{
						inventory: ftue.ProviderInventory{
							defaults.HarnessClaudeCode: {SessionCount: len(selectionCase.Listings), Enabled: true},
						},
						source: kickstart.NewScannerTreeSource(selectionCase.Listings),
					}
				}
				deps.openRetention = func() (configRetentionFile, error) {
					return selectionCommandRetentionFile{path: filepath.Join(dir, "claude-settings.json")}, nil
				}
				deps.run = func(model tea.Model) (tea.Model, error) {
					terminalCalls++
					model = drainSelectionCommandModel(t, model, model.Init())
					model = updateSelectionCommandModel(t, model, tea.WindowSizeMsg{Width: renderCase.Width, Height: renderCase.Height})
					model = updateSelectionCommandModel(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
					rendered = model.View().Content
					return model, nil
				}
				if _, err := executeWithDataDir(t, buildConfigCommand(deps), dir, nil); err != nil {
					t.Fatalf("run mounted config selection render: %v", err)
				}

			case selectionCommandSurfaceKickstart:
				deps := selectionKickstartDeps(t, selectionCase, func(model tea.Model) error {
					terminalCalls++
					model = updateSelectionCommandModel(t, model, tea.WindowSizeMsg{Width: renderCase.Width, Height: renderCase.Height})
					model = updateSelectionCommandModel(t, model, tea.KeyPressMsg{Code: tea.KeyEnter})
					rendered = model.View().Content
					return nil
				})
				if _, err := executeWithDataDir(t, buildKickstartCommand(deps), dir, nil); err != nil {
					t.Fatalf("run mounted kickstart selection render: %v", err)
				}

			default:
				t.Fatalf("unsupported selection command render surface %q", renderCase.Surface)
			}

			if terminalCalls != 1 {
				t.Fatalf("%s selection render terminal calls = %d, want 1", renderCase.Surface, terminalCalls)
			}
			assertSelectionCommandRender(t, renderCase, rendered)
			golden.RequireEqual(t, []byte(rendered))
			after, err := os.ReadFile(defaults.ResolveConfigFilePathWith(dir).String())
			if err != nil || !bytes.Equal(after, before) {
				t.Fatalf("%s selection render changed config bytes: readErr=%v", renderCase.Surface, err)
			}
		})
	}
}

func TestSelectionCommandFixtureMutationProbeRequiresToggle(t *testing.T) {
	probes := 0
	for _, c := range loadSelectionCommand(t).Cases {
		if !c.MutationProbe {
			continue
		}
		probes++
		hasToggle := false
		for _, input := range c.SelectionInputs {
			if input.Action == selectionCommandToggle {
				hasToggle = true
			}
		}
		if !hasToggle || reflect.DeepEqual(c.InitialSelection, c.ExpectedSelection) {
			t.Fatalf("selection command mutation probe %q would not catch a dropped toggle", c.Name)
		}
	}
	if probes != expectedSelectionCommandMutationProbeCount {
		t.Fatalf("selection command mutation probes = %d, want %d", probes, expectedSelectionCommandMutationProbeCount)
	}
}

func mutateSelectionCommandCount(t *testing.T, field string, expected int) []byte {
	t.Helper()
	declared := []byte(fmt.Sprintf("%s: %d", field, expected))
	changed := []byte(fmt.Sprintf("%s: %d", field, expected+1))
	mutated := bytes.Replace(selectionCommandData, declared, changed, 1)
	if bytes.Equal(mutated, selectionCommandData) {
		t.Fatalf("selection command %s mutation did not alter the fixture", field)
	}
	return mutated
}

func TestSelectionCommandFixtureRejectsUnknownFields(t *testing.T) {
	mutated := append(append([]byte(nil), selectionCommandData...), []byte("\nunknownField: true\n")...)
	if _, err := decodeSelectionCommand(mutated); err == nil {
		t.Fatal("selection command fixture accepted an unknown field")
	}
}

func TestSelectionCommandFixtureRejectsTrailingDocuments(t *testing.T) {
	mutated := append(append([]byte(nil), selectionCommandData...), []byte("\n---\n{}\n")...)
	if _, err := decodeSelectionCommand(mutated); err == nil {
		t.Fatal("selection command fixture accepted a trailing document")
	}
}

func TestSelectionCommandFixturePinsCaseCount(t *testing.T) {
	mutated := mutateSelectionCommandCount(t, "expectedCaseCount", expectedSelectionCommandCaseCount)
	if _, err := decodeSelectionCommand(mutated); err == nil {
		t.Fatal("selection command fixture accepted a changed case count")
	}
}

func TestSelectionCommandFixturePinsSessionCount(t *testing.T) {
	mutated := mutateSelectionCommandCount(t, "expectedSessionCount", expectedSelectionCommandSessionCount)
	if _, err := decodeSelectionCommand(mutated); err == nil {
		t.Fatal("selection command fixture accepted a changed session count")
	}
}

func TestSelectionCommandFixturePinsMutationProbeCount(t *testing.T) {
	t.Parallel()

	mutated := mutateSelectionCommandCount(t, "expectedMutationProbeCount", expectedSelectionCommandMutationProbeCount)
	if _, err := decodeSelectionCommand(mutated); err == nil {
		t.Fatal("selection command fixture accepted a changed mutation-probe count")
	}
}

func TestSelectionCommandFixturePinsRenderCaseCount(t *testing.T) {
	t.Parallel()

	mutated := mutateSelectionCommandCount(t, "expectedRenderCaseCount", expectedSelectionCommandRenderCaseCount)
	if _, err := decodeSelectionCommand(mutated); err == nil {
		t.Fatal("selection command fixture accepted a changed render-case count")
	}
}

func TestSelectionCommandFixturePinsRenderMatrix(t *testing.T) {
	t.Parallel()

	mutated := bytes.Replace(selectionCommandData, []byte("surface: kickstart"), []byte("surface: config"), 1)
	if bytes.Equal(mutated, selectionCommandData) {
		t.Fatal("selection command render-matrix mutation did not alter the fixture")
	}
	if _, err := decodeSelectionCommand(mutated); err == nil {
		t.Fatal("selection command fixture accepted a duplicate config row and missing kickstart row")
	}
}
