package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// BuildMigrateCommand constructs the `peasant migrate` maintenance command.
//
// The migration converts file-backed native sessions to the harmonized
// content model in place, with a per-session shadow verification that
// refuses to lose or alter a byte, and with resume points at every session
// and phase boundary (the per-session state in the database is the progress
// record; there is no side file). Ctrl-C finishes the current session's
// transaction and exits cleanly with a partial report.
//
// The flags mirror `reclaim`: --dry-run forecasts without writing, --limit
// bounds one pass, --confirm skips the interactive prompt, and --json emits
// machine-readable output.
func BuildMigrateCommand() *cobra.Command {
	var (
		dryRun     bool
		limit      int
		confirm    bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Convert file-backed sessions to the harmonized content model",
		Long: "Convert file-backed native sessions to the harmonized content model in place.\n\n" +
			"Phase 0 preflights read-only (--dry-run stops here): counts per phase,\n" +
			"pending intents and superseded generations to discard, sampled\n" +
			"field/blob mismatches, free disk, and an advisory no-other-writer note.\n" +
			"Phase 1 drains pending intents, staged directories, and superseded\n" +
			"generations. Phase 2 converts each file-backed native session under its\n" +
			"exclusive lock, shadow-verifying the new rows against the legacy readers\n" +
			"before any old row is deleted. Phase 3 consolidates the search index in\n" +
			"one transaction. Phase 4 sweeps flagged sessions and reports the\n" +
			"retirement preconditions. Phase 5 (documented, offline) runs optimize\n" +
			"and VACUUM.\n\n" +
			"Run with --dry-run first: it reports the per-phase counts and changes\n" +
			"nothing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			previousContext := cmd.Context()
			cmd.SetContext(ctx)
			defer cmd.SetContext(previousContext)
			cfgPath := resolveConfigPath(cmd)
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			ownedRoot, err := ingest.NewResolvedPath(cfg.Output.BasePath)
			if err != nil {
				return fmt.Errorf("resolve the owned-artifact root: %w", err)
			}

			dbPath := string(defaults.ResolveDBFilePathWith(dataDirOverride(cmd)))
			db, closeStore, err := openMigrateStore(cmd, dryRun, string(ownedRoot), dbPath)
			if err != nil {
				return err
			}
			defer closeStore()

			if dryRun {
				plan, err := db.PlanMigration(ctx)
				if err != nil {
					return fmt.Errorf("plan migration: %w", err)
				}
				return writeMigratePlan(cmd, plan, jsonOutput)
			}
			plan, err := db.PlanMigration(ctx)
			if err != nil {
				return fmt.Errorf("plan migration: %w", err)
			}
			writeMigratePreflight(cmd.ErrOrStderr(), plan)
			if err := plan.CheckDisk(); err != nil {
				return err
			}

			if !confirm {
				agreed, err := confirmMigrate(cmd)
				if err != nil {
					return err
				}
				if !agreed {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}

			tracker := newMigrateProgressTracker(jsonOutput)
			result, err := db.Migrate(ctx, store.MigrateOptions{
				Limit:    limit,
				Progress: tracker.report(cmd),
			})
			if writeErr := writeMigrateResult(cmd, result, jsonOutput, dbPath); writeErr != nil {
				return errors.Join(err, writeErr)
			}
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return fmt.Errorf("migration stopped at a resume boundary: %w; the partial report is above; re-run `peasant migrate --confirm` to resume idempotently", err)
				}
				return fmt.Errorf("migrate: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report per-phase counts without converting any session")
	cmd.Flags().IntVar(&limit, "limit", 0, "Convert at most this many sessions with work (0 = all); re-run to continue where this pass stopped")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Skip interactive confirmation (for scripts/CI)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output results as JSON")

	return cmd
}

// openMigrateStore opens the analytics store with the managed-generation
// artifact store rooted at the configured output base. A dry run opens
// read-only and creates no file; a real run opens read-write.
func openMigrateStore(cmd *cobra.Command, dryRun bool, ownedRoot, dbPath string) (*store.Store, func(), error) {
	if dryRun {
		options, err := generationStoreOptionsReadOnly(ownedRoot)
		if err != nil {
			return nil, func() {}, err
		}
		db, err := store.OpenReadOnlyWithOptions(dbPath, options...)
		if err != nil {
			return nil, func() {}, err
		}
		return db, func() {
			_ = db.Close()
		}, nil
	}
	dataDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	if err := os.MkdirAll(dataDir, defaults.PrivateDirPerm); err != nil {
		return nil, func() {}, fmt.Errorf("create data directory: %w", err)
	}
	options, err := generationStoreOptions(ownedRoot)
	if err != nil {
		return nil, func() {}, err
	}
	options = append(options, store.WithMigrationConsent(store.PromptMigrationConsentOnTTY()))
	db, err := store.Open(dbPath, options...)
	if err != nil {
		return nil, func() {}, fmt.Errorf("open analytics store: %w", err)
	}
	return db, func() {
		_ = db.Close()
	}, nil
}

// confirmMigrate asks for consent to an in-place conversion. It refuses
// to prompt unless standard input is a terminal, so a piped or closed
// stream can never be attributed to a person; --confirm is the documented
// way to proceed without a prompt.
func confirmMigrate(cmd *cobra.Command) (bool, error) {
	in := cmd.InOrStdin()
	file, isFile := in.(*os.File)
	if !isFile || !term.IsTerminal(int(file.Fd())) {
		return false, fmt.Errorf("non-interactive terminal: `peasant migrate` refused to prompt for consent to convert sessions in place because its standard input is not a terminal; nothing was converted; re-run with --confirm to proceed without a prompt, or run the command from an interactive shell")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "\nWith no other writer running (stop any harvest), convert file-backed sessions in place? [y/N]: ")
	var response string
	fmt.Fscanln(file, &response)
	return response == "y" || response == "Y", nil
}

// migrateProgressTracker derives rate and ETA from the store's progress
// events for the human-readable progress lines. In JSON mode the lines
// go to stderr so stdout carries exactly one JSON document.
type migrateProgressTracker struct {
	started  time.Time
	last     time.Time
	jsonMode bool
}

func newMigrateProgressTracker(jsonMode bool) *migrateProgressTracker {
	now := time.Now()
	return &migrateProgressTracker{started: now, last: now, jsonMode: jsonMode}
}

// report returns the progress callback: a line per phase entry and a
// periodic line while sessions convert, each with sessions done and
// total, bytes freed, rate, and ETA.
func (t *migrateProgressTracker) report(cmd *cobra.Command) func(store.MigrateProgress) {
	var lastPhase store.MigrationPhase = -1
	var lastLine time.Time
	return func(progress store.MigrateProgress) {
		out := cmd.OutOrStdout()
		if t.jsonMode {
			out = cmd.ErrOrStderr()
		}
		if progress.Phase != lastPhase {
			lastPhase = progress.Phase
			lastLine = time.Now()
			fmt.Fprintf(out, "phase %s: %d/%d sessions, %s freed\n",
				progress.Phase, progress.SessionsDone, progress.SessionsTotal, formatMigrateBytes(progress.BytesFreed))
			return
		}
		now := time.Now()
		if now.Sub(lastLine) < 5*time.Second && progress.SessionsDone != progress.SessionsTotal {
			return
		}
		lastLine = now
		elapsed := now.Sub(t.started).Round(time.Second)
		rate, eta := migrateRateETA(t.started, now, progress.SessionsDone, progress.SessionsTotal)
		fmt.Fprintf(out, "phase %s: %d/%d sessions, %s freed, %s/s, elapsed %s, ETA %s\n",
			progress.Phase, progress.SessionsDone, progress.SessionsTotal,
			formatMigrateBytes(progress.BytesFreed), rate, elapsed, eta)
	}
}

// migrateRateETA derives the conversion rate and the estimated remaining
// time from the event stream. No rate exists before the first session
// finishes; a finished pass reports zero remaining.
func migrateRateETA(started, now time.Time, done, total int) (string, string) {
	elapsed := now.Sub(started)
	if done <= 0 || elapsed <= 0 {
		return "—", "—"
	}
	perSecond := float64(done) / elapsed.Seconds()
	remaining := "0s"
	if total > done {
		remaining = (time.Duration(float64(total-done)/perSecond) * time.Second).Round(time.Second).String()
	}
	return fmt.Sprintf("%.1f", perSecond), remaining
}

// formatMigrateBytes renders freed bytes in human units.
func formatMigrateBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit || suffix == "TiB" {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", bytes)
}

// writeMigratePlan prints a dry-run forecast: the per-phase counts, the
// drain sets, the consolidation need, the disk check, and the advisory.
// A dry run writes nothing; its counts equal the applied counts.
func writeMigratePlan(cmd *cobra.Command, plan store.MigratePlan, jsonOutput bool) error {
	if jsonOutput {
		sessions := make([]string, 0, len(plan.Sessions))
		for _, session := range plan.Sessions {
			sessions = append(sessions, string(session))
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"dry_run":  true,
			"sessions": sessions,
			"totals": map[string]any{
				"sessions":             len(plan.Sessions),
				"pending_intents":      plan.PendingIntents,
				"superseded":           plan.SupersededGenerations,
				"mirror_rows":          plan.MirrorRows,
				"estimated_bytes":      plan.EstimatedBytes,
				"search_consolidation": plan.NeedsSearchConsolidation,
			},
			"mismatch_sample": map[string]any{
				"sessions":          plan.SampledSessions,
				"mismatch_sessions": plan.SampledMismatchSessions,
				"mismatched_refs":   plan.SampledMismatchedRefs,
			},
			"disk": map[string]any{
				"free_bytes":     plan.DiskFreeBytes,
				"required_bytes": plan.DiskRequiredBytes,
				"store_bytes":    plan.StoreBytes,
				"ok":             plan.DiskFreeOK,
			},
			"advisory": plan.Advisory,
		})
	}

	out := cmd.OutOrStdout()
	if len(plan.Sessions) == 0 && !plan.NeedsSearchConsolidation {
		fmt.Fprintln(out, "no sessions to convert and the search index is consolidated: nothing to do")
		return nil
	}
	fmt.Fprintf(out, "Phase 0 preflight: %d session(s) to convert, %d pending intent(s) to discard, %d superseded generation(s) to discard\n",
		len(plan.Sessions), plan.PendingIntents, plan.SupersededGenerations)
	writeMigratePlanSessions(out, plan.Sessions)
	if plan.SampledSessions > 0 {
		fmt.Fprintf(out, "sampled field/blob mismatches: %d of %d sampled session(s) carry %d mismatched ref(s) and will roll back in Phase 2\n",
			plan.SampledMismatchSessions, plan.SampledSessions, plan.SampledMismatchedRefs)
	}
	fmt.Fprintf(out, "Phase 1 drain: %d pending intent(s), %d superseded generation(s)\n", plan.PendingIntents, plan.SupersededGenerations)
	fmt.Fprintf(out, "Phase 2 convert: %d session(s), about %d mirror row(s)\n", len(plan.Sessions), plan.MirrorRows)
	if plan.NeedsSearchConsolidation {
		fmt.Fprintln(out, "Phase 3 search consolidation: the retired index is still present")
	} else {
		fmt.Fprintln(out, "Phase 3 search consolidation: already consolidated, skipped")
	}
	fmt.Fprintf(out, "Phase 4 cleanup: sweep flagged sessions and evaluate the retirement preconditions\n")
	fmt.Fprintf(out, "Phase 5 offline: optimize + VACUUM (documented, never run here)\n")
	fmt.Fprintf(out, "estimated owned-tree bytes to free: %s\n", formatMigrateBytes(plan.EstimatedBytes))
	writeMigratePreflight(out, plan)
	fmt.Fprintln(out, "dry run: nothing was converted and nothing was deleted; re-run without --dry-run to migrate")
	return nil
}

