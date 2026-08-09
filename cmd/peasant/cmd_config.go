package main

import (
	"context"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// configDiscovery is the read-only data needed to build the canonical settings
// registry. Keeping it separate from the command model makes the command's
// authority explicit: discovery supplies display data and nothing executable.
type configDiscovery struct {
	inventory ftue.ProviderInventory
	source    kit.TreeSource
}

// configRetentionFile binds the initial retention value, later writer, and
// reported path to one strictly opened Claude settings document.
type configRetentionFile interface {
	Path() string
	CleanupDays() (int, bool)
	WriteCleanupDays(int) error
}

var _ configRetentionFile = (*ftue.ClaudeSettingsFile)(nil)

// configCommandDeps contains only external boundaries used by the config
// command. The production builder wires real discovery, Claude retention I/O,
// and Bubble Tea; tests replace those boundaries while retaining the real
// command, Screen, Registry, fields, and Draft.
type configCommandDeps struct {
	discover      func(context.Context, string, string) configDiscovery
	openRetention func() (configRetentionFile, error)
	run           func(tea.Model) (tea.Model, error)
}

func defaultConfigCommandDeps() configCommandDeps {
	return configCommandDeps{
		discover: func(ctx context.Context, configPath, dbPath string) configDiscovery {
			spinner := newDiscoverySpinner(os.Stderr)
			inventory, sessions := ftueDiscover(ctx, configPath, dbPath, spinner)
			spinner.Stop()
			return configDiscovery{
				inventory: inventory,
				source:    kickstart.NewScannerTreeSource(sessions),
			}
		},
		openRetention: func() (configRetentionFile, error) {
			return ftue.OpenClaudeSettings()
		},
		run: func(model tea.Model) (tea.Model, error) {
			return tea.NewProgram(model).Run()
		},
	}
}

// BuildConfigCommand builds the dense settings editor. The settings alias is a
// Cobra alias on this same command, so both names mount one production path.
func BuildConfigCommand() *cobra.Command {
	return buildConfigCommand(defaultConfigCommandDeps())
}

func buildConfigCommand(deps configCommandDeps) *cobra.Command {
	return &cobra.Command{
		Use:     "config",
		Aliases: []string{"settings"},
		Short:   "Edit peasant configuration",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateConfigCommandDeps(deps); err != nil {
				return err
			}

			configPath := resolveConfigPath(cmd)
			loaded, err := loadConfig(configPath)
			if err != nil {
				return fmt.Errorf(
					"open config editor for %q: %w.\n"+
						"what: the configuration could not be loaded.\n"+
						"why: the file is unreadable, malformed, or contains an unsupported value.\n"+
						"where: peasant config load step.\n"+
						"when: before opening the interactive settings screen.\n"+
						"means: no settings were changed and no file was written.\n"+
						"fix: correct or restore the named config file, then run peasant config again.",
					configPath, err)
			}

			draft, err := settings.NewDraft(configPath, loaded)
			if err != nil {
				return fmt.Errorf(
					"open config editor draft for %q: %w.\n"+
						"what: buffered settings state could not be created.\n"+
						"why: the loaded config could not be copied or its disk snapshot could not be read.\n"+
						"where: peasant config draft step.\n"+
						"when: before mounting any settings fields.\n"+
						"means: no settings were changed and no file was written.\n"+
						"fix: repair the reported path or config value, then run peasant config again.",
					configPath, err)
			}

			retentionFile, err := deps.openRetention()
			if err != nil {
				return configRetentionOpenError(err)
			}
			if retentionFile == nil || retentionFile.Path() == "" {
				return configRetentionOpenError(fmt.Errorf("opened retention file is nil or has an empty path"))
			}
			retentionPath := retentionFile.Path()
			retentionDays, found := retentionFile.CleanupDays()
			if found && retentionDays <= 0 {
				return configRetentionOpenError(fmt.Errorf("cleanupPeriodDays=%d at %q is not a positive integer", retentionDays, retentionPath))
			}
			if !found {
				retentionDays = kickstart.RecommendedRetentionDays
			}
			if err := kickstart.SeedRetentionInitial(draft, retentionDays); err != nil {
				return err
			}

			discovery := deps.discover(
				cmd.Context(),
				configPath,
				string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd))),
			)
			if discovery.source == nil {
				return fmt.Errorf(
					"open config editor for %q: discovery returned no transcript source.\n"+
						"what: the canonical selection field has no read-only data source.\n"+
						"why: config discovery did not return a TreeSource implementation.\n"+
						"where: peasant config discovery step.\n"+
						"when: before building the canonical settings registry.\n"+
						"means: the screen was not mounted and no file was written.\n"+
						"fix: retry after transcript discovery is available; if this repeats, report the command wiring defect.",
					configPath)
			}

			registry := kickstart.BuildRegistry(kickstart.Options{
				Source:                discovery.source,
				VillageConnected:      loaded.Village.Connected,
				ClaudeSessionsPresent: claudeSessionsPresent(discovery.inventory),
			})
			screen := settings.NewScreen(theme.New(themeModeFor(loaded)), registry, draft)
			if err := screen.Err(); err != nil {
				return err
			}
			model := &configScreenModel{
				screen:     screen,
				draft:      draft,
				retention:  retentionFile,
				configPath: configPath,
			}
			final, err := deps.run(model)
			if err != nil {
				return fmt.Errorf(
					"run config editor for %q: %w.\n"+
						"what: the interactive terminal session stopped unexpectedly.\n"+
						"why: Bubble Tea could not complete the mounted Screen session.\n"+
						"where: peasant config terminal runner.\n"+
						"when: while editing buffered settings.\n"+
						"means: only a save confirmed before this terminal error could have changed a file.\n"+
						"fix: inspect both named files, repair the terminal error, and retry peasant config.",
					configPath, err)
			}
			if returned, ok := final.(*configScreenModel); ok {
				model = returned
			}
			if model.err != nil {
				return model.err
			}
			if !model.saved {
				return nil
			}
			if model.retentionWritten {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "saved settings to %s and Claude retention to %s\n", configPath, retentionPath)
				return nil
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "saved settings to %s\n", configPath)
			return nil
		},
	}
}

