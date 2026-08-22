package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

type kickstartCommandDeps struct {
	discover func(ctx context.Context, configPath, dbPath string, spinner *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing)
	getwd    func() (string, error)
	// pathResolver and repositoryResolver keep exact worktree identity separate
	// from transient Git repository topology at the command boundary. Production
	// uses physical/Git resolvers; mounted tests can supply deterministic values.
	pathResolver       ingest.PathIdentityResolver
	repositoryResolver ingest.RepositoryIdentityResolver
	// run is the retained legacy terminal boundary. Production selects runFlow;
	// tests for still-shipping legacy behavior deliberately omit runFlow.
	run func(ftue.WizardModel) error
	// runFlow is the guided orchestration selected by the production builder.
	// runModel is only its terminal execution boundary, so tests can inspect the
	// real mounted Program without replacing the production path decision.
	runFlow  func(*cobra.Command, kickstartCommandDeps, string, ftue.ProviderInventory, []ftue.SessionListing) error
	runModel func(tea.Model) error
	// readRetention is the external Claude settings read used before Flow mounts.
	readRetention func() (int, bool)
	existingUser  func(string) string
	// alreadyConnected reads the same isolated credential directory the command
	// was configured with; localIngest returns one runner and its exact
	// concurrent progress source so Program never observes a different attempt.
	alreadyConnected func(configDir string) bool
	localIngest      func(*cobra.Command, string, []ftue.SessionListing) (kickstart.IngestFunc, kickstart.ProgressSource)
	// flowIngest is a focused test seam for the post-save callback. Production
	// uses localIngest so the runner and progress source always share one attempt.
	flowIngest kickstart.IngestFunc
}

func defaultKickstartCommandDeps() kickstartCommandDeps {
	return kickstartCommandDeps{
		discover:           ftueDiscover,
		getwd:              os.Getwd,
		pathResolver:       ingest.NewPhysicalPathResolver(),
		repositoryResolver: ingest.NewGitRepositoryIdentityResolver(),
		run: func(model ftue.WizardModel) error {
			_, err := tea.NewProgram(model).Run()
			return err
		},
		runFlow: runKickstartFlow,
		runModel: func(model tea.Model) error {
			_, err := tea.NewProgram(model).Run()
			return err
		},
		readRetention: ftue.ReadClaudeCleanupDays,
		alreadyConnected: func(configDir string) bool {
			return villageAlreadyConnected(configDir)
		},
		localIngest: kickstartLocalIngest,
		existingUser: func(configDir string) string {
			if creds, err := auth.LoadCredentialsFrom(configDir); err == nil && creds != nil && creds.IsValid() {
				return creds.Username
			}
			return ""
		},
	}
}

func BuildKickstartCommand() *cobra.Command {
	return buildKickstartCommand(defaultKickstartCommandDeps())
}

