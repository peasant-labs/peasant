package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

// runKickstartFlow mounts the rebuilt onboarding: the declarative settings.Flow
// (the single atomic config commit point) sequenced after an optional village
// OAuth step and before ingest, all rendered on the kit. It reuses the SAME
// business logic the legacy wizard drove - discovery (already run into inventory
// + sessions), the login runner (internal/auth.Login), the ingest pipeline, and
// the Claude retention writer - wiring each as an injected seam of
// kickstart.Program. The legacy FTUE wizard construction remains present in
// cmd_kickstart.go as a deprecation candidate; this is the entry point the
// command now mounts.
func runKickstartFlow(
	cmd *cobra.Command,
	deps kickstartCommandDeps,
	configPath string,
	inventory ftue.ProviderInventory,
	sessions []ftue.SessionListing,
) error {
	ctx := cmd.Context()

	// Load (or seed) the config the flow's draft buffers and commits atomically.
	loaded, err := loadConfig(configPath)
	if err != nil {
		loaded = config.BaseConfig()
	}
	th := theme.New(themeModeFor(loaded))

	// One store handle serves both the already-imported marks and the preview
	// pane's body reads for the whole flow. A legacy all-projects policy also
	// reads its complete stored-session identity evidence through this handle.
	// It is closed when the flow returns.
	db, closeStore, storeOpenErr := openKickstartStoreWithError(cmd)
	defer closeStore()
	if storeOpenErr != nil && loaded.Selection.Mode == config.SelectionModeAll {
		return fmt.Errorf(
			"prepare legacy project selection: %w.\n"+
				"what: Peasant could not open the stored sessions needed to build exact project choices.\n"+
				"why: Peasant could not read the local session store as a Peasant database.\n"+
				"where: runKickstartFlow, before the selection screen opened.\n"+
				"when: while converting the saved all-projects setting.\n"+
				"meaning: Peasant did not change the saved setting or start setup.\n"+
				"fix: check the Peasant data directory and database, then run `peasant kickstart` again.", storeOpenErr)
	}
	identityResolver := ingest.NewPhysicalPathResolver()
	flowConfig, err := prepareKickstartFlowConfig(ctx, loaded, sessions, db, identityResolver)
	if err != nil {
		return err
	}
	draft, err := settings.NewDraft(configPath, flowConfig)
	if err != nil {
		return err
	}
	// Seed the retention field's starting value from Claude Code's current
	// cleanup setting so its cursor lands on the value already in force (or the
	// recommended keep-forever when none is set). The value is transient (never
	// written to config.yaml); the flow carries it to the retention writer.
	seedRetentionChoice(draft)

	programDeps := kickstart.ProgramDeps{
		Theme: th,
		Draft: draft,
		Source: kickstart.NewScannerTreeSource(
			sessions,
			kickstart.WithPathIdentityResolver(identityResolver),
			kickstart.WithIngestedSessionIDs(ingestedSessionIDs(cmd, db)),
		),
		Preview:               kickstartPreview(cmd, db, th, sessions),
		ClaudeSessionsPresent: claudeSessionsPresent(inventory),
		Login:                 kickstartLoginFunc(cmd, configPath),
		Ingest:                kickstartIngestFunc(cmd, configPath, sessions),
		AlreadyConnected:      villageAlreadyConnected(),
		Retention:             kickstart.DefaultRetentionWriter(),
		// The retention value now comes from the flow's retention field, carried
		// through the committed draft. This fallback stays 0 so a run that never
		// offers the field (no Claude sessions) writes nothing.
		RetentionDays: 0,
		Context:       ctx,
	}

	model := kickstart.NewModel(kickstart.NewProgram(programDeps))
	return deps.runFlow(model)
}

// prepareKickstartFlowConfig replaces a legacy mode-all policy only in the
// in-memory draft baseline. It reads every stored session, builds exact physical
// project choices through kickstart.ConvertLegacyAll, and carries unresolved
// stored session IDs into the selected-mode baseline. The config file is not
// changed here. settings.Draft.Commit remains the only write, after the user
// reviews and saves the flow.
func prepareKickstartFlowConfig(
	ctx context.Context,
	loaded *config.Config,
	sessions []ftue.SessionListing,
	db *store.Store,
	resolver ingest.PathIdentityResolver,
) (*config.Config, error) {
	if loaded.Selection.Mode != config.SelectionModeAll {
		return loaded, nil
	}

	var stored []store.IngestedSessionRow
	if db != nil {
		var err error
		stored, err = db.AllIngestedSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf(
				"prepare legacy project selection: %w.\n"+
					"what: Peasant could not read the stored sessions needed to build exact project choices.\n"+
					"why: the local session store query failed.\n"+
					"where: runKickstartFlow, before the selection screen opened.\n"+
					"when: while converting the saved all-projects setting.\n"+
					"meaning: Peasant did not change the saved setting or start setup.\n"+
					"fix: check that the Peasant data directory is readable, then run `peasant kickstart` again.", err)
		}
	}

	conversion, err := kickstart.ConvertLegacyAll(
		sessions,
		stored,
		resolver,
		loaded.Selection.AutoIngestNewBranches,
	)
	if err != nil {
		return nil, err
	}
	merged := settings.MergeSelection(settings.TreeSelection{
		Mode:      conversion.Initial.Mode,
		Harnesses: conversion.Initial.Harnesses,
	}, conversion.Unmatched)
	selection := conversion.Initial
	selection.Harnesses = merged.Harnesses
	converted := *loaded
	converted.Selection = selection
	return &converted, nil
}

