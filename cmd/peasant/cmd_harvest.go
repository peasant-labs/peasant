package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/animation"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// checkIngestRedactionLevel refuses an import whose configured redaction level
// this version cannot apply.
//
// Importing under an unsupported level is refused rather than silently downgraded
// because the stored transcript is what every later publish is built from: a user
// who believes their code identifiers were anonymised at import time would
// otherwise accumulate a store that was never protected, and find out only if
// they read the published result.
//
// No supported level redacts while ingest writes. That is the deferred redaction
// model: transcripts are stored as recorded, and redaction happens on the way
// out. Only the removed maximum level ever redacted at import - which is also why
// re-introducing a level that does needs the host slug written to the directory,
// the database row, and the metadata file to be made consistent first, since
// redacting the slug while the directory keeps the real one is what made affected
// repositories permanently unpublishable.
//
// Both the harvest command and the kickstart wizard check here, so there is one
// place that decides and one place a future level has to be taught about.
func checkIngestRedactionLevel(cfg *config.Config, cfgPath, operation string) error {
	if config.RedactionLevelSupported(cfg.Redaction.Level) {
		return nil
	}
	return &config.UnsupportedRedactionLevelError{
		Level:     cfg.Redaction.Level,
		Source:    configSourceDescription(cfgPath),
		Operation: operation,
		Step:      "before any transcript was read or written",
		Impact:    "Nothing was imported and nothing already imported was changed.",
	}
}

// harvestMode controls which pipeline stages are executed.
type harvestMode int

const (
	// harvestAll runs the full pipeline: logs + index (default).
	harvestAll harvestMode = iota
	// harvestLogsOnly extracts transcripts to peasant-sync/ without DB operations.
	harvestLogsOnly
	// harvestIndexOnly populates the DB from existing peasant-sync/ files.
	harvestIndexOnly
)

// harvestFlags holds all CLI flags shared across harvest subcommands.
type harvestFlags struct {
	sourceProvider string
	sourcePath     string
	outputPath     string
	dryRun         bool
	force          bool
	all            bool
	includeActive  bool
	verbose        bool
	debug          bool
	jsonOutput     bool
	detectCommits  bool
	profileIndex   bool
	sessionIDs     []string
	since          string
}

// BuildHarvestCommand constructs the harvest command with logs/index subcommands.
// "ingest" is registered as a silent alias for backward compatibility.
func BuildHarvestCommand() *cobra.Command {
	var flags harvestFlags

	cmd := &cobra.Command{
		Use:     "harvest",
		Aliases: []string{"ingest"},
		Short:   "Harvest AI coding agent transcripts",
		Long:    "Discover, normalize, and store AI coding agent transcripts from Claude Code, OpenCode, Codex, Cursor, and Strike.\nUse 'harvest logs' for file extraction only, or 'harvest index' for DB population only.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHarvest(cmd, harvestAll, &flags)
		},
	}

	registerHarvestFlags(cmd, &flags, harvestAll)

	// Subcommand: harvest logs — file extraction only (no DB).
	logsCmd := &cobra.Command{
		Use:   "logs",
		Short: "Extract transcripts to peasant-sync/ (no database)",
		Long:  "Discover and copy AI agent transcripts to the local peasant-sync/ directory without populating the analytics database.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHarvest(cmd, harvestLogsOnly, &flags)
		},
	}
	registerHarvestFlags(logsCmd, &flags, harvestLogsOnly)
	cmd.AddCommand(logsCmd)

	// Subcommand: harvest index — DB population from existing files.
	indexCmd := &cobra.Command{
		Use:   "index",
		Short: "Populate database from existing peasant-sync/ files",
		Long:  "Read transcripts already in peasant-sync/ and populate the SQLite analytics database (indexing, metrics, annotations).",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHarvest(cmd, harvestIndexOnly, &flags)
		},
	}
	registerHarvestFlags(indexCmd, &flags, harvestIndexOnly)
	cmd.AddCommand(indexCmd)

	// Subcommand: harvest verify — checks database schema integrity.
	var verifyVerbose bool
	verifyCmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify database schema integrity",
		Long:  "Checks that the SQLite database has the expected schema (all tables and key columns).",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, verifyVerbose)
		},
	}
	verifyCmd.Flags().BoolVar(&verifyVerbose, "verbose", false, "Show sample data from each table")
	cmd.AddCommand(verifyCmd)

	return cmd
}

// registerHarvestFlags registers the appropriate flags for the given harvest mode.
func registerHarvestFlags(cmd *cobra.Command, flags *harvestFlags, mode harvestMode) {
	// Common flags for all modes.
	cmd.Flags().BoolVar(&flags.dryRun, "dry-run", false, "Show what would be processed without writing")
	cmd.Flags().BoolVar(&flags.force, "force", false, "Force re-process sessions that match current filters")
	cmd.Flags().BoolVar(&flags.all, "all", false, "Process ALL sessions (clears filters, implies --force)")
	cmd.Flags().BoolVar(&flags.verbose, "verbose", false, "Show file-level detail")
	cmd.Flags().BoolVar(&flags.debug, "debug", false, "Show debug-level logging")
	_ = cmd.Flags().MarkHidden("debug")
	cmd.Flags().BoolVar(&flags.jsonOutput, defaults.JSONFlagName, false, "Output as JSON instead of human-readable")
	cmd.Flags().StringSliceVar(&flags.sessionIDs, "session", nil, "Filter to specific session IDs (repeatable, comma-separated)")
	cmd.Flags().StringVar(&flags.since, "since", "", "Filter to sessions from the last N period (e.g. 2w, 3m, 7d)")

	// Source/output flags (relevant for logs and all modes).
	if mode != harvestIndexOnly {
		cmd.Flags().StringVar(&flags.sourceProvider, "source-provider", "", "Override source provider (claude-code, opencode, codex, cursor, strike)")
		cmd.Flags().StringVar(&flags.sourcePath, "source-path", "", "Override source paths for the provider (replaces config, not additive)")
		cmd.Flags().StringVar(&flags.outputPath, "output", "", "Override output base path")
		cmd.Flags().BoolVar(&flags.includeActive, "include-active", false, "Also process sessions still being written")
	}

	// Detect-commits flag (relevant for index and all modes).
	if mode != harvestLogsOnly {
		cmd.Flags().BoolVar(&flags.detectCommits, "detect-commits", false, "Detect and store git commits linked to each session")
		cmd.Flags().BoolVar(&flags.profileIndex, "profile-index", false, "Print INDEX parse/write timing diagnostics")
	}
}