func buildKickstartCommand(deps kickstartCommandDeps) *cobra.Command {
	var reset bool

	cmd := &cobra.Command{
		Use:     "kickstart",
		Aliases: []string{"ftue"},
		Short:   "Run the first-time setup wizard",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			configPath := resolveConfigPath(cmd)

			if reset {
				if err := resetAll(cmd, configPath); err != nil {
					return err
				}
			}

			// Check for existing credentials before entering the TUI alt-screen.
			existingUser := ""
			if deps.existingUser != nil {
				existingUser = deps.existingUser(configDirOverride(cmd))
			}

			// Load existing selection index from config for pre-population.
			var existingSelection *config.SelectionConfig
			var loadedConfig *config.Config
			var configSnapshot []byte
			configExisted := false
			if !reset {
				if snapshot, readErr := os.ReadFile(configPath); readErr == nil {
					configSnapshot = snapshot
					configExisted = true
				}
				if existingCfg, err := loadConfig(configPath); err == nil {
					loadedConfig = existingCfg
					if existingCfg.Selection.Mode != "" {
						existingSelection = &existingCfg.Selection
					}
				}
			}

			// Discover real transcript counts before entering the TUI alt-screen.
			// Non-fatal: empty counts are displayed if config is missing or paths are empty.
			// Show a progress spinner during discovery since git resolution can take several seconds.
			// A database this build can reuse turns that resolution into a lookup
			// for every session already recorded and unchanged since.
			spinner := newDiscoverySpinner(os.Stderr)
			dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
			inventory, sessions := deps.discover(ctx, configPath, dbPath, spinner)
			spinner.Stop()

			// Mount the rebuilt onboarding: the declarative settings.Flow rendered
			// on the kit, sequenced through optional login/visibility guidance,
			// local ingest, and persistent completion. This is the default
			// production entry point (deps.runFlow is wired). The legacy page-based
			// FTUE wizard is retained as a deprecation candidate and is reached only
			// when no flow runner is injected (its direct coverage drives
			// runLegacyFTUEWizard); its view layer is retired in a separate,
			// user-confirmed step.
			if deps.runFlow != nil {
				if err := deps.runFlow(cmd, deps, configPath, inventory, sessions); err != nil {
					return fmt.Errorf("setup flow failed: %w", err)
				}
				return nil
			}
			return runLegacyFTUEWizard(cmd, deps, ctx, configPath, existingUser,
				inventory, sessions, existingSelection, loadedConfig, configSnapshot, configExisted)
		},
	}

	cmd.Flags().BoolVar(&reset, "reset", false, "Remove config, credentials, database, ingested data, and state (full reset)")
	return cmd
}

// runLegacyFTUEWizard builds and runs the original page-based FTUE wizard.
//
// Deprecated: the mounted onboarding entry point is now runKickstartFlow (the
// settings.Flow rebuilt on the kit). This constructor is RETAINED as a
// deprecation candidate so the legacy wizard, its journey runner, and its
// still-shipping page code stay compiled and exercisable until the FTUE view
// layer is retired in a separate, user-confirmed step; it is not called on the
// default kickstart path. Do not delete without that confirmation.
func runLegacyFTUEWizard(
	cmd *cobra.Command,
	deps kickstartCommandDeps,
	ctx context.Context,
	configPath string,
	existingUser string,
	inventory ftue.ProviderInventory,
	sessions []ftue.SessionListing,
	existingSelection *config.SelectionConfig,
	loadedConfig *config.Config,
	configSnapshot []byte,
	configExisted bool,
) error {
	runner, progress := buildFTUEIngestRunner(cmd, configPath)
	invocationPWD, _ := deps.getwd()

	model := ftue.NewWizard(
		ftue.WithExistingUser(existingUser),
		ftue.WithProviderInventory(inventory),
		ftue.WithSessions(sessions),
		ftue.WithIngestRunner(runner),
		ftue.WithProgress(progress),
		ftue.WithExistingSelection(existingSelection),
		ftue.WithConfigPersistence(configPath, loadedConfig),
		ftue.WithConfigSnapshot(configSnapshot, configExisted),
		ftue.WithInvocationPWD(invocationPWD),
		ftue.WithJourneyRunner(buildKickstartJourneyRunner(cmd, configPath, loadedConfig, configSnapshot, configExisted, runner)),
		ftue.WithJourneyContext(ctx),
	)
	if err := deps.run(model); err != nil {
		return fmt.Errorf("setup wizard failed: %w", err)
	}
	return nil
}