// openKickstartStore opens the local store for the duration of the flow, plus
// the func that closes it. It is best-effort: on a first run the store may not
// exist yet, and any open problem yields a nil store and a no-op close, so
// onboarding runs without the marks and preview bodies it would have provided.
func openKickstartStore(cmd *cobra.Command) (*store.Store, func()) {
	db, closeStore, _ := openKickstartStoreWithError(cmd)
	return db, closeStore
}

// openKickstartStoreWithError is the exact store-open boundary used by legacy
// conversion. The compatibility wrapper above keeps preview and imported-mark
// reads best-effort, while conversion can reject an unreadable existing store
// instead of mistaking unavailable evidence for an empty store.
func openKickstartStoreWithError(cmd *cobra.Command) (*store.Store, func(), error) {
	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return nil, func() {}, nil
		}
		return nil, func() {}, err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, func() {}, err
	}
	return db, func() { _ = db.Close() }, nil
}

// ingestedSessionIDs reads the session ids the local store already holds so the
// scanner can mark already-ingested sessions in the tree. Any read problem
// simply leaves every session unmarked rather than failing onboarding.
func ingestedSessionIDs(cmd *cobra.Command, db *store.Store) []string {
	if db == nil {
		return nil
	}
	ids, err := db.AllSessionIDs(cmd.Context())
	if err != nil {
		return nil
	}
	return ids
}