// BuildIngestCommand returns BuildHarvestCommand for backward compatibility.
// The "ingest" alias is handled by cobra's Aliases field.
var BuildIngestCommand = BuildHarvestCommand

// runHarvest is the shared implementation for all harvest modes.
// IndexOutcomeEndedIndexed reports whether an outcome means the session came out
// of the INDEX stage with its entries.
//
// It is exported so the corpus that drives countIndexFailures classifies outcomes
// through the SAME function production does. It was written twice - once here as
// a switch, once in the test as its own list - and nothing bound the two, so the
// corpus agreed with production only by coincidence: it used two of the five
// outcomes, and deleting the other two success arms from production was green.
//
// Reindexed and Fallback are successes and are reachable: `--reindex` records
// both. Skipped is deliberately NOT a success - the session ended without
// entries, it simply ended that way without an error - so it neither clears a
// failure nor counts as one.
func IndexOutcomeEndedIndexed(outcome ingest.IndexOutcome) bool {
	switch outcome {
	case ingest.IndexOutcomeIndexed, ingest.IndexOutcomeReindexed, ingest.IndexOutcomeFallback:
		return true
	}
	return false
}

// countIndexFailures counts SESSIONS the INDEX stage could not index, not log
// rows.
//
// These are not pipeline errors: the import succeeded and the session is stored.
// What failed is the step that makes it readable, so a run reporting only
// "0 errors" is telling the truth about the wrong thing.
//
// One session can produce SEVERAL log rows in one run - the drain loop and the
// stale-index sweep each record an attempt - so counting rows over-reports.
// A single session can otherwise be reported as multiple import failures when
// both indexing paths record a failed attempt. The warning count must agree with
// the session summary rather than count implementation-level log rows.
//
// A session that fails one attempt and SUCCEEDS on another is not a failure at
// all, so a later success clears an earlier error. That sequence is reachable:
// the drain loop can lose what the sweep then resolves.
func countIndexFailures(log []ingest.IndexLogEntry) int {
	// Both maps are allocated unconditionally, and the failure map is lazy only
	// because writing to a nil map panics - not as an optimisation. An earlier
	// comment here claimed the common case allocates nothing, directly above a map
	// allocated on every call and populated on every clean run.
	var failed map[ingest.SessionID]bool
	succeeded := map[ingest.SessionID]bool{}
	for _, entry := range log {
		switch {
		case entry.Outcome == ingest.IndexOutcomeError:
			if failed == nil {
				failed = map[ingest.SessionID]bool{}
			}
			failed[entry.SessionID] = true
		case IndexOutcomeEndedIndexed(entry.Outcome):
			succeeded[entry.SessionID] = true
		}
	}
	count := 0
	for session := range failed {
		if !succeeded[session] {
			count++
		}
	}
	return count
}