// resetAll removes the config file, credentials, database, peasant-sync directory, and state directory.
func resetAll(cmd *cobra.Command, configPath string) error {
	// 1. Config file.
	if err := removeIfExists(configPath); err != nil {
		return fmt.Errorf("reset config: %w", err)
	}

	// 2. Credentials.
	if err := auth.ClearCredentialsFrom(configDirOverride(cmd)); err != nil {
		return fmt.Errorf("reset credentials: %w", err)
	}

	// 3. Database file.
	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	if err := removeIfExists(dbPath); err != nil {
		return fmt.Errorf("reset database: %w", err)
	}

	// 4. Peasant-sync directory (ingested transcripts + metadata).
	syncDir := filepath.Join(string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd))), "peasant-sync")
	if err := removeAllIfExists(syncDir); err != nil {
		return fmt.Errorf("reset peasant-sync: %w", err)
	}

	// 5. State directory (PID files).
	stateDir := string(defaults.State.DirPath)
	if err := removeAllIfExists(stateDir); err != nil {
		return fmt.Errorf("reset state: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Reset: cleaned config, credentials, database, peasant-sync, and state.\n")

	return nil
}

// removeIfExists removes a file. Missing file is not an error.
func removeIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		fmt.Fprintf(os.Stderr, "  removed %s\n", path)
	}
	return nil
}

// removeAllIfExists removes a directory tree. Missing directory is not an error.
func removeAllIfExists(path string) error {
	_, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "  removed %s\n", path)
	return nil
}

// ftueDiscover runs the DISCOVER stage for each configured provider and returns
// typed provider inventory plus a flat session listing for display in the wizard.
// Errors are silenced — the wizard shows zero counts instead of failing.
// The optional spinner is updated with progress during discovery and git resolution.
//
// dbPath names the local analytics database. When it already carries this
// build's schema, the sessions it recorded are reused instead of being resolved
// from git again (see loadKnownSessions); a missing or unusable database simply
// resolves everything.
func ftueDiscover(ctx context.Context, configPath, dbPath string, spinner *discoverySpinner) (ftue.ProviderInventory, []ftue.SessionListing) {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return ftue.ProviderInventory{}, nil
	}
	// One open store answers both questions a scan asks of it: what it already
	// resolved from git, and what discovery already mined from the transcripts.
	db := openReusableStore(dbPath)
	var cache ingest.ClaudeEvidenceCache
	if db != nil {
		defer db.Close()
		cache = db
	}
	return ftueDiscoverWith(ctx, cfg, &ingest.OSFileSystem{}, &ingest.ExecGitResolver{},
		loadKnownSessionsFrom(ctx, db), cache, spinner)
}

