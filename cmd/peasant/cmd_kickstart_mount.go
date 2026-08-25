package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kickstart"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/settings"
	"github.com/peasant-labs/peasant/internal/tui/theme"
)

const kickstartRawSourcePreviewLimit = 16 * 1024

// runKickstartFlow mounts the rebuilt onboarding: the declarative settings.Flow
// (the single atomic config commit point) sequenced after an optional village
// OAuth step and before ingest, all rendered on the kit. It reuses the SAME
// business logic the legacy wizard drove - discovery (already run into inventory
// + sessions), the path-aware login runner (internal/auth.LoginFrom), the ingest pipeline, and
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

	// One store handle serves the stored-session snapshot, already-imported marks,
	// and preview bodies for the whole flow. The snapshot feeds both legacy
	// conversion and the scanner/store union used by the save gate. It is closed
	// when the flow returns.
	db, closeStore, storeOpenErr := openKickstartStoreWithError(cmd)
	defer closeStore()
	if storeOpenErr != nil {
		return kickstartStoreOpenError(loaded.Selection.Mode, storeOpenErr)
	}
	storedSessions, err := readKickstartStoredSessions(ctx, db)
	if err != nil {
		return kickstartStoreReadError(loaded.Selection.Mode, err)
	}
	identityResolver := deps.pathResolver
	if identityResolver == nil {
		identityResolver = ingest.NewPhysicalPathResolver()
	}
	repositoryResolver := deps.repositoryResolver
	if repositoryResolver == nil {
		repositoryResolver = ingest.NewGitRepositoryIdentityResolver()
	}
	flowConfig, err := prepareKickstartFlowConfig(loaded, sessions, storedSessions, identityResolver)
	if err != nil {
		return err
	}
	// Keep the file-backed config as the draft baseline. A legacy conversion is
	// an in-memory working edit so the user's final confirmation persists it;
	// treating the conversion as the baseline would make Commit preserve the
	// legacy bytes as a semantically clean no-op.
	draft, err := settings.NewDraft(configPath, loaded)
	if err != nil {
		return err
	}
	if flowConfig != loaded {
		draft.Working().Selection = flowConfig.Selection
	}
	// Seed the retention field's starting value from Claude Code's current
	// cleanup setting so its cursor lands on the value already in force (or the
	// recommended keep-forever when none is set). The value is transient (never
	// written to config.yaml); the flow carries it to the retention writer.
	if err := seedRetentionChoice(draft, deps.readRetention); err != nil {
		return err
	}

	source := kickstart.NewScannerTreeSource(
		sessions,
		kickstart.WithPathIdentityResolver(identityResolver),
		kickstart.WithRepositoryIdentityResolver(repositoryResolver),
		kickstart.WithIngestedSessionIDs(ingestedSessionIDs(cmd, db)),
	)
	commitGateCandidates, err := source.CommitGateCandidates(storedSessions)
	if err != nil {
		return fmt.Errorf(
			"prepare project save evidence: %w.\n"+
				"what: Peasant could not build the complete project and session evidence for the save check.\n"+
				"why: a locally stored session has identity data that Peasant cannot validate safely.\n"+
				"where: runKickstartFlow, before the selection screen opened.\n"+
				"when: while joining scanner and stored-session evidence for the no-project confirmation.\n"+
				"meaning: Peasant did not change the saved setting or start ingest because project availability is unknown.\n"+
				"fix: run `peasant ingest verify`, repair the reported stored session, then run `peasant kickstart` again.", err)
	}
	if deps.localIngest == nil {
		return fmt.Errorf(
			"mount guided kickstart for %q: local ingest boundary is nil.\n"+
				"what: the guided Program cannot start the post-consent local import.\n"+
				"why: kickstartCommandDeps.localIngest was assembled without the shared runner and progress source.\n"+
				"where: runKickstartFlow before constructing kickstart.Program.\n"+
				"when: after opening the buffered draft and before any interactive choice is shown.\n"+
				"means: no configuration, retention setting, or transcript was changed.\n"+
				"fix: construct kickstart through BuildKickstartCommand or supply the local ingest boundary.",
			configPath)
	}
	ingestRun, progress := deps.localIngest(cmd, configPath, sessions)
	alreadyConnected := false
	if deps.alreadyConnected != nil {
		alreadyConnected = deps.alreadyConnected(configDirOverride(cmd))
	}
	if deps.flowIngest != nil {
		ingestRun = deps.flowIngest
	}

	programDeps := kickstart.ProgramDeps{
		Theme:                 th,
		Draft:                 draft,
		Source:                source,
		CommitGate:            settings.NewCommitGateEvaluator(commitGateCandidates),
		Preview:               kickstartPreview(cmd, db, th, sessions, source),
		ClaudeSessionsPresent: claudeSessionsPresent(inventory),
		Login:                 kickstartLoginFunc(cmd, configPath),
		Ingest:                ingestRun,
		Progress:              progress,
		AlreadyConnected:      alreadyConnected,
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

// prepareKickstartFlowConfig replaces legacy mode-all and pathless selected-mode
// policies only in the in-memory draft baseline. It receives the same complete
// stored-session snapshot the save gate uses. Mode all also consults the scanner
// cohort; selected migration deliberately does not, so scanner-only sibling
// clones stay clear. The config file is not changed here.
// settings.Draft.Commit remains the only write, after the user reviews and saves
// the flow.
func prepareKickstartFlowConfig(
	loaded *config.Config,
	sessions []ftue.SessionListing,
	stored []store.IngestedSessionRow,
	resolver ingest.PathIdentityResolver,
) (*config.Config, error) {
	if loaded.Selection.Mode == config.SelectionModeSelected && hasPathlessSelectedProject(loaded.Selection) {
		selection, err := kickstart.ConvertLegacySelected(loaded.Selection, stored, resolver)
		if err != nil {
			return nil, err
		}
		converted := *loaded
		converted.Selection = selection
		return &converted, nil
	}
	if loaded.Selection.Mode != config.SelectionModeAll {
		return loaded, nil
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

func hasPathlessSelectedProject(selection config.SelectionConfig) bool {
	if selection.Mode != config.SelectionModeSelected {
		return false
	}
	for _, configured := range selection.Harnesses {
		for _, project := range configured.Projects {
			if len(project.ClonePaths) == 0 {
				return true
			}
		}
	}
	return false
}

func readKickstartStoredSessions(ctx context.Context, db *store.Store) ([]store.IngestedSessionRow, error) {
	if db == nil {
		return nil, nil
	}
	return db.AllIngestedSessions(ctx)
}

func kickstartStoreOpenError(mode config.SelectionMode, err error) error {
	if mode == config.SelectionModeAll {
		return fmt.Errorf(
			"prepare legacy project selection: %w.\n"+
				"what: Peasant could not open the stored sessions needed to build exact project choices.\n"+
				"why: Peasant could not read the local session store as a Peasant database.\n"+
				"where: runKickstartFlow, before the selection screen opened.\n"+
				"when: while converting the saved all-projects setting.\n"+
				"meaning: Peasant did not change the saved setting or start setup.\n"+
				"fix: check the Peasant data directory and database, then run `peasant kickstart` again.", err)
	}
	return fmt.Errorf(
		"prepare saved project availability: %w.\n"+
			"what: Peasant could not open the stored sessions needed to verify the saved project choice.\n"+
			"why: Peasant could not read the local session store as a Peasant database.\n"+
			"where: runKickstartFlow, before the selection screen opened.\n"+
			"when: while aligning the no-project save check with the web viewer and push chooser.\n"+
			"meaning: Peasant did not change the saved setting or start ingest because project availability is unknown.\n"+
			"fix: check the Peasant data directory and database, then run `peasant kickstart` again.", err)
}

func kickstartStoreReadError(mode config.SelectionMode, err error) error {
	if mode == config.SelectionModeAll {
		return fmt.Errorf(
			"prepare legacy project selection: %w.\n"+
				"what: Peasant could not read the stored sessions needed to build exact project choices.\n"+
				"why: the local session store query failed.\n"+
				"where: runKickstartFlow, before the selection screen opened.\n"+
				"when: while converting the saved all-projects setting.\n"+
				"meaning: Peasant did not change the saved setting or start setup.\n"+
				"fix: check that the Peasant data directory is readable, then run `peasant kickstart` again.", err)
	}
	return fmt.Errorf(
		"prepare saved project availability: %w.\n"+
			"what: Peasant could not read the stored sessions needed to verify the saved project choice.\n"+
			"why: the local session store query failed.\n"+
			"where: runKickstartFlow, before the selection screen opened.\n"+
			"when: while aligning the no-project save check with the web viewer and push chooser.\n"+
			"meaning: Peasant did not change the saved setting or start ingest because project availability is unknown.\n"+
			"fix: run `peasant ingest verify`, repair the local session store, then run `peasant kickstart` again.", err)
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
// conversion and the save gate. The compatibility wrapper above keeps callers
// that only need preview reads best-effort, while runKickstartFlow rejects an
// unreadable existing store instead of mistaking unavailable evidence for an
// empty store.
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
// listing, the local store, and the harness transcripts discovery found: the
// highlighted session is named from the same listing the tree was folded from,
// and its body is the WHOLE transcript.
//
// The store is the first source, because it holds the transcript Peasant
// already imported and indexed. A session the store does not hold falls back to
// the transcript its harness wrote, which discovery located before any import.
// The user therefore previews every discovered session, including on a first
// run with no database at all. The fallback reads the transcript in place. It
// copies nothing, and it writes nothing to disk or to the database.
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
func kickstartPreview(
	cmd *cobra.Command,
	db *store.Store,
	th theme.Theme,
	sessions []ftue.SessionListing,
	contexts ...kickstart.ListingPreviewContextSource,
) kit.BodySource {
	ctx := cmd.Context()
	// storedTurns reports the turns AND whether the store holds the session at
	// all, so the two outcomes stay distinguishable: a session the store never
	// imported falls back to its harness transcript, while a store that could
	// not be read is a real failure the pane must surface. Collapsing both into
	// "no turns" would report a broken database as an empty session.
	var storedTurns func(sessionID string) ([]ingest.Turn, bool, error)
	if db != nil {
		provider := api.NewStoreDataProvider(db, sessionvisibility.All())
		storedTurns = func(sessionID string) ([]ingest.Turn, bool, error) {
			row, err := db.SessionDetailByID(ctx, sessionID)
			if err != nil {
				return nil, false, err
			}
			if row == nil {
				return nil, false, nil
			}
			session, err := provider.SessionByID(ctx, sessionID)
			if err != nil {
				return nil, true, err
			}
			return session.Turns, true, nil
		}
	}
	// The reader materializes a SQLite-discovered session through the same
	// adapter dependencies the discovery path uses, so the preview reads one
	// session's rows rather than the whole provider database.
	sourceTurns := kickstart.NewSourceTurns(&ingest.OSFileSystem{}, sessions,
		kickstart.WithSourceTurnsGitResolver(&ingest.ExecGitResolver{}),
		kickstart.WithSourceTurnsSalt(salt.Salt{}))
	turns := kickstart.SessionTurnsFunc(func(sessionID string) ([]ingest.Turn, error) {
		if storedTurns != nil {
			recorded, held, err := storedTurns(sessionID)
			if err != nil {
				return nil, err
			}
			if len(recorded) > 0 {
				return recorded, nil
			}
			if held {
				// The store holds the session but produced no turns. That state
				// has its own explanation, with the raw source records, so the
				// harness transcript must not overwrite it here.
				return nil, nil
			}
		}
		return sourceTurns.Turns(sessionID)
	})
	// The notice reports what the bounded harness read left out. A session the
	// store already answered has no harness read behind it, so its notice is
	// empty and the pane shows the stored turns alone.
	opts := []kickstart.ListingPreviewOption{kickstart.WithSessionPreviewNotice(sourceTurns.Notice)}
	if db != nil {
		opts = append(opts, kickstart.WithEmptySessionBody(kickstartImportedEmptySessionBody(ctx, db)))
	}
	if len(contexts) > 0 && contexts[0] != nil {
		opts = append(opts, kickstart.WithListingPreviewContextSource(contexts[0]))
	}
	return kickstart.NewListingPreview(th, sessions, turns, opts...)
}

// kickstartImportedEmptySessionBody gives an already stored session with no
// renderable turns an honest preview. Source records are bounded so a malformed
// or unusually large transcript cannot monopolize the interactive preview.
func kickstartImportedEmptySessionBody(ctx context.Context, db *store.Store) kickstart.EmptySessionBodyFunc {
	return func(sessionID string) (kickstart.EmptySessionPreview, bool, error) {
		info, err := db.SessionSourceInfo(ctx, sessionID)
		if err != nil {
			return kickstart.EmptySessionPreview{}, false, fmt.Errorf("read source information for imported session %q: %w", sessionID, err)
		}
		const noTurns = "imported, but no renderable transcript turns were produced."
		if info == nil {
			return kickstart.EmptySessionPreview{}, false, nil
		}
		if info.SourcePath == "" {
			return kickstart.EmptySessionPreview{Note: noTurns + "\n\nraw source is unavailable."}, true, nil
		}

		file, err := os.Open(info.SourcePath)
		if err != nil {
			return kickstart.EmptySessionPreview{Note: noTurns + "\n\nraw source is unavailable."}, true, nil
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, kickstartRawSourcePreviewLimit+1))
		if err != nil {
			return kickstart.EmptySessionPreview{}, false, fmt.Errorf("read source for imported session %q: %w", sessionID, err)
		}
		truncated := len(data) > kickstartRawSourcePreviewLimit
		if truncated {
			data = data[:kickstartRawSourcePreviewLimit]
			if newline := bytes.LastIndexByte(data, '\n'); newline >= 0 {
				data = data[:newline]
			}
		}

		var records []string
		for _, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var pretty bytes.Buffer
			if json.Indent(&pretty, line, "", "  ") == nil {
				records = append(records, pretty.String())
			}
		}
		if len(records) == 0 {
			return kickstart.EmptySessionPreview{Note: noTurns + "\n\nraw source records are unavailable or malformed."}, true, nil
		}

		note := noTurns + "\n\nraw source records:"
		if truncated {
			note += "\n\nsource preview truncated."
		}
		return kickstart.EmptySessionPreview{Note: note, SourceJSON: strings.Join(records, "\n")}, true, nil
	}
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
func villageAlreadyConnected(configDir string) bool {
	creds, err := auth.LoadCredentialsFrom(configDir)
	return err == nil && creds != nil && creds.IsValid()
}

// kickstartLoginFunc adapts the existing auth.Login runner to the program's
// LoginFunc seam, resolving the village URL and credential store exactly as
// `peasant login` does.
func kickstartLoginFunc(cmd *cobra.Command, configPath string) kickstart.LoginFunc {
	configDir := configDirOverride(cmd)
	return func(ctx context.Context, onURL func(string)) (string, error) {
		villageURL := os.Getenv("PEASANT_VILLAGE_URL")
		if villageURL == "" {
			if cfg, err := loadConfig(configPath); err == nil && cfg.Village.URL != "" {
				villageURL = cfg.Village.URL
			}
		}
		if villageURL == "" {
			villageURL = defaults.DefaultVillageURL.String()
		}
		creds, err := auth.LoginFrom(ctx, villageURL, false, configDir, onURL)
		if err != nil {
			return "", err
		}
		return creds.Username, nil
	}
}

// kickstartLocalIngest builds the post-commit ingest step and returns the exact
// concurrent progress state populated by that runner. It reuses the existing
// ftue ingest runner core and the canonical selection matcher:
// after the flow saves config.Selection, this reloads the config, derives which
// discovered sessions the saved selection admits via ingest.SelectionMatcher (the
// same matcher ingest, push, discovery, and prune use), and hands them to the
// existing runner. Mode:all imports everything; mode:selected imports exactly the
// admitted sessions.
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

func kickstartLocalIngest(cmd *cobra.Command, configPath string, sessions []ftue.SessionListing) (kickstart.IngestFunc, kickstart.ProgressSource) {
	runner, progress := buildFTUEIngestRunnerWithProgress(cmd, configPath)
	return kickstartIngestFuncWithRunner(configPath, sessions, ingest.NewPhysicalPathResolver(), runner), progress
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
		return ftue.WizardAnswers{SelectionMode: cfg.Selection.Mode, ProviderSelections: provs}
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
		SelectionMode:      cfg.Selection.Mode,
		ProviderSelections: provs,
		SelectedSessions:   selected,
	}
}