func runHarvest(cmd *cobra.Command, mode harvestMode, flags *harvestFlags) error {
	ctx := cmd.Context()
	// resolveConfigPath, not the raw flag: reading --config directly returns its
	// default when unset, so --config-dir was ignored and this command read a
	// DIFFERENT configuration than the one the user pointed at - including its
	// redaction level, which this command is one of the surfaces that must refuse.
	configPath := resolveConfigPath(cmd)

	// 1. Construct real dependencies.
	fs := &ingest.OSFileSystem{}
	git := &ingest.ExecGitResolver{}

	// 2. Load config.
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Refuse before touching anything, including in --dry-run: a dry run that
	// reported what it would import would be describing an import this version
	// will not perform.
	if levelErr := checkIngestRedactionLevel(cfg, configPath, "peasant "+cmd.Name()); levelErr != nil {
		cmd.SilenceUsage = true
		return levelErr
	}

	// Notify the user when no config file exists.
	if _, statErr := os.Stat(configPath); os.IsNotExist(statErr) {
		fmt.Fprintf(os.Stderr, "notice: no config found at %s — using defaults. Run 'peasant kickstart' to configure.\n", configPath)
	}

	// 3. Apply CLI flag overrides (source flags only for logs/all modes).
	if mode != harvestIndexOnly {
		if flags.sourceProvider != "" || flags.sourcePath != "" {
			if flags.sourceProvider == "" {
				return fmt.Errorf("--source-path requires --source-provider")
			}
			if flags.sourcePath == "" {
				return fmt.Errorf("--source-provider requires --source-path")
			}
			provider, err := resolveHarnessFlag(flags.sourceProvider)
			if err != nil {
				return err
			}
			resolved, err := ingest.NewResolvedPath(flags.sourcePath)
			if err != nil {
				return fmt.Errorf("resolve source path: %w", err)
			}
			applySourceOverride(cfg, provider, resolved)
			// --source-path (which requires --source-provider) scopes the run to
			// the NAMED provider as the SOLE active source: disable default
			// discovery of the OTHER providers so "ingest from THIS path" does not
			// also read their real default dirs (~/.claude, opencode, codex) — the
			// isolation leak exposed by the source-scoped integration path.
			isolateSourceProvider(cfg, provider)
		}
	}

	outputDir := cfg.Output.BasePath
	if flags.outputPath != "" {
		outputDir = flags.outputPath
	}
	if outputDir == "" {
		return fmt.Errorf("no output path configured: set output.basePath in %s or use --output flag", defaults.Config.FileName)
	}
	resolvedOutput, err := ingest.NewResolvedPath(outputDir)
	if err != nil {
		return fmt.Errorf("resolve output path %q: %w", outputDir, err)
	}

	// 4. Build adapter registry.
	adapters := map[defaults.Harness]ingest.AdapterFactory{
		defaults.HarnessClaudeCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewClaudeAdapter(f, g, s)
		},
		defaults.HarnessOpenCode: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewOpenCodeAdapter(f, g, s)
		},
		defaults.HarnessCodex: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewCodexAdapter(f, g, s)
		},
		defaults.HarnessCursor: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewCursorAdapter(f, g, s)
		},
		defaults.HarnessStrike: func(f ingest.FileSystem, g ingest.GitResolver, s salt.Salt) ingest.SourceAdapter {
			return ingest.NewStrikeAdapter(f, g, s)
		},
	}

	// 5. Build source configs from config.
	sources := buildSourceConfigs(cfg)

	// 6. Build pipeline config.
	staleness := time.Duration(cfg.Output.StalenessThresholdSec) * time.Second
	if staleness == 0 {
		staleness = time.Duration(defaults.ConfigStalenessThresholdSec) * time.Second
	}

	// --all implies --force and --include-active, and clears all session filters.
	force := flags.force || flags.all
	if flags.all {
		flags.includeActive = true
	}

	progState := ingest.NewProgressState()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	renderer := newProgressRenderer(os.Stderr, progState, animation.IngestAnimation())
	go renderer.Run(ctx)

	if renderer.IsTTY() {
		orig := slog.Default().Handler()
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		defer slog.SetDefault(slog.New(orig))
	}

	// For index-only mode, use the existing Reindex code path.
	reindex := mode == harvestIndexOnly

	var indexProfiler *ingest.IndexProfiler
	if flags.profileIndex {
		indexProfiler = &ingest.IndexProfiler{}
	}

	pipelineCfg := ingest.PipelineConfig{
		Sources:            sources,
		OutputDir:          resolvedOutput,
		Force:              force,
		IncludeActive:      flags.includeActive,
		StalenessThreshold: staleness,
		DryRun:             flags.dryRun,
		Reindex:            reindex,
		Parallelism:        0, // 0 = auto (runtime.NumCPU())
		IndexProfiler:      indexProfiler,
		Progress:           progState,
	}

	// 6b. Wire --session flag into AllowedSessionIDs (unless --all clears filters).
	if !flags.all && len(flags.sessionIDs) > 0 {
		allowed := make(map[ingest.SessionID]bool, len(flags.sessionIDs))
		for _, raw := range flags.sessionIDs {
			sid, err := ingest.NewSessionID(raw)
			if err != nil {
				return fmt.Errorf("invalid session ID %q: %w", raw, err)
			}
			allowed[sid] = true
		}
		pipelineCfg.AllowedSessionIDs = allowed
	}

	// 6c. Wire --since flag into Since (unless --all clears filters).
	if !flags.all && flags.since != "" {
		cutoff, err := parseSinceDuration(flags.since)
		if err != nil {
			return err
		}
		pipelineCfg.Since = &cutoff
	}

	// 6d. Wire selection index into SessionFilter (unless --all clears filters).
	var selectionConflicts *selectionConflictRecorder
	if !flags.all && len(flags.sessionIDs) == 0 && cfg.Selection.Mode == config.SelectionModeSelected {
		selectionFilter, recorder := buildSelectionFilterWithRecorder(cfg, git)
		pipelineCfg.PrepareSessionFilter = selectionFilter.Prepare
		pipelineCfg.SessionFilter = selectionFilter.Match
		pipelineCfg.SessionExclusionFilter = selectionFilter.Excludes
		selectionConflicts = recorder
	}

	// 7. Count custom patterns before the dry-run branch (visible in both modes).
	customPatternCount := len(cfg.Redaction.CustomPatterns)

	// 8. Build pipeline options based on harvest mode.
	var pipelineOpts []ingest.PipelineOption

	// Logs-only mode skips all DB operations.
	skipDB := mode == harvestLogsOnly

	if !skipDB {
		needsDB := !flags.dryRun || reindex
		dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
		dbExists := func() bool { _, err := os.Stat(dbPath); return err == nil }
		if needsDB || dbExists() {
			dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
			if !flags.dryRun {
				if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
					return fmt.Errorf("create data directory: %w", err)
				}
			}
			db, err := store.Open(dbPath)
			if err != nil {
				if !flags.dryRun {
					return fmt.Errorf("open analytics store: %w", err)
				}
			} else {
				defer db.Close()

				pipelineOpts = append(pipelineOpts,
					ingest.WithStore(db),
					ingest.WithMetricsStore(db),
				)

				installSalt, _, saltErr := salt.Load(db.Pool())
				if saltErr != nil {
					slog.Warn("salt.Load failed; using zero salt for project hashes (non-fatal)",
						"err", saltErr,
					)
				} else {
					pipelineOpts = append(pipelineOpts, ingest.WithSalt(installSalt))
				}

				if !flags.dryRun {
					pipelineOpts = append(pipelineOpts,
						ingest.WithIndexers(ingest.NewIndexerRegistry(fs, ingest.IndexerRegistryOptions{})),
						ingest.WithAnalyzer(metrics.NewEngineWithModels(db, db)),
						ingest.WithClassifier(metrics.NewClassifierAnnotator(db, db)),
						ingest.WithLogger(db),
						ingest.WithIndexLogger(db),
					)
				}
			}
		}
	}

	// Inject git diff analyzer when --detect-commits is set (not for logs-only).
	if !skipDB && flags.detectCommits {
		pipelineOpts = append(pipelineOpts, ingest.WithGitDiffAnalyzer(ingest.NewExecGitDiffAnalyzer()))
	}

	// 9. Create and run pipeline.
	pipeline, err := ingest.NewPipeline(fs, git, adapters, pipelineCfg, pipelineOpts...)
	if err != nil {
		return fmt.Errorf("create pipeline: %w", err)
	}
	result, err := pipeline.Run(ctx)
	cancel()
	renderer.Wait()
	renderer.Clear()
	if err != nil {
		return fmt.Errorf("pipeline failed: %w", err)
	}
	if selectionConflicts != nil {
		selectionConflicts.notice(cmd.ErrOrStderr(), configPath)
	}

	// 10. Output results.
	if result.Summary.StoreError != nil {
		fmt.Fprintf(os.Stderr, "warning: %v\n", result.Summary.StoreError)
	}
	for _, diagnostic := range result.DiscoveryDiagnostics {
		fmt.Fprintf(os.Stderr, "warning: %s discovery skipped %s: %s\n", string(diagnostic.Provider), diagnostic.Location, diagnostic.Summary)
	}
	if indexProfiler != nil {
		printIndexProfile(os.Stderr, indexProfiler.Snapshot())
	}
	if flags.jsonOutput {
		return printJSON(cmd.OutOrStdout(), result)
	}
	printSummary(cmd.OutOrStdout(), result, flags.verbose, flags.includeActive, string(resolvedOutput), configPath, sources, customPatternCount)

	// 11. Exit code: 1 if any errors occurred during ingestion.
	if result.Summary.Errors > 0 {
		return fmt.Errorf("%d session(s) failed", result.Summary.Errors)
	}
	return nil
}

