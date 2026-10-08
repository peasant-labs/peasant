package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// BuildReclaimCommand constructs the `peasant reclaim` maintenance command.
//
// A session's managed generations are immutable: each activation installs a new
// generation and only the session's active pointer is read. The superseded
// generations keep their projection rows and their content-blob directories,
// which dominate a mature store. `peasant reclaim` removes them: under the
// exclusive per-session lock it reads the active generation and any pending
// activation intent, deletes every other generation's rows in one transaction,
// and then removes each superseded generation directory through the
// ownership-verified cleanup path.
//
// SQLite reuses freed pages, so the database file does not shrink on its own.
// The physical shrink is an OFFLINE step: stop every process that has the
// database open and run `sqlite3 <db> 'VACUUM;'`. `peasant reclaim` prints the
// exact path and refuses to run VACUUM itself, because it holds its own
// connection.
func BuildReclaimCommand() *cobra.Command {
	var (
		dryRun     bool
		limit      int
		confirm    bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "reclaim",
		Short: "Reclaim superseded managed generations (rows and blob directories)",
		Long: "Remove the managed generations a session no longer reads.\n\n" +
			"Only a session's active generation is read; every superseded generation keeps its\n" +
			"projection rows and its content-blob directory. This command deletes those rows in\n" +
			"one transaction and removes the superseded generation directories through the\n" +
			"ownership-verified cleanup path. A session with a pending activation intent is left\n" +
			"alone, the active generation is never touched, and a missing directory is success,\n" +
			"so an interrupted run is safe to repeat.\n\n" +
			"Run with --dry-run first: it lists the sessions, generations, rows and bytes it\n" +
			"would reclaim and changes nothing.\n\n" +
			"SQLite reuses the freed pages but does not shrink the database file. To reclaim the\n" +
			"disk space, stop every process using the database and run the offline VACUUM:\n\n" +
			"  peasant reclaim --dry-run          # confirm nothing is left\n" +
			"  sqlite3 <db> 'VACUUM;'             # offline; needs exclusive access\n\n" +
			"The database path is printed after a successful run.",
		RunE: func(cmd *cobra.Command, args []string) error {
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
			db, closeStore, err := openReclaimStore(cmd, dryRun, string(ownedRoot), dbPath)
			if err != nil {
				return err
			}
			defer closeStore()

			ctx := cmd.Context()
			if dryRun {
				plan, err := db.PlanSupersededGenerationReclaim(ctx, limit)
				if err != nil {
					return fmt.Errorf("plan superseded-generation reclaim: %w", err)
				}
				return writeReclaimPlan(cmd, plan, jsonOutput)
			}

			if !confirm {
				agreed, err := confirmReclaim(cmd)
				if err != nil {
					return err
				}
				if !agreed {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}

			result, err := db.ReclaimSupersededGenerations(ctx, limit)
			if err != nil {
				return fmt.Errorf("reclaim superseded generations: %w", err)
			}
			return writeReclaimResult(cmd, result, jsonOutput, dbPath)
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "List what would be reclaimed without deleting rows or directories")
	cmd.Flags().IntVar(&limit, "limit", 0, "Reclaim at most this many sessions with work (0 = all); re-run to continue where this pass stopped")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Skip interactive confirmation (for scripts/CI)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output results as JSON")

	return cmd
}

// openReclaimStore opens the analytics store with the managed-generation
// artifact store rooted at the configured output base. A dry run opens
// read-only and creates no file; a real run opens read-write.
func openReclaimStore(cmd *cobra.Command, dryRun bool, ownedRoot, dbPath string) (*store.Store, func(), error) {
	if dryRun {
		options, err := generationStoreOptionsReadOnly(ownedRoot)
		if err != nil {
			return nil, func() {}, err
		}
		db, err := store.OpenReadOnlyWithOptions(dbPath, options...)
		if err != nil {
			return nil, func() {}, err
		}
		return db, func() { _ = db.Close() }, nil
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
	return db, func() { _ = db.Close() }, nil
}

// confirmReclaim asks for consent to an irreversible delete. It refuses to
// prompt unless standard input is a terminal, so a piped or closed stream can
// never be attributed to a person; --confirm is the documented way to proceed
// without a prompt.
func confirmReclaim(cmd *cobra.Command) (bool, error) {
	in := cmd.InOrStdin()
	file, isFile := in.(*os.File)
	if !isFile || !term.IsTerminal(int(file.Fd())) {
		return false, fmt.Errorf("non-interactive terminal: `peasant reclaim` refused to prompt for consent to delete superseded generations because its standard input is not a terminal; nothing was deleted; re-run with --confirm to proceed without a prompt, or run the command from an interactive shell")
	}
	fmt.Fprint(cmd.OutOrStderr(), "\nReclaim all superseded managed generations? [y/N]: ")
	var response string
	fmt.Fscanln(file, &response)
	return response == "y" || response == "Y", nil
}

// writeReclaimPlan prints a dry-run forecast.
func writeReclaimPlan(cmd *cobra.Command, plan store.ReclaimPlan, jsonOutput bool) error {
	rows, footprint := plan.Totals()
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
			"dry_run":                 true,
			"sessions":                reclaimPlanSessionsToJSON(plan),
			"pending_intent_sessions": reclaimSessionIDsToStrings(plan.PendingIntentSessions),
			"totals": map[string]any{
				"sessions":    plan.SessionCount(),
				"generations": plan.GenerationCount(),
				"rows":        reclaimRowsToJSON(rows),
				"bytes":       footprint.Bytes,
				"files":       footprint.Files,
			},
		})
	}

	out := cmd.OutOrStdout()
	if plan.SessionCount() == 0 {
		fmt.Fprintln(out, "no superseded managed generations to reclaim")
		writeReclaimPendingIntentNotice(cmd, plan.PendingIntentSessions)
		return nil
	}
	fmt.Fprintf(out, "Superseded managed generations to reclaim (%d session(s), %d generation(s)):\n", plan.SessionCount(), plan.GenerationCount())
	fmt.Fprintf(out, "  %-38s  %-38s  %6s  %12s  %10s\n", "SESSION ID", "ACTIVE GENERATION", "GENS", "ROWS", "BYTES")
	for _, session := range plan.Sessions {
		sessionRows := session.Rows()
		sessionFootprint := session.Footprint()
		fmt.Fprintf(out, "  %-38s  %-38s  %6d  %12d  %10s\n",
			string(session.SessionID), session.ActiveGenerationID, len(session.Generations), sessionRows.Total(), formatReclaimBytes(sessionFootprint.Bytes))
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "rows by table:")
	writeReclaimRowTable(cmd, rows)
	fmt.Fprintf(out, "total: %d session(s), %d generation(s), %d row(s), %s\n", plan.SessionCount(), plan.GenerationCount(), rows.Total(), formatReclaimBytes(footprint.Bytes))
	fmt.Fprintln(out, "dry run: no row and no directory was removed; re-run without --dry-run to reclaim")
	writeReclaimPendingIntentNotice(cmd, plan.PendingIntentSessions)
	return nil
}