// kickstartPreview builds the selection step's side preview over the discovery
// listing and the local store: the highlighted session is named from the same
// listing the tree was folded from, and its body is the WHOLE recorded
// transcript the store holds. With no store the preview still names the session
// and says it is not imported yet, which is exactly true on a first run.
//
// The turns come from api.StoreDataProvider.SessionByID - the SAME read the
// session_detail channel and the transcript viewer use - rather than a second
// hand-rolled query. That is what gets the preview the full turn bodies:
// SessionByID folds session_entries into turns and, when anything in the
// session hit the DB's content-preview limit, overlays the untruncated bodies
// from the source transcript. A preview built on the stored previews alone
// would cut turns off mid-word, which is a bug that path already fixed once.
//
// Visibility is deliberately sessionvisibility.All: kickstart is where a
// selection is being CHOSEN, so scoping the preview by a selection the user has
// not committed yet would hide the very rows they are deciding about. This
// matches the ratified model in which a selection scopes discovery lists rather
// than access to stored data.
func kickstartPreview(cmd *cobra.Command, db *store.Store, th theme.Theme, sessions []ftue.SessionListing) kit.BodySource {
	var turns kickstart.SessionTurnsFunc
	if db != nil {
		ctx := cmd.Context()
		provider := api.NewStoreDataProvider(db, sessionvisibility.All())
		turns = func(sessionID string) ([]ingest.Turn, error) {
			// Ask whether the store holds this session BEFORE reading it, so
			// the two outcomes stay distinguishable: a session that was never
			// imported is the normal onboarding case and reads "not imported
			// yet", while a store that could not be read is a real failure the
			// pane must surface. Collapsing both into "no turns" would report a
			// broken database as an empty session.
			row, err := db.SessionDetailByID(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			if row == nil {
				return nil, nil
			}
			session, err := provider.SessionByID(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			return session.Turns, nil
		}
	}
	return kickstart.NewListingPreview(th, sessions, turns)
}

// themeModeFor picks the palette mode from config, defaulting to dark when the
// config names no (or an unknown) theme.
func themeModeFor(cfg *config.Config) theme.Mode {
	if cfg == nil {
		return theme.ModeDark
	}
	mode, err := theme.ModeFromConfig(string(cfg.Display.Theme))
	if err != nil {
		return theme.ModeDark
	}
	return mode
}

// seedRetentionChoice sets the draft's transient Claude retention value from the
// cleanup period already written in ~/.claude/settings.json, so the retention
// field opens on the value in force. When no value is set, it defaults to the
// recommended keep-forever so a first-time user's cursor lands on the safe
// choice. The value is never persisted to config.yaml (yaml:"-").
func seedRetentionChoice(draft *settings.Draft) {
	if days, ok := ftue.ReadClaudeCleanupDays(); ok && days > 0 {
		draft.Working().ClaudeRetentionDays = days
		return
	}
	draft.Working().ClaudeRetentionDays = kickstart.RecommendedRetentionDays
}

// claudeSessionsPresent reports whether the discovery inventory found any Claude
// Code sessions, which gates the Claude retention preference.
func claudeSessionsPresent(inventory ftue.ProviderInventory) bool {
	d, ok := inventory[defaults.HarnessClaudeCode]
	return ok && d.SessionCount > 0
}

// villageAlreadyConnected reports whether this machine already holds valid
// village credentials, so the connect-now step can be skipped (the UAT flagged
// that it was shown even when already connected).
func villageAlreadyConnected() bool {
	creds, err := auth.LoadCredentials()
	return err == nil && creds != nil && creds.IsValid()
}

// kickstartLoginFunc adapts the existing auth.Login runner to the program's
// LoginFunc seam, resolving the village URL exactly as `peasant login` does.
func kickstartLoginFunc(cmd *cobra.Command, configPath string) kickstart.LoginFunc {
	return func(ctx context.Context) (string, error) {
		villageURL := os.Getenv("PEASANT_VILLAGE_URL")
		if villageURL == "" {
			if cfg, err := loadConfig(configPath); err == nil && cfg.Village.URL != "" {
				villageURL = cfg.Village.URL
			}
		}
		if villageURL == "" {
			villageURL = defaults.DefaultVillageURL.String()
		}
		creds, err := auth.Login(ctx, villageURL, false)
		if err != nil {
			return "", err
		}
		return creds.Username, nil
	}
}

// kickstartIngestFunc builds the post-commit ingest step. It reuses the existing
// ftue ingest runner (buildFTUEIngestRunner) and the canonical selection matcher:
// after the flow saves config.Selection, this reloads the config, derives which
// discovered sessions the saved selection admits via ingest.SelectionMatcher (the
// same matcher ingest, push, discovery, and prune use), and hands them to the
// existing runner. Mode:all imports everything; mode:selected imports exactly the
// admitted sessions.
func kickstartIngestFunc(cmd *cobra.Command, configPath string, sessions []ftue.SessionListing) kickstart.IngestFunc {
	runner, _ := buildFTUEIngestRunner(cmd, configPath)
	return kickstartIngestFuncWithRunner(configPath, sessions, ingest.NewPhysicalPathResolver(), runner)
}

// kickstartIngestFuncWithRunner builds the same post-save callback with its
// filesystem identity boundary and ingest runner injected. Production supplies
// the physical resolver and real pipeline runner; focused mounted tests replace
// only those dependencies and exercise this callback unchanged.
func kickstartIngestFuncWithRunner(
	configPath string,
	sessions []ftue.SessionListing,
	resolver ingest.PathIdentityResolver,
	runner ftue.IngestRunnerFunc,
) kickstart.IngestFunc {
	return func(ctx context.Context) (*ftue.IngestResult, error) {
		cfg, err := loadConfig(configPath)
		if err != nil {
			return nil, err
		}
		answers := deriveKickstartAnswers(cfg, sessions, resolver)
		return runner(ctx, answers)
	}
}

// deriveKickstartAnswers translates the committed config.Selection into the
// ftue.WizardAnswers the existing ingest runner consumes, reusing the canonical
// selection matcher so kickstart imports exactly what the saved selection admits.
func deriveKickstartAnswers(
	cfg *config.Config,
	sessions []ftue.SessionListing,
	resolver ingest.PathIdentityResolver,
) ftue.WizardAnswers {
	// Mode:all - import every discovered provider's sessions.
	if cfg.Selection.Mode != config.SelectionModeSelected {
		harnesses := map[string]bool{}
		for _, s := range sessions {
			harnesses[s.Harness] = true
		}
		var provs []ftue.ProviderSelection
		for h := range harnesses {
			provs = append(provs, ftue.ProviderSelection{Harness: h, ImportAll: true})
		}
		return ftue.WizardAnswers{ProviderSelections: provs}
	}

	matcher := config.CompileSelectionMatcher(cfg.Selection)
	selectedHarnesses := map[string]bool{}
	var selected []ftue.SessionListing
	for _, prepared := range kickstart.PrepareSessionListings(sessions, resolver) {
		match := matcher.MatchDiscoveryCandidate(prepared.Candidate, cfg.Selection.AutoIngestNewBranches)
		if match == ingest.BranchMatchYes {
			selected = append(selected, prepared.Listing)
			selectedHarnesses[prepared.Listing.Harness] = true
		}
	}
	var provs []ftue.ProviderSelection
	for h := range selectedHarnesses {
		provs = append(provs, ftue.ProviderSelection{Harness: h, ImportAll: false})
	}
	return ftue.WizardAnswers{
		ProviderSelections: provs,
		SelectedSessions:   selected,
	}
}