// ftueDiscoverWith is the discovery core, with the filesystem, the git resolver,
// the store-recorded session index, and the transcript evidence cache injected.
// A non-empty index turns the per-session git resolution, the multi-second part
// of a scan, into a lookup for every session the store already holds and whose
// source has not changed. The evidence cache does the same for the transcript
// mining that links Claude teammate sessions. A nil cache mines every
// transcript again, which is slower but always correct.
func ftueDiscoverWith(
	ctx context.Context,
	cfg *config.Config,
	fs ingest.FileSystem,
	git ingest.GitResolver,
	known knownSessionIndex,
	evidence ingest.ClaudeEvidenceCache,
	spinner *discoverySpinner,
) (ftue.ProviderInventory, []ftue.SessionListing) {
	inventory := ftue.ProviderInventory{}
	var sessions []ftue.SessionListing

	staleness := configStaleness(cfg)

	for provider, factory := range ingest.DefaultAdapterRegistry {
		src, _, ok := resolveConfiguredSource(cfg, provider)
		if !ok {
			continue
		}
		discovery := ftue.ProviderDiscovery{Enabled: src.Enabled}
		inventory[provider] = discovery
		if len(src.Paths) == 0 {
			discovery.State = ftue.DiscoveryUnavailable
			discovery.Detail = "no transcript paths are configured"
			inventory[provider] = discovery
			continue
		}
		// Kickstart inventories available data before the user chooses providers.
		// The saved selection still controls which providers ingestion enables.
		src.Enabled = true
		spinner.SetPhase(fmt.Sprintf("Discovering %s sessions...", schema.HarnessDisplayName(provider)))
		adapter := factory(fs, git, salt.Salt{})
		ingest.AttachClaudeEvidenceCache(adapter, evidence)
		discovered, err := adapter.Discover(ctx, src)
		if err != nil {
			discovery.State = ftue.DiscoveryFailed
			discovery.Detail = err.Error()
			inventory[provider] = discovery
			continue
		}
		// Build parent → child session ID map from ALL discovered sessions.
		childMap := buildChildMap(discovered)

		roots := filterRootSessions(discovered)
		rootCount := len(roots)
		spinner.SetPhase(fmt.Sprintf("Resolving %s project info...", schema.HarnessDisplayName(provider)))
		spinner.SetProgress(0, rootCount)
		for idx, d := range roots {
			spinner.SetProgress(idx+1, rootCount)
			date := d.ModTime
			if !d.CreatedAt.IsZero() {
				date = d.CreatedAt
			}
			projectName := d.ProjectName
			title := d.Title
			branchName := d.Branch // prefer per-session branch from session data
			workingDir := d.CWD
			var gitRemote string
			// claudeProjectDir is the decoded project directory for a Claude
			// session, kept so the git resolution below can read its remote.
			claudeProjectDir := ""

			// Claude-specific: decode project slug to filesystem path for project name.
			if provider == defaults.HarnessClaudeCode {
				slug := filepath.Base(filepath.Dir(string(d.SourcePath)))
				if decoded := decodeClaudeSlugToPath(slug); decoded != "" {
					claudeProjectDir = decoded
					if projectName == "" {
						projectName = shortenPath(decoded)
					}
				}
				if projectName == "" {
					projectName = claudeSlugToProjectName(slug)
				}
			}

			if record, ok := known.reusable(d, staleness); ok {
				// The store already resolved this session and its source has not
				// moved since, so reuse what it recorded rather than paying for
				// git again. Values discovery itself carries still win: they come
				// from the source file, which is never staler than the record.
				gitRemote = record.GitRemote
				if branchName == "" {
					branchName = record.Branch
				}
				if title == "" {
					title = record.Title
				}
				if workingDir == "" {
					workingDir = record.workingDirectory()
				}
			} else {
				gitRemote, branchName = resolveSessionGit(ctx, git, d, claudeProjectDir, branchName)
			}

			sessions = append(sessions, ftue.SessionListing{
				Harness:     provider.String(),
				ProjectName: projectName,
				GitRemote:   gitRemote,
				Branch:      branchName,
				Title:       title,
				Date:        date,
				SessionID:   string(d.SessionID),
				SubagentIDs: childMap[string(d.SessionID)],
				WorkingDir:  workingDir,
				// The transcript location travels with the listing so the
				// selection step can preview a session before any import. The
				// file stays where the harness wrote it.
				Source: kickstart.ListingSource(d),
			})
		}
		discovery.SessionCount = rootCount
		// A provider can enumerate some sources and skip others without failing.
		// Name the skipped databases so a short session count is explained rather
		// than silent.
		if reporter, ok := adapter.(ingest.DiscoveryDiagnosticReporter); ok {
			if diagnostics := reporter.DiscoveryDiagnostics(); len(diagnostics) > 0 {
				skipped := make([]string, 0, len(diagnostics))
				for _, diagnostic := range diagnostics {
					skipped = append(skipped, fmt.Sprintf("%s (%s)", diagnostic.Location, diagnostic.Summary))
				}
				discovery.Detail = fmt.Sprintf("%d source(s) could not be fully read: %s", len(diagnostics), strings.Join(skipped, "; "))
			}
		}
		inventory[provider] = discovery
	}
	return inventory, sessions
}