func writeMigratePreflight(out io.Writer, plan store.MigratePlan) {
	if plan.DiskFreeBytes >= 0 {
		status := "OK"
		if !plan.DiskFreeOK {
			status = "INSUFFICIENT: free space before running"
		}
		fmt.Fprintf(out, "free disk: %s (%s); required %s (60%% of the %s peasant database)\n", formatMigrateBytes(plan.DiskFreeBytes), status, formatMigrateBytes(plan.DiskRequiredBytes), formatMigrateBytes(plan.StoreBytes))
	} else {
		fmt.Fprintf(out, "free disk: unknown on this platform; required %s (60%% of the peasant database); check available space before continuing\n", formatMigrateBytes(plan.DiskRequiredBytes))
	}
	fmt.Fprintf(out, "advisory: %s\n", plan.Advisory)
}

// writeMigratePlanSessions lists the sessions with conversion work,
// bounded so a large store prints a readable plan while the full list
// stays one --json away.
func writeMigratePlanSessions(out io.Writer, sessions []schema.SessionID) {
	const shown = 10
	for i, session := range sessions {
		if i >= shown {
			break
		}
		fmt.Fprintf(out, "  convert %s\n", session)
	}
	if len(sessions) > shown {
		fmt.Fprintf(out, "  ... and %d more (see --json for the full list)\n", len(sessions)-shown)
	}
}