// configScreenModel adapts the concrete Screen update contract to tea.Model and
// owns the one responsibility-specific effect that follows a successful save.
type configScreenModel struct {
	screen    settings.Screen
	draft     *settings.Draft
	retention configRetentionFile

	configPath string

	saved            bool
	retentionWritten bool
	err              error
}

func (m *configScreenModel) Init() tea.Cmd { return m.screen.Init() }

func (m *configScreenModel) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	if saved, ok := message.(settings.SavedMsg); ok {
		if m.saved {
			return m, tea.Quit
		}
		if saved.Draft() == nil || saved.Draft() != m.draft {
			m.err = fmt.Errorf(
				"finish config save for %q: SavedMsg contains an unexpected draft.\n"+
					"what: the post-commit handler cannot identify the draft that was saved.\n"+
					"why: the mounted Screen emitted a nil or different Draft pointer.\n"+
					"where: peasant config SavedMsg handler.\n"+
					"when: immediately after the Screen reported a successful config commit.\n"+
					"means: no retention write was attempted; inspect the config file before retrying.\n"+
					"fix: keep one Screen and Draft mounted together and report this internal wiring defect.",
				m.configPath)
			return m, tea.Quit
		}
		m.saved = true
		if !kickstart.RetentionChanged(m.draft) {
			return m, tea.Quit
		}
		days := m.draft.Working().ClaudeRetentionDays
		if err := m.retention.WriteCleanupDays(days); err != nil {
			m.err = configPartialSuccessError(m.configPath, m.retention.Path(), err)
			return m, tea.Quit
		}
		m.retentionWritten = true
		return m, tea.Quit
	}

	var cmd tea.Cmd
	m.screen, cmd = m.screen.Update(message)
	return m, cmd
}

func (m *configScreenModel) View() tea.View {
	view := tea.NewView(m.screen.View())
	view.AltScreen = true
	return view
}

func configPartialSuccessError(configPath, retentionPath string, cause error) error {
	return fmt.Errorf(
		"save config settings partially: update Claude retention at %q after committing %q: %w.\n"+
			"what: peasant configuration was saved, but Claude transcript retention was not updated.\n"+
			"why: the path-bound retention file could not atomically merge the selected cleanup period into Claude settings.\n"+
			"where: peasant config post-commit retention step for %q.\n"+
			"when: after Draft.Commit successfully replaced %q.\n"+
			"means: config %q remains committed while retention at %q did not change; the two files now reflect a partial save.\n"+
			"fix: repair the Claude settings path or permissions, then run peasant config again and reselect the retention value.",
		retentionPath, configPath, cause, retentionPath, configPath, configPath, retentionPath)
}

func configRetentionOpenError(cause error) error {
	return fmt.Errorf(
		"open Claude transcript retention while opening config editor: %w.\n"+
			"what: the Claude settings document could not be opened for one path-bound read and write.\n"+
			"why: its path is unavailable, the file is unreadable or malformed, the top level is not an object, or cleanupPeriodDays is not a positive integer.\n"+
			"where: peasant config retention setup.\n"+
			"when: after opening the config draft and before mounting any interactive field.\n"+
			"means: no settings were changed and neither config nor Claude settings was written.\n"+
			"fix: repair or remove the named Claude settings document, ensure the home path is accessible, then run peasant config again.",
		cause)
}

func validateConfigCommandDeps(deps configCommandDeps) error {
	if deps.discover == nil || deps.openRetention == nil || deps.run == nil {
		return fmt.Errorf(
			"build config command: one or more required dependencies are nil.\n" +
				"what: the config editor is missing discovery, retention I/O, or its terminal runner.\n" +
				"why: configCommandDeps was assembled incompletely.\n" +
				"where: buildConfigCommand dependency validation.\n" +
				"when: before loading configuration or mounting the Screen.\n" +
				"means: no settings were changed and no file was written.\n" +
				"fix: construct the command through BuildConfigCommand or provide every boundary dependency.")
	}
	return nil
}

var _ tea.Model = (*configScreenModel)(nil)
