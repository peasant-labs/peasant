package main

import (
	"context"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/annotations"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/mock"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// BuildTUICommand constructs the fully wired tui cobra command.
func BuildTUICommand() *cobra.Command {
	var mockDataStore string

	tuiCmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the terminal UI (deprecated)",
		Long: "Launch the terminal UI.\n\n" +
			"This command is deprecated. Use `peasant web` for the dashboard, sessions, and trends. " +
			"Use `peasant annotate` for annotations. The command still runs. " +
			"It will be removed after one release carries this notice.",
		Deprecated: "use `peasant web` for the dashboard, sessions, and trends, and `peasant annotate` for annotations. " +
			"The command still runs. It will be removed after one release carries this notice.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath := resolveConfigPath(cmd)
			return runTUI(cmd, cfgPath, mockDataStore)
		},
	}
	tuiCmd.Flags().StringVar(&mockDataStore, "mock-data-store", "", "Use mock data store (comma-separated: web,tui,api or dashboard,sessions,trends,metrics,qualitySessions)")

	return withCleanTerminalError(tuiCmd)
}

// runTUI launches the terminal UI with the appropriate data provider.
func runTUI(cmd *cobra.Command, cfgPath string, mockDataStore string) error {
	ctx := context.Background()

	// Load config
	cfg, err := config.LoadForBrowsing(cfgPath, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{})
	if err != nil {
		return fmt.Errorf("tui startup: load config %q: %w; the TUI stopped before opening stored history; fix the reported configuration field and retry", cfgPath, err)
	}
	// Apply --data-dir once, right after loading, like every runtime loader.
	applyDataDirOverride(cmd, cfg)
	visibility := sessionvisibility.New(cfg.Selection)

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
	var db *store.Store
	dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	// A fully mocked TUI needs no database, so an open failure is kept and
	// reported only if a section then needs the real store.
	var openErr error
	// The TUI only runs on a terminal, so require it before touching the
	// store: a non-interactive invocation must fail here instead of
	// migrating (and rebuilding) the database invisibly on the way to the
	// same refusal below.
	if !tuiTerminalAvailable() {
		return noTerminalError{command: "tui"}
	}
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		openErr = fmt.Errorf("create the data directory %s: %w", dataDir, err)
	} else if opened, err := store.Open(dbPath); err != nil {
		openErr = fmt.Errorf("open the analytics database %s: %w", dbPath, err)
	} else {
		db = opened
		defer db.Close()
		realProvider = api.NewStoreDataProvider(db, visibility, cfg.Output.BasePath)
	}

	// Create progressive provider
	provider := api.NewProgressiveProvider(cfg, defaults.MockComponents.TUI, mock.NewProvider(), realProvider)

	// Fetch sessions (TUI current takes a static list)
	sessions, err := provider.Sessions(ctx)
	if err != nil {
		cmd.SilenceUsage = true
		if openErr != nil {
			return fmt.Errorf("tui startup: %w; check the file's permissions, or move it aside and run `peasant harvest` to rebuild it from your transcripts; a rebuild does not bring back local-only data such as annotations and the session selection, so keep the moved file", openErr)
		}
		return fmt.Errorf("failed to fetch sessions: %w", err)
	}

	app, err := newTUIApp(ctx, db, sessions)
	if err != nil {
		cmd.SilenceUsage = true
		return err
	}
	p := tea.NewProgram(app)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("failed to run TUI: %w", err)
	}
	return nil
}

// newTUIApp builds the TUI model. With an open store, the session detail loads
// each session's turns from it, the annotation picker lists the registry's
// types, and pending annotations are kept in it and committed to the local web
// dashboard.
func newTUIApp(ctx context.Context, db *store.Store, sessions []ingest.Session) (tui.AppModel, error) {
	app := tui.NewApp(sessions)
	if db == nil {
		return app, nil
	}
	types, err := annotations.NewTypeReader(db).ListTypes(ctx, schema.TypeFilter{})
	if err != nil {
		return app, fmt.Errorf("tui startup: read the annotation types from the analytics database: %w", err)
	}
	return app.WithAnnotationStore(db, dashboardBaseURL(defaults.DefaultPort), types), nil
}