// resolveSessionGit walks git for one discovered session's remote and branch.
// This is the multi-second part of a scan: one process per lookup, per session.
// claudeProjectDir is the decoded Claude project directory when there is one,
// and branchName is the branch the session data already carries.
func resolveSessionGit(
	ctx context.Context,
	git ingest.GitResolver,
	d ingest.DiscoveredSession,
	claudeProjectDir string,
	branchName string,
) (string, string) {
	var gitRemote string

	// Claude records the project directory in its path, which is the most
	// precise place to ask.
	if claudeProjectDir != "" {
		if remote, err := git.RemoteURL(ctx, claudeProjectDir); err == nil && remote != "" {
			gitRemote = remote
		}
	}

	// For all providers: if we still need git remote, try to resolve
	// from the session's CWD or project directory.
	if gitRemote == "" {
		// Try the session's working directory first (most accurate).
		if d.CWD != "" {
			if remote, err := git.RemoteURL(ctx, d.CWD); err == nil && remote != "" {
				gitRemote = remote
			}
		}
		// Fall back to OriginalRoot or source file directory.
		if gitRemote == "" && d.ProjectName != "" {
			dir := string(d.OriginalRoot)
			if dir == "" {
				dir = filepath.Dir(string(d.SourcePath))
			}
			if remote, err := git.RemoteURL(ctx, dir); err == nil && remote != "" {
				gitRemote = remote
			}
		}
	}

	// If we still don't have a branch, try resolving from CWD.
	// This is a last resort — prefers session-recorded branch above.
	if branchName == "" && d.CWD != "" {
		if branch, err := git.Branch(ctx, d.CWD); err == nil && branch != "" {
			branchName = branch
		}
	}
	return gitRemote, branchName
}

// filterRootSessions returns only root sessions (no subagents).
func filterRootSessions(discovered []ingest.DiscoveredSession) []ingest.DiscoveredSession {
	var roots []ingest.DiscoveredSession
	for _, d := range discovered {
		if d.ParentUUID == nil {
			roots = append(roots, d)
		}
	}
	return roots
}

// decodeClaudeSlugToPath decodes a Claude project slug to the real filesystem path.
// Delegates to ingest.DecodeClaudeSlug with an os.Stat-based directory checker.
func decodeClaudeSlugToPath(encoded string) string {
	return ingest.DecodeClaudeSlug(encoded, func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.IsDir()
	})
}

// shortenPath replaces the home directory prefix with "~" for display.
func shortenPath(path string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(path, home) {
		return "~" + path[len(home):]
	}
	return path
}

// claudeSlugToProjectName converts a Claude project slug (e.g. "-Users-alice-GitHub-my-repo")
// to a human-readable name (e.g. "my-repo"). It mirrors the logic in the village's
// extractProjectDisplayName for the dash-delimited slug case.
func claudeSlugToProjectName(slug string) string {
	if slug == "" {
		return ""
	}
	knownDirs := []string{"github", "documents", "projects", "repos", "src", "code", "dev", "home"}
	stripped := strings.TrimPrefix(slug, "-")
	segments := strings.Split(stripped, "-")
	lastKnownIdx := -1
	for i, seg := range segments {
		for _, known := range knownDirs {
			if strings.EqualFold(seg, known) {
				lastKnownIdx = i
				break
			}
		}
	}
	if lastKnownIdx >= 0 && lastKnownIdx < len(segments)-1 {
		return strings.Join(segments[lastKnownIdx+1:], "-")
	}
	return slug
}

// progressAdapter wraps ingest.ProgressState to satisfy ftue.ProgressSnapshot.
type progressAdapter struct {
	state *ingest.ProgressState
}

func (a *progressAdapter) Snapshot() map[string]ftue.StageProgress {
	raw := a.state.Snapshot()
	result := make(map[string]ftue.StageProgress, len(raw))
	for name, sp := range raw {
		result[string(name)] = ftue.StageProgress{
			Done:   sp.Done,
			Total:  sp.Total,
			Ended:  sp.Ended,
			HasErr: sp.HasErr,
		}
	}
	return result
}