func printIndexProfile(w io.Writer, profile ingest.IndexProfileSnapshot) {
	if len(profile.Batches) == 0 {
		fmt.Fprintln(w, "INDEX profile: no INDEX batches ran")
		return
	}
	sizeCounts := map[int]int{}
	totalSessions := 0
	totalEntries := 0
	totalBytes := int64(0)
	totalParse := time.Duration(0)
	totalWrite := time.Duration(0)
	maxWorkers := 0
	for _, batch := range profile.Batches {
		sizeCounts[batch.Sessions]++
		totalSessions += batch.Sessions
		totalEntries += batch.Entries
		totalBytes += batch.Bytes
		totalParse += batch.ParseDuration
		totalWrite += batch.WriteDuration
		if batch.MaxParseWorkers > maxWorkers {
			maxWorkers = batch.MaxParseWorkers
		}
	}
	sizes := make([]int, 0, len(sizeCounts))
	for size := range sizeCounts {
		sizes = append(sizes, size)
	}
	sort.Ints(sizes)
	dist := make([]string, 0, len(sizes))
	for _, size := range sizes {
		dist = append(dist, fmt.Sprintf("%dx%d", size, sizeCounts[size]))
	}

	fmt.Fprintf(w, "INDEX profile: %d batches, %d sessions, %d entries, %d bytes\n", len(profile.Batches), totalSessions, totalEntries, totalBytes)
	fmt.Fprintf(w, "  batch sizes: %s\n", strings.Join(dist, ", "))
	fmt.Fprintf(w, "  parse: %s total; write: %s total; max parse workers: %d\n", totalParse.Round(time.Millisecond), totalWrite.Round(time.Millisecond), maxWorkers)
	if len(profile.SlowSessions) == 0 {
		return
	}
	fmt.Fprintln(w, "  slow sessions:")
	for _, session := range profile.SlowSessions {
		fmt.Fprintf(w, "    %s total (parse %s, write %s) %s %s entries=%d bytes=%d outcome=%s path=%s\n",
			session.TotalDuration().Round(time.Millisecond),
			session.ParseDuration.Round(time.Millisecond),
			session.WriteDuration.Round(time.Millisecond),
			session.Harness,
			session.SessionID,
			session.Entries,
			session.Bytes,
			session.Outcome,
			session.SourcePath,
		)
	}
}

// runVerify checks database schema integrity.
func runVerify(cmd *cobra.Command, verbose bool) error {
	ctx := cmd.Context()
	dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))

	db, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer db.Close()

	out := cmd.OutOrStdout()

	// Header
	fmt.Fprintln(out, "==============================================")
	fmt.Fprintln(out, "Peasant Database Verification")
	fmt.Fprintln(out, "==============================================")
	fmt.Fprintf(out, "Database: %s\n\n", dbPath)

	// 1. Schema Overview
	fmt.Fprintln(out, "1. Schema Overview")
	fmt.Fprintln(out, "----------------------------------------------")

	// Get list of existing tables
	existingTables, err := store.AllTableNamesFromDB(db)
	if err != nil {
		return fmt.Errorf("get table names: %w", err)
	}

	// Expected tables from schema.go
	expectedTables := store.AllTableNames
	schemaVersion := "v1-v20 (migrations)"

	fmt.Fprintf(out, "Expected schema: %s\n", schemaVersion)
	fmt.Fprintf(out, "Expected tables: %d\n", len(expectedTables))
	fmt.Fprintf(out, "Found tables:    %d\n", len(existingTables))

	// 2. Table Presence Check
	fmt.Fprintln(out, "\n2. Table Presence Check")
	fmt.Fprintln(out, "----------------------------------------------")

	// Check for missing tables
	var missingTables []string
	for _, t := range expectedTables {
		if slices.Contains(existingTables, t) {
			fmt.Fprintf(out, "[OK]   %s\n", t)
		} else {
			fmt.Fprintf(out, "[FAIL] %s - MISSING\n", t)
			missingTables = append(missingTables, t)
		}
	}

	// 3. Result Summary
	fmt.Fprintln(out, "\n3. Result Summary")
	fmt.Fprintln(out, "----------------------------------------------")

	if len(missingTables) > 0 {
		fmt.Fprintf(out, "Status: FAILED\n")
		fmt.Fprintf(out, "Missing tables: %d\n", len(missingTables))
		for _, t := range missingTables {
			fmt.Fprintf(out, "  - %s\n", t)
		}
		return fmt.Errorf("database verification failed: %d missing table(s)", len(missingTables))
	}

	fmt.Fprintln(out, "Status: PASSED")
	fmt.Fprintln(out, "All expected tables are present.")

	// 4. Table Statistics
	fmt.Fprintln(out, "\n4. Table Statistics")
	fmt.Fprintln(out, "----------------------------------------------")
	fmt.Fprintln(out, "(showing only tables with data; session_commits always shown)")
	fmt.Fprintln(out, "")

	totalRows := int64(0)
	commitCount := int64(-1) // -1 = not yet fetched
	for _, t := range expectedTables {
		count, err := store.TableRowCount(db, t)
		if err != nil {
			continue
		}
		if t == "session_commits" {
			commitCount = count // shown separately below
			totalRows += count
			continue
		}
		if count > 0 {
			fmt.Fprintf(out, "%-30s %10d rows\n", t+":", count)
			totalRows += count
		}
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "%-30s %10d rows\n", "TOTAL:", totalRows)

	// Always show session_commits count so users can verify --detect-commits worked.
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Commit Detection:")
	if commitCount < 0 {
		fmt.Fprintln(out, "  session_commits:           (error reading row count)")
	} else if commitCount == 0 {
		fmt.Fprintln(out, "  session_commits:                    0 rows")
		fmt.Fprintln(out, "  (run 'peasant harvest --detect-commits' to populate)")
	} else {
		fmt.Fprintf(out, "  %-28s %10d rows\n", "session_commits:", commitCount)
	}

	// Annotation Engine subsection (within section 4, always shown).
	if err := runAnnotationEngineSection(ctx, db, out, verbose); err != nil {
		// Non-fatal: show error inline so the rest of verify output is preserved.
		fmt.Fprintf(out, "\n  (annotation engine error: %v)\n", err)
	}

	// 5. Sample Data (verbose only)
	if verbose {
		fmt.Fprintln(out, "\n5. Sample Data (--verbose)")
		fmt.Fprintln(out, "----------------------------------------------")
		fmt.Fprintln(out, "(first 5 rows per table, limited columns)")
		fmt.Fprintln(out, "")

		// For each table with data, show sample (excluding session_commits — shown separately).
		for _, t := range expectedTables {
			if t == "session_commits" {
				continue
			}
			count, err := store.TableRowCount(db, t)
			if err == nil && count > 0 {
				samples, err := store.TableSelectLimit(db, t, 5)
				if err == nil && len(samples) > 0 {
					fmt.Fprintf(out, "--- %s (first %d rows) ---\n", t, len(samples))
					for _, row := range samples {
						fmt.Fprintf(out, "  %s\n", row)
					}
					fmt.Fprintln(out, "")
				}
			}
		}

		// Always show session_commits block so its status is visible in --verbose.
		fmt.Fprintln(out, "--- session_commits (commit detection) ---")
		if commitCount <= 0 {
			fmt.Fprintln(out, "  (empty — run 'peasant harvest --detect-commits' to populate)")
		} else {
			samples, err := store.TableSelectLimit(db, "session_commits", 5)
			if err == nil && len(samples) > 0 {
				for _, row := range samples {
					fmt.Fprintf(out, "  %s\n", row)
				}
			}
		}
		fmt.Fprintln(out, "")
	}

	return nil
}