// writeReclaimResult prints an applied pass and the offline VACUUM reminder.
func writeReclaimResult(cmd *cobra.Command, result store.ReclaimResult, jsonOutput bool, dbPath string) error {
	if jsonOutput {
		encoded := map[string]any{
			"sessions":                result.Sessions,
			"generations":             result.Generations,
			"rows":                    reclaimRowsToJSON(result.Rows),
			"bodies_deleted":          result.BodiesDeleted,
			"blobs_deleted":           result.BlobsDeleted,
			"bytes":                   result.Footprint.Bytes,
			"files":                   result.Footprint.Files,
			"directories_removed":     result.DirectoriesRemoved,
			"pending_intent_sessions": reclaimSessionIDsToStrings(result.PendingIntentSessions),
			"warnings":                reclaimErrorsToStrings(result.Warnings),
			"database":                dbPath,
			"vacuum":                  "offline: sqlite3 " + dbPath + " 'VACUUM;'",
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(encoded)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "reclaimed %d session(s), %d generation(s), %d row(s), %s, %d director(ies)\n",
		result.Sessions, result.Generations, result.Rows.Total(), formatReclaimBytes(result.Footprint.Bytes), result.DirectoriesRemoved)
	for _, warning := range result.Warnings {
		fmt.Fprintf(cmd.OutOrStderr(), "warning: %v\n", warning)
	}
	writeReclaimPendingIntentNotice(cmd, result.PendingIntentSessions)
	if result.Rows.Total() > 0 || result.DirectoriesRemoved > 0 {
		fmt.Fprintf(out, "\nSQLite reuses the freed pages but does not shrink the file. To reclaim the space, stop every process using the database and run offline:\n  sqlite3 %s 'VACUUM;'\n", dbPath)
	}
	return nil
}

// writeReclaimRowTable prints the per-table row totals.
func writeReclaimRowTable(cmd *cobra.Command, rows store.ReclaimTableCounts) {
	out := cmd.OutOrStdout()
	for _, entry := range reclaimRowEntries(rows) {
		fmt.Fprintf(out, "  %-34s  %12d\n", entry.table, entry.count)
	}
}

// writeReclaimPendingIntentNotice reports sessions left alone because a pending
// activation intent owns a generation.
func writeReclaimPendingIntentNotice(cmd *cobra.Command, sessions []schema.SessionID) {
	if len(sessions) == 0 {
		return
	}
	shown := sessions
	elided := ""
	const limit = 10
	if len(shown) > limit {
		elided = fmt.Sprintf(" (and %d more)", len(shown)-limit)
		shown = shown[:limit]
	}
	ids := make([]string, len(shown))
	for i, id := range shown {
		ids[i] = string(id)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "note: left %d session(s) untouched because a pending activation intent owns a generation: %s%s; recover or retry the activation, then re-run\n",
		len(sessions), strings.Join(ids, ", "), elided)
}

// reclaimRowEntry is one table's name and count.
type reclaimRowEntry struct {
	table string
	count int64
}

// reclaimRowEntries lists the closed set of reclaim tables and their counts,
// sorted by table name so the output is stable.
func reclaimRowEntries(rows store.ReclaimTableCounts) []reclaimRowEntry {
	entries := []reclaimRowEntry{
		{"session_context_segment_refs", rows.SegmentRefs},
		{"session_context_segments", rows.ContextSegments},
		{"session_generation_associations", rows.GenerationAssociations},
		{"session_generation_commits", rows.GenerationCommits},
		{"session_generation_content", rows.GenerationContent},
		{"session_generation_diagnostics", rows.GenerationDiagnostics},
		{"session_generation_entries", rows.GenerationEntries},
		{"session_generation_subagents", rows.GenerationSubagents},
		{"session_generation_title_refs", rows.GenerationTitleRefs},
		{"session_generations", rows.SessionGenerations},
		{"session_projection_aliases", rows.ProjectionAliases},
		{"session_projection_content", rows.ProjectionContent},
		{"session_projection_entries", rows.ProjectionEntries},
		{"session_projection_generations", rows.Generations},
		{"session_projection_sections", rows.ProjectionSections},
		{"session_relationship_evidence", rows.RelationshipEvidence},
		{"session_section_native_metadata", rows.NativeMetadata},
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].table < entries[j].table })
	return entries
}