// writeMigrateResult prints an applied pass: the per-session
// dispositions, the data rollbacks with their reasons, the warnings,
// the search consolidation, the retirement evaluation, and the offline
// VACUUM reminder.
func writeMigrateResult(cmd *cobra.Command, result store.MigrateResult, jsonOutput bool, dbPath string) error {
	if jsonOutput {
		rollbacks := make([]map[string]any, 0, len(result.Rollbacks))
		for _, rollback := range result.Rollbacks {
			rollbacks = append(rollbacks, map[string]any{
				"session":   string(rollback.SessionID),
				"dimension": rollback.Dimension,
				"reason":    rollback.Reason,
			})
		}
		preconditions := make([]map[string]any, 0, len(result.Preconditions))
		for _, precondition := range result.Preconditions {
			entry := map[string]any{
				"name":      string(precondition.Name),
				"passed":    precondition.Passed,
				"row_count": precondition.RowCount,
			}
			if precondition.Detail != "" {
				entry["detail"] = precondition.Detail
			}
			preconditions = append(preconditions, entry)
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"converted":           result.Converted,
			"rolled_back":         result.RolledBack,
			"marked":              result.Marked,
			"skipped":             result.Skipped,
			"bytes_freed":         result.BytesFreed,
			"stats_overflow_keys": result.StatsOverflowKeys,
			"rollbacks":           rollbacks,
			"warnings":            result.Warnings,
			"search_consolidated": result.SearchConsolidated,
			"preconditions":       preconditions,
			"ready_for_release":   result.ReadyForNextRelease,
			"database":            dbPath,
			"vacuum":              "offline: sqlite3 " + dbPath + " 'VACUUM;'",
		})
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "converted %d session(s), rolled back %d, marked %d, skipped %d, %s freed\n",
		result.Converted, result.RolledBack, result.Marked, result.Skipped, formatMigrateBytes(result.BytesFreed))
	fmt.Fprintf(out, "stat overflow keys: %v\n", result.StatsOverflowKeys)
	for _, rollback := range result.Rollbacks {
		fmt.Fprintf(out, "rolled back %s at %s: %s; marked for re-index\n", rollback.SessionID, rollback.Dimension, rollback.Reason)
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(cmd.OutOrStderr(), "warning: %s\n", warning)
	}
	if result.SearchConsolidated {
		fmt.Fprintln(out, "search index consolidated: triggers retargeted, index rebuilt, retired index dropped")
	} else {
		fmt.Fprintln(out, "search index not yet consolidated: re-run peasant migrate --confirm to resume")
	}
	if len(result.Preconditions) > 0 {
		if result.ReadyForNextRelease {
			fmt.Fprintln(out, "ready for the next release: all five retirement preconditions hold")
		} else {
			fmt.Fprintln(out, "not ready for the next release; failing preconditions:")
			for _, precondition := range result.Preconditions {
				if precondition.Passed {
					continue
				}
				fmt.Fprintf(out, "  - %s: %s\n", precondition.Name, precondition.Detail)
			}
		}
	}
	if result.BytesFreed > 0 {
		fmt.Fprintf(out, "\nSQLite reuses the freed pages but does not shrink the file. To reclaim the space, stop every process using the database and run offline:\n  sqlite3 %s \"INSERT INTO session_search_fts(session_search_fts) VALUES('optimize'); VACUUM;\"\n", dbPath)
	}
	return nil
}