// runAnnotationEngineSection prints the "Annotation Engine" subsection within
// section 4 (Table Statistics). It mirrors the visual style of the "Commit Detection"
// subsection. In verbose mode it also shows per-type and per-annotator details.
func runAnnotationEngineSection(ctx context.Context, db *store.Store, out io.Writer, verbose bool) error {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Annotation Engine:")

	// Use ListAnnotationTypes to derive per-status counts and verify seed count.
	allTypes, err := db.ListAnnotationTypes(ctx, "", "")
	if err != nil {
		fmt.Fprintf(out, "  annotation_types:          (error: %v)\n", err)
	} else {
		statusCounts := make(map[string]int)
		for _, t := range allTypes {
			statusCounts[string(t.Status)]++
		}
		okOrFail := func(ok bool) string {
			if ok {
				return "[OK]"
			}
			return "[FAIL]"
		}
		// Seed floor: 11 types ship in migrations (8 pre-V35 quality/research/metadata
		// types + the V35 user.custom_label free-text type + the V39
		// quality.turn_outcome/quality.turn_flag pair). Fewer means a seed
		// migration failed to apply.
		fmt.Fprintf(out, "  annotation_types:          %d  %s\n", len(allTypes), okOrFail(len(allTypes) >= 11))
		for _, status := range []string{"active", "proposed", "deprecated", "retired"} {
			if n := statusCounts[status]; n > 0 {
				fmt.Fprintf(out, "    %-12s %d types\n", status+":", n)
			}
		}

		// Seed counts for annotators and deps.
		annCount, _ := store.TableRowCount(db, "annotators")
		depCount, _ := store.TableRowCount(db, "annotation_type_deps")
		fmt.Fprintf(out, "  annotators:                %d  %s\n", annCount, okOrFail(annCount >= 6))
		fmt.Fprintf(out, "  annotation_type_deps:      %d  %s\n", depCount, okOrFail(depCount >= 1))

		// Taxonomy chain check.
		badCount, err := db.TaxonomyChainBadCount(ctx)
		if err != nil {
			fmt.Fprintf(out, "  taxonomy chain:            (error: %v)\n", err)
		} else if badCount == 0 {
			fmt.Fprintln(out, "  taxonomy chain:            [OK]  (all types have valid family\u2192class)")
		} else {
			fmt.Fprintf(out, "  taxonomy chain:            [FAIL] (%d broken)\n", badCount)
		}

		// Annotation count by target kind via VIEW.
		byKind, err := db.AnnotationsByTargetKind(ctx)
		if err != nil {
			fmt.Fprintf(out, "  annotations by kind:       (error: %v)\n", err)
		} else if len(byKind) == 0 {
			fmt.Fprintln(out, "  annotations by kind:       (none yet \u2014 normal for fresh database)")
		} else {
			for kind, count := range byKind {
				fmt.Fprintf(out, "  annotations (%-10s): %d\n", kind, count)
			}
		}
	}

	// Verbose: per-type details (with family/class via JOIN) and per-annotator details.
	if verbose {
		typeDetails, err := db.AnnotationTypeDetails(ctx)
		if err != nil {
			fmt.Fprintf(out, "  (error fetching type details: %v)\n", err)
		} else {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "  Annotation Type Details (--verbose):")
			for _, row := range typeDetails {
				fmt.Fprintf(out, "    %-40s %-12s %s/%s\n", row.TypeID, row.Status, row.Class, row.Family)
			}
		}

		annotators, err := db.ListAnnotators(ctx)
		if err != nil {
			fmt.Fprintf(out, "  (error fetching annotators: %v)\n", err)
		} else {
			fmt.Fprintln(out, "")
			fmt.Fprintln(out, "  Annotators (--verbose):")
			for _, row := range annotators {
				fmt.Fprintf(out, "    %-30s %-10s %s\n", row.Name, string(row.Kind), row.Status)
			}
		}
	}

	return nil
}

// resolveHarnessFlag converts a --source-provider flag value to a typed Harness,
// returning a clear error for unknown values. Legacy harness names that were
// renamed in the bestiary migration get a specific deprecation message.
func resolveHarnessFlag(raw string) (defaults.Harness, error) {
	switch defaults.Harness(raw) {
	case defaults.LegacyHarnessClaude:
		return "", fmt.Errorf(
			"--source-provider=%q is deprecated: the harness was renamed to %q.\n"+
				"  Rerun with: --source-provider=%s",
			raw, defaults.HarnessClaudeCode, defaults.HarnessClaudeCode,
		)
	case defaults.LegacyHarnessGemini:
		return "", fmt.Errorf(
			"--source-provider=%q is deprecated: the harness was renamed to %q.\n"+
				"  Rerun with: --source-provider=%s",
			raw, defaults.HarnessGeminiCLI, defaults.HarnessGeminiCLI,
		)
	}
	h := defaults.Harness(raw)
	if !h.IsKnown() {
		known := defaults.Harnesses()
		names := make([]string, len(known))
		for i, kh := range known {
			names[i] = string(kh)
		}
		return "", fmt.Errorf(
			"unknown harness %q (known: %s)",
			raw, strings.Join(names, ", "),
		)
	}
	// Cursor and antigravity are recognized by bestiary but peasant has no
	// ingester adapter for them yet.
	switch h {
	case defaults.HarnessCursor, defaults.HarnessAntigravity:
		return "", fmt.Errorf(
			"harness %q is recognized by bestiary but peasant has no ingester for it yet (planned for a future release)",
			raw,
		)
	}
	return h, nil
}