// buildFTUEIngestRunner returns the IngestRunnerFunc the wizard's IngestPage calls
// after the user confirms import. The config is re-loaded at call time so it picks
// up the file the wizard just saved.
func buildFTUEIngestRunner(cmd *cobra.Command, configPath string) (ftue.IngestRunnerFunc, ftue.ProgressSnapshot) {
	runner, progress := buildFTUEIngestRunnerWithProgress(cmd, configPath)
	return runner, &progressAdapter{state: progress}
}

// buildFTUEIngestRunnerWithProgress constructs the retained runner together with
// the exact ProgressState it writes. Guided kickstart consumes the native state
// through its narrow ProgressSource boundary; the legacy wizard receives the
// compatibility adapter above. Both presentations therefore observe the same
// pipeline run rather than parallel counters.
func buildFTUEIngestRunnerWithProgress(cmd *cobra.Command, configPath string) (ftue.IngestRunnerFunc, *ingest.ProgressState) {
	progState := ingest.NewProgressState()
	return func(ctx context.Context, answers ftue.WizardAnswers) (*ftue.IngestResult, error) {
		// Suppress log output so it doesn't corrupt the TUI alt-screen.
		origLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer slog.SetDefault(origLogger)

		cfg, err := loadConfig(configPath)
		if err != nil {
			return nil, fmt.Errorf("load config: %w", err)
		}

		// The same refusal the harvest command makes. The wizard cannot offer an
		// unsupported level, so reaching this means a configuration file already
		// carried one before the wizard ran; importing under it anyway would build
		// a store the user believes is protected and is not.
		if levelErr := checkIngestRedactionLevel(cfg, configPath, "peasant kickstart"); levelErr != nil {
			return nil, levelErr
		}

		fs := &ingest.OSFileSystem{}
		git := &ingest.ExecGitResolver{}

		sources := ftueSources(cfg, answers)

		outputPath := cfg.Output.BasePath
		if outputPath == "" {
			outputPath = string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
		}
		resolvedOutput, err := ingest.NewResolvedPath(outputPath)
		if err != nil {
			return nil, fmt.Errorf("resolve output path: %w", err)
		}

		staleness := configStaleness(cfg)

		// Map selected sessions to AllowedSessionIDs filter, expanding
		// parent sessions to include their subagent children.
		allowedIDs := kickstartAllowedSessionIDs(answers)

		pipelineCfg := ingest.PipelineConfig{
			Sources:            sources,
			OutputDir:          resolvedOutput,
			StalenessThreshold: staleness,
			Parallelism:        0,
			Progress:           progState,
			AllowedSessionIDs:  allowedIDs,
		}

		// Open DB and wire analytics stages.
		dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
		if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
		db, err := store.Open(string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd))))
		if err != nil {
			return nil, fmt.Errorf("open analytics store: %w", err)
		}
		defer db.Close()

		pipelineOpts := []ingest.PipelineOption{
			ingest.WithStore(db),
			ingest.WithMetricsStore(db),
		}
		pipelineOpts = append(pipelineOpts,
			ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
			ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
			ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)),
			ingest.WithLogger(db),
			ingest.WithIndexLogger(db),
		)

		pipeline, err := ingest.NewPipeline(fs, git, ingest.DefaultAdapterRegistry, pipelineCfg, pipelineOpts...)
		if err != nil {
			return nil, fmt.Errorf("create pipeline: %w", err)
		}
		result, err := pipeline.Run(ctx)
		if err != nil {
			return nil, fmt.Errorf("pipeline failed: %w", err)
		}

		providerCounts := aggregateProviderCounts(result.Sessions)

		s := result.Summary
		return &ftue.IngestResult{
			New:            s.New,
			Updated:        s.Updated,
			Unchanged:      s.Unchanged,
			Errors:         s.Errors,
			Duration:       result.Duration,
			ProviderCounts: providerCounts,
		}, nil
	}, progState
}

