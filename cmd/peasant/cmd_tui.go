package main

import (
	"context"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/mock"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui"
	"github.com/spf13/cobra"
)

// BuildTUICommand constructs the fully wired tui cobra command.
func BuildTUICommand() *cobra.Command {
	var mockDataStore string

	tuiCmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the terminal UI",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := resolveConfigPath(cmd)
			return runTUI(cmd, cfgPath, mockDataStore)
		},
	}
	tuiCmd.Flags().StringVar(&mockDataStore, "mock-data-store", "", "Use mock data store (comma-separated: web,tui,api or dashboard,sessions,trends,metrics,qualitySessions)")

	return tuiCmd
}

// runTUI launches the terminal UI with the appropriate data provider.
func runTUI(cmd *cobra.Command, cfgPath string, mockDataStore string) error {
	ctx := context.Background()

	// Load config
	cfg, err := config.Load(cfgPath, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{})
	if err != nil {
		return fmt.Errorf("tui startup: load config %q: %w; the TUI stopped before exposing discovery with default selection, so fix config.yaml or run `peasant kickstart` and retry", cfgPath, err)
	}
	visibility, err := sessionvisibility.New(cfg.Selection)
	if err != nil {
		return fmt.Errorf("tui startup: selection policy: %w", err)
	}

	// Apply CLI overrides
	if mockDataStore != "" {
		opts, err := ParseMockDataStore(mockDataStore)
		if err != nil {
			return fmt.Errorf("invalid --mock-data-store: %w", err)
		}
		cfg.ApplyMockOverrides(opts, defaults.MockComponents.TUI)
	}

	// Open real store
	var realProvider api.DataProvider
	dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err == nil {
		if db, err := store.Open(dbPath); err == nil {
			defer db.Close()
			realProvider = api.NewStoreDataProvider(db, visibility)
		}
	}

	// Create progressive provider
	provider := api.NewProgressiveProvider(cfg, defaults.MockComponents.TUI, mock.NewProvider(), realProvider)

	// Fetch sessions (TUI current takes a static list)
	sessions, err := provider.Sessions(ctx)
	if err != nil {
		return fmt.Errorf("failed to fetch sessions: %w", err)
	}

	p := tea.NewProgram(tui.NewApp(sessions))
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("failed to run TUI: %w", err)
	}
	return nil
}