// isolateSourceProvider scopes ingestion to a single named provider: it enables
// that provider and DISABLES default discovery of every other provider for this
// run. Used with --source-path so a path-scoped ingest reads ONLY that provider
// from that path, not the other providers' real default source dirs. Bare
// `peasant ingest` (no source flags) is unaffected — it uses the config's enabled
// set as-is; multi-provider mixes remain available via config (sources.*.enabled).
func isolateSourceProvider(cfg *config.Config, provider defaults.Harness) {
	cfg.Sources.ClaudeCode.Enabled = provider == defaults.HarnessClaudeCode
	cfg.Sources.OpenCode.Enabled = provider == defaults.HarnessOpenCode
	cfg.Sources.Codex.Enabled = provider == defaults.HarnessCodex
	cfg.Sources.Cursor.Enabled = provider == defaults.HarnessCursor
	cfg.Sources.Strike.Enabled = provider == defaults.HarnessStrike
}

// applySourceOverride replaces the config paths for a single provider.
// --source-path replaces, rather than appends to, configured paths so an
// explicit command-line source remains isolated from implicit defaults.
func applySourceOverride(cfg *config.Config, provider defaults.Harness, path ingest.ResolvedPath) {
	switch provider {
	case defaults.HarnessClaudeCode:
		cfg.Sources.ClaudeCode.Enabled = true
		cfg.Sources.ClaudeCode.Paths = []string{string(path)}
	case defaults.HarnessOpenCode:
		cfg.Sources.OpenCode.Enabled = true
		cfg.Sources.OpenCode.Paths = []string{string(path)}
	case defaults.HarnessCodex:
		cfg.Sources.Codex.Enabled = true
		cfg.Sources.Codex.Paths = []string{string(path)}
	case defaults.HarnessCursor:
		cfg.Sources.Cursor.Enabled = true
		cfg.Sources.Cursor.Paths = []string{string(path)}
	case defaults.HarnessStrike:
		cfg.Sources.Strike.Enabled = true
		cfg.Sources.Strike.Paths = []string{string(path)}
	}
}

type sourcePathIssue struct {
	path string
	err  error
}

func resolveConfiguredSource(cfg *config.Config, harness defaults.Harness) (ingest.SourceConfig, []sourcePathIssue, bool) {
	configured, ok := cfg.Sources.Provider(harness)
	if !ok {
		return ingest.SourceConfig{}, nil, false
	}

	paths := make([]ingest.ResolvedPath, 0, len(configured.Paths))
	var issues []sourcePathIssue
	for _, path := range configured.Paths {
		resolved, err := ingest.NewResolvedPath(path)
		if err != nil {
			issues = append(issues, sourcePathIssue{path: path, err: err})
			continue
		}
		paths = append(paths, resolved)
	}
	return ingest.SourceConfig{Paths: paths, Enabled: configured.Enabled}, issues, true
}

// buildSourceConfigs converts every registered provider's config into pipeline sources.
func buildSourceConfigs(cfg *config.Config) map[defaults.Harness]ingest.SourceConfig {
	sources := map[defaults.Harness]ingest.SourceConfig{}
	for harness := range ingest.DefaultAdapterRegistry {
		source, issues, ok := resolveConfiguredSource(cfg, harness)
		if !ok || !source.Enabled {
			continue
		}
		for _, issue := range issues {
			fmt.Fprintf(os.Stderr, "warning: skipping invalid %s source path %q: %v\n", harness, issue.path, issue.err)
		}
		sources[harness] = source
	}
	return sources
}

// parseSinceDuration parses a relative duration string like "2w", "3m", "7d"
// and returns the corresponding time.Time cutoff (now - duration).
func parseSinceDuration(s string) (time.Time, error) {
	if len(s) < 2 {
		return time.Time{}, fmt.Errorf("invalid duration: %q (expected format: Nd, Nw, Nm)", s)
	}
	numStr := s[:len(s)-1]
	unit := s[len(s)-1]

	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		return time.Time{}, fmt.Errorf("invalid duration: %q (number must be positive integer)", s)
	}

	now := time.Now()
	switch unit {
	case 'd':
		return now.AddDate(0, 0, -n), nil
	case 'w':
		return now.AddDate(0, 0, -n*7), nil
	case 'm':
		return now.AddDate(0, -n, 0), nil
	default:
		return time.Time{}, fmt.Errorf("invalid duration unit %q in %q (use d/w/m)", string(unit), s)
	}
}

// jsonPipelineResult is the JSON-safe equivalent of ingest.PipelineResult.
// SessionResult.Error (type error) does not serialize; we convert it to a string.
type jsonPipelineResult struct {
	Summary              ingest.PipelineSummary       `json:"summary"`
	Sessions             []jsonSessionResult          `json:"sessions"`
	Duration             string                       `json:"duration"`
	IndexLog             []ingest.IndexLogEntry       `json:"indexLog,omitempty"`
	DiscoveryDiagnostics []ingest.DiscoveryDiagnostic `json:"discoveryDiagnostics,omitempty"`
}

// jsonSessionResult is the JSON-safe equivalent of ingest.SessionResult.
type jsonSessionResult struct {
	SessionID  ingest.SessionID `json:"sessionId"`
	Provider   defaults.Harness `json:"provider"`
	Status     string           `json:"status"`
	OutputPath string           `json:"outputPath,omitempty"`
	Error      string           `json:"error,omitempty"`
}

