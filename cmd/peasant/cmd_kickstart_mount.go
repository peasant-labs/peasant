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
	draft, err := settings.NewDraft(configPath, loaded)
	if err != nil {
		return err
	}
	// Seed the retention field's starting value from Claude Code's current
	// cleanup setting so its cursor lands on the value already in force (or the
	// recommended keep-forever when none is set). The value is transient (never
	// written to config.yaml); the flow carries it to the retention writer.
	if err := seedRetentionChoice(draft, deps.readRetention); err != nil {
		return err
	}

	th := theme.New(themeModeFor(loaded))

	// One store handle serves both the already-imported marks and the preview
	// pane's body reads for the whole flow; it is closed when the flow returns.
	db, closeStore := openKickstartStore(cmd)
	defer closeStore()

	programDeps := kickstart.ProgramDeps{
		Theme:                 th,
		Draft:                 draft,
		Source:                kickstart.NewScannerTreeSource(sessions, kickstart.WithIngestedSessionIDs(ingestedSessionIDs(cmd, db))),
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
	if deps.runModel == nil {
		return fmt.Errorf(
			"mount guided kickstart for %q: terminal runner is nil.\n"+
				"what: the guided Program cannot be started.\n"+
				"why: kickstartCommandDeps.runModel was assembled without its Bubble Tea boundary.\n"+
				"where: runKickstartFlow after the Program and Flow were constructed.\n"+
				"when: immediately before entering the interactive terminal session.\n"+
				"means: the draft was not committed and local ingest did not run.\n"+
				"fix: construct kickstart through BuildKickstartCommand or supply the terminal runner.",
			configPath)
	}
	return deps.runModel(model)
}

// openKickstartStore opens the local store for the duration of the flow, plus
// the func that closes it. It is best-effort: on a first run the store may not
// exist yet, and any open problem yields a nil store and a no-op close, so
// onboarding runs without the marks and preview bodies it would have provided.
func openKickstartStore(cmd *cobra.Command) (*store.Store, func()) {
	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
	if _, err := os.Stat(dbPath); err != nil {
		return nil, func() {}
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, func() {}
	}
	return db, func() { _ = db.Close() }
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

// seedRetentionChoice initializes both copies of the draft's transient Claude
// retention value from the cleanup period already written in
// ~/.claude/settings.json, so the retention field opens clean on the value in
// force. When no value is set, it defaults to the recommended keep-forever
// choice. The value is never persisted to config.yaml (yaml:"-").
func seedRetentionChoice(draft *settings.Draft, read func() (int, bool)) error {
	if read == nil {
		return fmt.Errorf(
			"seed guided retention: Claude settings reader is nil.\n" +
				"what: the retention field cannot be initialized from the value currently in force.\n" +
				"why: kickstartCommandDeps.readRetention was assembled without its read boundary.\n" +
				"where: seedRetentionChoice before settings.Flow mounts.\n" +
				"when: after opening the Draft and before any interactive field is shown.\n" +
				"means: the flow was not mounted and no configuration or Claude setting was written.\n" +
				"fix: construct kickstart through BuildKickstartCommand or supply the Claude retention reader.")
	}
	days := kickstart.RecommendedRetentionDays
	if current, ok := read(); ok && current > 0 {
		days = current
	}
	return kickstart.SeedRetentionInitial(draft, days)
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
	return func(ctx context.Context) (*ftue.IngestResult, error) {
		cfg, err := loadConfig(configPath)
		if err != nil {
			return nil, err
		}
		answers := deriveKickstartAnswers(cfg, sessions)
		return runner(ctx, answers)
	}
}

// deriveKickstartAnswers translates the committed config.Selection into the
// ftue.WizardAnswers the existing ingest runner consumes, reusing the canonical
// selection matcher so kickstart imports exactly what the saved selection admits.
func deriveKickstartAnswers(cfg *config.Config, sessions []ftue.SessionListing) ftue.WizardAnswers {
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
	for _, s := range sessions {
		match := matcher.MatchDiscovery(
			ingest.Harness(s.Harness), s.GitRemote, s.ProjectName, s.Branch,
			ingest.SessionID(s.SessionID), cfg.Selection.AutoIngestNewBranches)
		if match == ingest.BranchMatchYes {
			selected = append(selected, s)
			selectedHarnesses[s.Harness] = true
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