// kickstartAllowedSessionIDs preserves the difference between unrestricted
// legacy wizard input and a committed selected-mode policy that currently
// matches no discovered sessions. Pipeline uses nil to mean allow all, so the
// latter must cross this adapter as an allocated empty set.
func kickstartAllowedSessionIDs(answers ftue.WizardAnswers) map[ingest.SessionID]bool {
	allowed := expandAllowedSessionIDs(answers.EffectiveSelectedSessions())
	if allowed == nil && (answers.SelectionMode == config.SelectionModeSelected ||
		hasRestrictedProviderSelection(answers.ProviderSelections)) {
		return map[ingest.SessionID]bool{}
	}
	return allowed
}

func hasRestrictedProviderSelection(selections []ftue.ProviderSelection) bool {
	for _, selection := range selections {
		if !selection.ImportAll {
			return true
		}
	}
	return false
}

// ftueSources builds the pipeline source map from config, filtered by the wizard's
// per-provider selections.
func ftueSources(cfg *config.Config, answers ftue.WizardAnswers) map[defaults.Harness]ingest.SourceConfig {
	all := buildSourceConfigs(cfg)

	if len(answers.ProviderSelections) == 0 {
		// No provider selections — run the full source set.
		return all
	}

	// Build a set of selected provider strings for fast lookup.
	selected := make(map[string]bool, len(answers.ProviderSelections))
	for _, ps := range answers.ProviderSelections {
		selected[ps.Harness] = true
	}

	filtered := make(map[defaults.Harness]ingest.SourceConfig, len(all))
	for provider, src := range all {
		if selected[provider.String()] {
			filtered[provider] = src
		}
	}
	return filtered
}

// aggregateProviderCounts builds per-provider new/updated counts from session results,
// skipping errored sessions.
func aggregateProviderCounts(sessions []ingest.SessionResult) []ftue.ProviderIngestCount {
	providerMap := map[string]*ftue.ProviderIngestCount{}
	for _, sr := range sessions {
		if sr.Error != nil {
			continue
		}
		name := schema.HarnessDisplayName(sr.Harness)
		pc, ok := providerMap[name]
		if !ok {
			pc = &ftue.ProviderIngestCount{Harness: name}
			providerMap[name] = pc
		}
		switch sr.Status {
		case ingest.DiffNew:
			pc.New++
		case ingest.DiffUpdated:
			pc.Updated++
		}
	}
	var counts []ftue.ProviderIngestCount
	for _, pc := range providerMap {
		if pc.New > 0 || pc.Updated > 0 {
			counts = append(counts, *pc)
		}
	}
	sort.Slice(counts, func(i, j int) bool {
		return counts[i].Harness < counts[j].Harness
	})
	return counts
}

// expandAllowedSessionIDs builds the AllowedSessionIDs map from selected sessions,
// expanding each parent session to include its subagent child session IDs.
// Returns nil when no sessions are selected (nil means "allow all" in the pipeline).
func expandAllowedSessionIDs(selected []ftue.SessionListing) map[ingest.SessionID]bool {
	if len(selected) == 0 {
		return nil
	}
	allowed := make(map[ingest.SessionID]bool, len(selected))
	for _, s := range selected {
		if sid, err := ingest.NewSessionID(s.SessionID); err == nil {
			allowed[sid] = true
		}
		for _, childID := range s.SubagentIDs {
			if sid, err := ingest.NewSessionID(childID); err == nil {
				allowed[sid] = true
			}
		}
	}
	return allowed
}

// buildChildMap builds a mapping from parent session ID to child session IDs
// from the full (unfiltered) discovery results.
func buildChildMap(discovered []ingest.DiscoveredSession) map[string][]string {
	childMap := make(map[string][]string)
	for _, d := range discovered {
		if d.ParentUUID != nil {
			parentID := string(*d.ParentUUID)
			childMap[parentID] = append(childMap[parentID], string(d.SessionID))
		}
	}
	return childMap
}