// reclaimRowsToJSON converts per-table counts to a JSON object.
func reclaimRowsToJSON(rows store.ReclaimTableCounts) map[string]any {
	return map[string]any{
		"session_context_segment_refs":   rows.SegmentRefs,
		"session_context_segments":       rows.ContextSegments,
		"session_generation_associations": rows.GenerationAssociations,
		"session_generation_commits":     rows.GenerationCommits,
		"session_generation_content":     rows.GenerationContent,
		"session_generation_diagnostics": rows.GenerationDiagnostics,
		"session_generation_entries":     rows.GenerationEntries,
		"session_generation_subagents":   rows.GenerationSubagents,
		"session_generation_title_refs":  rows.GenerationTitleRefs,
		"session_generations":              rows.SessionGenerations,
		"session_projection_aliases":     rows.ProjectionAliases,
		"session_projection_content":     rows.ProjectionContent,
		"session_projection_entries":     rows.ProjectionEntries,
		"session_projection_generations": rows.Generations,
		"session_projection_sections":    rows.ProjectionSections,
		"session_relationship_evidence":  rows.RelationshipEvidence,
		"session_section_native_metadata": rows.NativeMetadata,
		"total":                          rows.Total(),
	}
}

// reclaimPlanSessionsToJSON converts a plan to a JSON-friendly slice.
func reclaimPlanSessionsToJSON(plan store.ReclaimPlan) []map[string]any {
	result := make([]map[string]any, 0, plan.SessionCount())
	for _, session := range plan.Sessions {
		generations := make([]map[string]any, 0, len(session.Generations))
		for _, generation := range session.Generations {
			generations = append(generations, map[string]any{
				"generation_id": generation.GenerationID,
				"committed":     generation.Committed,
				"rows":          reclaimRowsToJSON(generation.Rows),
				"bytes":         generation.Footprint.Bytes,
				"files":         generation.Footprint.Files,
			})
		}
		result = append(result, map[string]any{
			"session_id":           string(session.SessionID),
			"active_generation_id": session.ActiveGenerationID,
			"generations":          generations,
		})
	}
	return result
}

// reclaimSessionIDsToStrings converts session identifiers to plain strings.
func reclaimSessionIDsToStrings(ids []schema.SessionID) []string {
	result := make([]string, len(ids))
	for i, id := range ids {
		result[i] = string(id)
	}
	return result
}

// reclaimErrorsToStrings converts warnings to plain strings.
func reclaimErrorsToStrings(errs []error) []string {
	result := make([]string, 0, len(errs))
	for _, err := range errs {
		result = append(result, err.Error())
	}
	return result
}

// formatReclaimBytes renders a byte count with a binary unit suffix.
func formatReclaimBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	value := float64(bytes)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	index := -1
	for value >= unit && index < len(units)-1 {
		value /= unit
		index++
	}
	return fmt.Sprintf("%.1f %s", value, units[index])
}