// printJSON outputs the pipeline result as JSON.
// The error field in SessionResult is serialized as a string to ensure JSON compatibility.
func printJSON(w io.Writer, result *ingest.PipelineResult) error {
	out := jsonPipelineResult{
		Summary:              result.Summary,
		Duration:             result.Duration.Round(100 * time.Millisecond).String(),
		IndexLog:             result.IndexLog,
		DiscoveryDiagnostics: result.DiscoveryDiagnostics,
	}
	for _, sr := range result.Sessions {
		js := jsonSessionResult{
			SessionID:  sr.SessionID,
			Provider:   sr.Harness,
			Status:     sr.Status.String(),
			OutputPath: sr.OutputPath,
		}
		if sr.Error != nil {
			js.Error = sr.Error.Error()
		}
		out.Sessions = append(out.Sessions, js)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// printSummary outputs the human-readable pipeline summary.
//
// Default (no --verbose): path header + summary line + per-session rows with provider and output path.
// --verbose: same rows but subagents are expanded underneath their parent instead of collapsed.
func printSummary(w io.Writer, result *ingest.PipelineResult, verbose bool, includeActive bool, outputDir string, configPath string, sources map[defaults.Harness]ingest.SourceConfig, customPatternCount int) {
	// Path header.
	fmt.Fprintf(w, "Output:  %s\n", outputDir)
	if configPath != "" {
		fmt.Fprintf(w, "Config:  %s\n", configPath)
	} else {
		fmt.Fprintf(w, "Config:  (using defaults)\n")
	}
	fmt.Fprintf(w, "Sources:\n")
	for provider, cfg := range sources {
		if !cfg.Enabled {
			continue
		}
		var paths []string
		for _, p := range cfg.Paths {
			paths = append(paths, string(p))
		}
		fmt.Fprintf(w, "   - %-9s %s\n", string(provider)+":", strings.Join(paths, ", "))
	}
	if customPatternCount > 0 {
		fmt.Fprintf(w, "Custom patterns: %d\n", customPatternCount)
	}

	s := result.Summary
	total := s.New + s.Updated + s.Unchanged + s.Active + s.Errors

	// Distinguish debounced active sessions from explicitly included ones.
	activeLabel := "(debounced)"
	if includeActive {
		activeLabel = "(included)"
	}
	fmt.Fprintf(w, "peasant harvest: %d sessions (%d new, %d updated, %d unchanged, %d errors), %d active %s [%s]\n",
		total,
		s.New, s.Updated, s.Unchanged, s.Errors, s.Active,
		activeLabel,
		result.Duration.Round(100*time.Millisecond))
	// The index line is printed whenever there is anything to say, INCLUDING when
	// nothing was indexed because indexing failed.
	//
	// It used to be gated on Indexed > 0 || Computed > 0, but an index refusal
	// sets neither. Without the failure branch an interactive run exited zero with
	// no index line even though the index log and JSON output recorded the refusal.
	//
	// Index failures are counted from the log rather than from the summary because
	// the summary has no field for them; they are not pipeline Errors, since the
	// import itself succeeded.
	indexFailures := countIndexFailures(result.IndexLog)
	if s.Indexed > 0 || s.Computed > 0 || indexFailures > 0 {
		fmt.Fprintf(w, "  index: %d indexed, %d computed (index_version=%d, metadata_version=%d)\n",
			s.Indexed, s.Computed, s.IndexVersion, s.MetadataVersion)
	}
	if indexFailures > 0 {
		fmt.Fprintf(w, "  warning: %d session(s) were imported but NOT indexed, so they are empty in the viewer, in "+
			"search, in metrics, and in anything published. Run 'peasant harvest --json' for the reason on each.\n",
			indexFailures)
	}

	if len(result.Sessions) == 0 {
		return
	}

	// Build subagent maps using ParentUUID (semantic, not path-based).
	subagentCount := map[ingest.SessionID]int{}
	subagentsByParent := map[ingest.SessionID][]ingest.SessionResult{}
	for _, sr := range result.Sessions {
		if sr.ParentUUID != nil {
			subagentCount[*sr.ParentUUID]++
			subagentsByParent[*sr.ParentUUID] = append(subagentsByParent[*sr.ParentUUID], sr)
		}
	}

	// Collect root sessions (non-subagents) preserving original order.
	var rootSessions []ingest.SessionResult
	for _, sr := range result.Sessions {
		if sr.ParentUUID == nil {
			rootSessions = append(rootSessions, sr)
		}
	}

	// printRoot outputs one root session row and, in verbose mode, expands its subagents.
	printRoot := func(sr ingest.SessionResult) {
		statusStr := sr.Status.String()
		if sr.Error != nil {
			statusStr = "error"
		}

		if verbose {
			// Verbose mode expands subagents beneath each root session.
			if sr.OutputPath != "" {
				fmt.Fprintf(w, "  %-10s %-10s %s  ->  %s\n",
					strings.ToUpper(statusStr),
					string(sr.Harness),
					string(sr.SessionID),
					sr.OutputPath)
			} else {
				fmt.Fprintf(w, "  %-10s %-10s %s\n",
					strings.ToUpper(statusStr),
					string(sr.Harness),
					string(sr.SessionID))
			}
			// Expand each subagent on its own indented line.
			for _, sub := range subagentsByParent[sr.SessionID] {
				if sub.OutputPath != "" {
					fmt.Fprintf(w, "  %-10s %-10s   %s  ->  %s\n",
						"", "",
						string(sub.SessionID),
						sub.OutputPath)
				} else {
					fmt.Fprintf(w, "  %-10s %-10s   %s\n",
						"", "",
						string(sub.SessionID))
				}
			}
		} else {
			// Default mode summarizes subagents beside the provider and output path.
			suffix := ""
			if n := subagentCount[sr.SessionID]; n > 0 {
				suffix = fmt.Sprintf(" + %d subagent(s)", n)
			}
			if sr.OutputPath != "" {
				fmt.Fprintf(w, "  %-10s %-10s %s  ->  %s%s\n",
					strings.ToUpper(statusStr),
					string(sr.Harness),
					string(sr.SessionID),
					sr.OutputPath,
					suffix)
			} else {
				fmt.Fprintf(w, "  %-10s %-10s %s%s\n",
					strings.ToUpper(statusStr),
					string(sr.Harness),
					string(sr.SessionID),
					suffix)
			}
		}
	}

	// When more than 20 root sessions are present, show the first and last 10.
	// --verbose disables truncation and shows all sessions.
	const truncThreshold = 20
	const truncShow = 10
	if verbose || len(rootSessions) <= truncThreshold {
		for _, sr := range rootSessions {
			printRoot(sr)
		}
	} else {
		skipped := len(rootSessions) - 2*truncShow
		for _, sr := range rootSessions[:truncShow] {
			printRoot(sr)
		}
		fmt.Fprintf(w, "  ... and %d more ...\n", skipped)
		for _, sr := range rootSessions[len(rootSessions)-truncShow:] {
			printRoot(sr)
		}
	}
}

type selectionConflict struct {
	session ingest.DiscoveredSession
	branch  string
	// entries are the disagreeing entries reported by the matcher itself, not
	// every entry configured for the harness. Naming the whole harness would
	// grow less actionable the more projects a user configures — exactly
	// backwards from what the disclosure is for.
	entries []ingest.SelectionEntry
}

type selectionConflictRecorder struct {
	conflicts []selectionConflict
}

// entrySeparator joins the disagreeing entries in the withheld-conflict
// warning. It is named because the boundary is load-bearing for a reader: each
// entry's identity has to be findable as one run of text, not assembled from
// pieces of several, so anything asserting about one entry needs the same
// boundary the renderer used.
const entrySeparator = " and "

// describeEntries renders the disagreeing entries by the identity the user
// wrote in their configuration, so the warning names text they can search for.
func describeEntries(entries []ingest.SelectionEntry) string {
	rendered := make([]string, len(entries))
	for i, entry := range entries {
		rendered[i] = entry.String()
	}
	return strings.Join(rendered, entrySeparator)
}

func (r *selectionConflictRecorder) notice(w io.Writer, configPath string) {
	const limit = 10
	for i, conflict := range r.conflicts {
		if i == limit {
			fmt.Fprintf(w, "warning: %d additional sessions were withheld because selection rules conflict; inspect %q and edit one project entry or add the branch\n", len(r.conflicts)-limit, configPath)
			break
		}
		fmt.Fprintf(w, "warning: session %s (%s) was withheld during ingest because project selection entries %s disagree on branch %q in %q; the session was not ingested; edit one entry or add the branch\n", conflict.session.SessionID, conflict.session.Harness, describeEntries(conflict.entries), conflict.branch, configPath)
	}
}

type preparedHarvestSelection struct {
	candidate ingest.DiscoveryCandidate
	decision  ingest.DiscoveryDecision
	excluded  bool
	session   ingest.DiscoveredSession
}

type harvestSelectionFilter struct {
	matcher               ingest.SelectionMatcher
	autoIngestNewBranches bool
	git                   ingest.GitResolver
	pathResolver          ingest.PathIdentityResolver
	projectHarnesses      map[ingest.Harness]bool
	prepared              map[ingest.SessionID]preparedHarvestSelection
	recordedConflicts     map[ingest.SessionID]bool
	recorder              *selectionConflictRecorder
}

func buildSelectionFilterWithRecorder(cfg *config.Config, git ingest.GitResolver) (*harvestSelectionFilter, *selectionConflictRecorder) {
	return buildSelectionFilterWithResolver(cfg, git, ingest.NewPhysicalPathResolver())
}

func buildSelectionFilterWithResolver(
	cfg *config.Config,
	git ingest.GitResolver,
	pathResolver ingest.PathIdentityResolver,
) (*harvestSelectionFilter, *selectionConflictRecorder) {
	recorder := &selectionConflictRecorder{}
	projectHarnesses := make(map[ingest.Harness]bool)
	for harness, selection := range cfg.Selection.Harnesses {
		if len(selection.Projects) > 0 || len(selection.Exclusions.Branches) > 0 {
			projectHarnesses[ingest.Harness(harness)] = true
		}
	}
	filter := &harvestSelectionFilter{
		matcher:               cfg.SelectionMatcher(),
		autoIngestNewBranches: cfg.Selection.AutoIngestNewBranches,
		git:                   git,
		pathResolver:          pathResolver,
		projectHarnesses:      projectHarnesses,
		recorder:              recorder,
	}
	return filter, recorder
}

// Prepare materializes every discovered session before the first Match call.
// Decisions are cached by session ID; Match is lookup-only and fails closed if
// the pipeline supplies a session outside the prepared cohort.
func (f *harvestSelectionFilter) Prepare(ctx context.Context, sessions []ingest.DiscoveredSession) error {
	type gitContext struct{ remote, branch string }
	resolvedGit := make(map[string]gitContext)
	inputs := make([]selectionCandidateInput, len(sessions))
	for index, session := range sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		projectPath := discoveredSessionProjectPath(session)
		branch := session.Branch
		gitRemote := ""
		if (f.projectHarnesses[session.Harness] || f.matcher.DiscoveryNeedsGit(session.Harness, session.SessionID)) && f.git != nil {
			dir := discoveredSessionGitDirectory(session, projectPath)
			if dir != "" {
				gitCtx, ok := resolvedGit[dir]
				if !ok {
					if remote, err := f.git.RemoteURL(ctx, dir); err == nil {
						gitCtx.remote = remote
					}
					if resolvedBranch, err := f.git.Branch(ctx, dir); err == nil {
						gitCtx.branch = resolvedBranch
					}
					resolvedGit[dir] = gitCtx
				}
				gitRemote = gitCtx.remote
				if gitCtx.branch != "" {
					branch = gitCtx.branch
				}
			}
		}
		inputs[index] = selectionCandidateInput{
			Harness:     session.Harness,
			GitRemote:   gitRemote,
			ProjectName: session.ProjectName,
			ProjectPath: projectPath,
			Branch:      branch,
			SessionID:   session.SessionID,
		}
	}

	candidates, err := prepareSelectionCandidates(ctx, inputs, f.pathResolver)
	if err != nil {
		return err
	}
	f.prepared = make(map[ingest.SessionID]preparedHarvestSelection, len(candidates))
	f.recordedConflicts = make(map[ingest.SessionID]bool)
	f.recorder.conflicts = nil
	for index, candidate := range candidates {
		f.prepared[candidate.SessionID] = preparedHarvestSelection{
			candidate: candidate,
			decision:  f.matcher.MatchDiscoveryCandidateDecision(candidate, f.autoIngestNewBranches),
			excluded:  f.matcher.ExcludesCandidate(candidate),
			session:   sessions[index],
		}
	}
	return nil
}

// Excludes reports the exact-denial result cached during complete-cohort
// preparation. An unknown session fails closed so a child outside that cohort
// cannot inherit a selected parent's result.
func (f *harvestSelectionFilter) Excludes(session ingest.DiscoveredSession) bool {
	prepared, ok := f.prepared[session.SessionID]
	return !ok || prepared.excluded
}

func (f *harvestSelectionFilter) Match(session ingest.DiscoveredSession) bool {
	prepared, ok := f.prepared[session.SessionID]
	if !ok {
		return false
	}
	switch prepared.decision.Match {
	case ingest.BranchMatchYes:
		return true
	case ingest.BranchMatchWithheldConflict:
		if !f.recordedConflicts[session.SessionID] {
			f.recorder.conflicts = append(f.recorder.conflicts, selectionConflict{
				session: prepared.session,
				branch:  prepared.candidate.Branch,
				entries: prepared.decision.Conflicting(),
			})
			f.recordedConflicts[session.SessionID] = true
		}
	}
	return false
}

func discoveredSessionProjectPath(session ingest.DiscoveredSession) string {
	if filepath.IsAbs(session.CWD) {
		return filepath.Clean(session.CWD)
	}
	if session.Harness == defaults.HarnessClaudeCode {
		slug := filepath.Base(filepath.Dir(string(session.SourcePath)))
		if decoded := decodeClaudeSlugToPath(slug); decoded != "" {
			return decoded
		}
	}
	return ""
}

func discoveredSessionGitDirectory(session ingest.DiscoveredSession, projectPath string) string {
	if projectPath != "" {
		return projectPath
	}
	dir := string(session.OriginalRoot)
	if dir == "" && session.SourcePath != "" {
		dir = filepath.Dir(string(session.SourcePath))
	}
	return dir
}
