package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// BuildPruneCommand constructs the `peasant prune` command.
func BuildPruneCommand() *cobra.Command {
	var (
		sessionIDs []string
		project    string
		harness    string
		before     string
		after      string
		all        bool
		unselected bool
		confirm    bool
		dryRun     bool
		jsonOutput bool
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove sessions from local store and database",
		Long:  "Permanently delete sessions matching the given filters from the local SQLite database and filesystem.",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Build filter from flags.
			filter, err := buildPruneFilter(sessionIDs, project, harness, before, after, all)
			if err != nil {
				return err
			}

			// Require at least one filter.
			if !filter.All && !unselected && len(filter.SessionIDs) == 0 && filter.ProjectHash == nil &&
				filter.Harness == nil && filter.Before == nil && filter.After == nil {
				return fmt.Errorf("at least one filter is required (--session, --project, --harness, --before, --after, --all, or --unselected)")
			}

			// --unselected is mutually exclusive with all other filter flags.
			// Mixing them is ambiguous: --unselected overrides the filter anyway.
			if unselected && (filter.All || len(filter.SessionIDs) > 0 ||
				filter.ProjectHash != nil || filter.Harness != nil ||
				filter.Before != nil || filter.After != nil) {
				return fmt.Errorf("--unselected cannot be combined with other filter flags (--all, --session, --project, --harness, --before, --after)")
			}

			// --unselected: load config, validate mode, build matcher, then query all sessions.
			var selMatcher *ingest.SelectionMatcher
			if unselected {
				cfgPath := resolveConfigPath(cmd)
				cfg, cfgErr := loadConfig(cfgPath)
				if cfgErr != nil {
					return fmt.Errorf("load config: %w", cfgErr)
				}
				if cfg.Selection.Mode != config.SelectionModeSelected {
					return fmt.Errorf("--unselected requires selection.mode=%q in config, but mode is %q; with mode=%q all sessions are selected so nothing would be pruned",
						config.SelectionModeSelected, cfg.Selection.Mode, config.SelectionModeAll)
				}
				m := cfg.SelectionMatcher()
				selMatcher = &m
				// Query all sessions so we can filter client-side.
				filter = ingest.PruneFilter{All: true}
			}

			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			ctx := cmd.Context()

			// Query matching sessions.
			sessions, err := db.QueryPrunableSessions(ctx, filter)
			if err != nil {
				return fmt.Errorf("query sessions: %w", err)
			}

			// Apply selection filter client-side when --unselected is active.
			if selMatcher != nil {
				sessions, err = unselectedPruneSessions(
					ctx, sessions, *selMatcher, ingest.NewPhysicalPathResolver(),
				)
				if err != nil {
					return err
				}
			}

			if len(sessions) == 0 {
				if jsonOutput {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
						"deleted": 0,
						"errors":  []string{},
					})
				}
				if unselected {
					fmt.Fprintln(cmd.OutOrStdout(), "no unselected sessions found")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "no sessions match the given filters")
				}
				return nil
			}

			plan := ingest.NewPrunePlan(sessions)
			plannedSessions := plan.Sessions()

			// Captured with the plan, for --all's residue notice only: it is the
			// only way to tell a directory that was already there without a
			// database row from one that arrives while the user is deciding.
			var sessionDirsBeforeRun map[string]bool
			if all {
				sessionDirsBeforeRun = scanSessionDirs(pruneSyncDir(cmd))
			}

			// Print preview.
			if !jsonOutput {
				printPrunePreview(cmd, plannedSessions)
			}

			// Dry run: preview only.
			if dryRun {
				if jsonOutput {
					return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
						"dry_run":  true,
						"sessions": pruneSessionsToJSON(plannedSessions),
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "\ndry run: %d session(s) would be deleted\n", len(plannedSessions))
				return nil
			}

			// Safety check: require confirmation.
			if !confirm {
				agreed, err := confirmPrune(cmd, plannedSessions)
				if err != nil {
					return err
				}
				if !agreed {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted")
					return nil
				}
			}

			// Execute only the immutable set that was previewed and confirmed.
			result, err := db.PruneSessions(ctx, plan.SessionIDs())
			if err != nil {
				return fmt.Errorf("prune sessions: %w", err)
			}

			// Filesystem cleanup (after DB commit).
			fsErrs := pruneFilesystem(cmd, plannedSessions)
			result.Errors = append(result.Errors, fsErrs...)
			if all {
				noticePruneResidue(cmd, sessionDirsBeforeRun, plannedSessions)
			}

			if jsonOutput {
				errStrs := make([]string, len(result.Errors))
				for i, e := range result.Errors {
					errStrs[i] = e.Error()
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{
					"deleted": result.Deleted,
					"errors":  errStrs,
				})
			}

			fmt.Fprintf(cmd.OutOrStdout(), "deleted %d session(s)\n", result.Deleted)
			for _, e := range result.Errors {
				fmt.Fprintf(cmd.OutOrStderr(), "warning: %v\n", e)
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&sessionIDs, "session", nil, "Session IDs to prune (repeatable)")
	cmd.Flags().StringVar(&project, "project", "", "Prune sessions by project hash")
	cmd.Flags().StringVar(&harness, "harness", "", "Prune sessions by harness (claude-code, gemini-cli, codex, opencode)")
	cmd.Flags().StringVar(&before, "before", "", "Prune sessions started before this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&after, "after", "", "Prune sessions started after this date (YYYY-MM-DD)")
	cmd.Flags().BoolVar(&all, "all", false, "Prune all sessions")
	cmd.Flags().BoolVar(&unselected, "unselected", false, "Prune sessions not selected by harness, project, clone, or explicit session ID. Branch filters are not applied (requires selection.mode=selected)")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "Skip interactive confirmation (for scripts/CI)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Show what would be deleted without deleting")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output results as JSON")

	return cmd
}

// unselectedPruneSessions classifies the complete queried cohort before it
// returns any destructive candidate. ProjectPath resolution failures remain
// ambiguous, so a remote or name alone cannot retain one guessed clone while
// another clone is pruned.
func unselectedPruneSessions(
	ctx context.Context,
	rows []ingest.PruneSessionRow,
	matcher ingest.SelectionMatcher,
	resolver ingest.PathIdentityResolver,
) ([]ingest.PruneSessionRow, error) {
	inputs := make([]selectionCandidateInput, len(rows))
	for index, row := range rows {
		inputs[index] = selectionCandidateInput{
			Harness:     row.Harness,
			GitRemote:   row.GitRemote,
			ProjectName: row.ProjectName,
			ProjectHash: row.ProjectHash,
			ProjectPath: row.ProjectPath,
			SessionID:   row.SessionID,
		}
	}
	candidates, err := prepareSelectionCandidates(ctx, inputs, resolver)
	if err != nil {
		return nil, fmt.Errorf("prepare prune selection: resolve the complete stored-session cohort before matching clones: %w; no session was deleted; retry the command", err)
	}
	unselected := make([]ingest.PruneSessionRow, 0, len(rows))
	for index, candidate := range candidates {
		if !matcher.MatchesCandidate(candidate) {
			unselected = append(unselected, rows[index])
		}
	}
	return unselected, nil
}

// buildPruneFilter constructs a PruneFilter from CLI flag values.
func buildPruneFilter(sessionIDStrs []string, project, harness, before, after string, all bool) (ingest.PruneFilter, error) {
	filter := ingest.PruneFilter{All: all}

	for _, raw := range sessionIDStrs {
		sid, err := ingest.NewSessionID(raw)
		if err != nil {
			return filter, fmt.Errorf("invalid session ID %q: %w", raw, err)
		}
		filter.SessionIDs = append(filter.SessionIDs, sid)
	}

	if project != "" {
		filter.ProjectHash = &project
	}

	if harness != "" {
		p := ingest.Harness(harness)
		if !p.IsKnown() {
			return filter, fmt.Errorf("invalid --harness value %q: must be one of %s", harness, joinHarnesses(defaults.AllHarnesses))
		}
		filter.Harness = &p
	}

	if before != "" {
		t, err := time.Parse("2006-01-02", before)
		if err != nil {
			return filter, fmt.Errorf("invalid --before date %q: use YYYY-MM-DD format", before)
		}
		ms := t.UnixMilli()
		filter.Before = &ms
	}

	if after != "" {
		t, err := time.Parse("2006-01-02", after)
		if err != nil {
			return filter, fmt.Errorf("invalid --after date %q: use YYYY-MM-DD format", after)
		}
		ms := t.UnixMilli()
		filter.After = &ms
	}

	return filter, nil
}

// confirmPrune asks for interactive consent to an irreversible delete and
// reports whether it was given. The count it prints is taken from the frozen
// plan — the same slice that feeds the preview, the delete and the filesystem
// cleanup — so the number a user approves is the number that is removed.
//
// The prompt reads the command's own input stream, which is os.Stdin for a real
// invocation. Consent is refused outright unless that stream is a terminal:
// a piped or closed stdin cannot be an informed answer to a destructive
// question, and --confirm is the documented way to proceed without one.
func confirmPrune(cmd *cobra.Command, plannedSessions []ingest.PruneSessionRow) (bool, error) {
	in := cmd.InOrStdin()
	file, isFile := in.(*os.File)
	if !isFile || !term.IsTerminal(int(file.Fd())) {
		return false, fmt.Errorf("non-interactive terminal: `peasant prune` refused to prompt for consent to delete %d session(s) because its standard input is not a terminal, so an affirmative answer cannot be attributed to a person; nothing was deleted; re-run with --confirm to proceed without a prompt, or run the command from an interactive shell", len(plannedSessions))
	}
	fmt.Fprintf(cmd.OutOrStderr(), "\nDelete %d session(s)? [y/N]: ", len(plannedSessions))
	var response string
	fmt.Fscanln(file, &response)
	return response == "y" || response == "Y", nil
}

// printPrunePreview displays a table of sessions that will be deleted.
func printPrunePreview(cmd *cobra.Command, sessions []ingest.PruneSessionRow) {
	fmt.Fprintf(cmd.OutOrStdout(), "Sessions to delete (%d):\n", len(sessions))
	fmt.Fprintf(cmd.OutOrStdout(), "  %-38s  %-10s  %-20s  %s\n", "SESSION ID", "HARNESS", "PROJECT", "DATE")
	for _, s := range sessions {
		date := "unknown"
		if s.StartMs > 0 {
			date = time.UnixMilli(s.StartMs).UTC().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(cmd.OutOrStdout(), "  %-38s  %-10s  %-20s  %s\n",
			string(s.SessionID), s.Harness.String(), truncate(s.ProjectName, 20), date)
	}
}

// pruneFilesystem removes session directories from the output directory.
// Only directories captured in the confirmed plan are removed; sessions that
// appear concurrently after preview are never swept by a broad directory delete.
func pruneFilesystem(cmd *cobra.Command, sessions []ingest.PruneSessionRow) []error {
	outputDir := string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd)))
	syncDir := filepath.Join(outputDir, "peasant-sync")

	// Selective: remove individual session dirs, then clean up empty parents.
	var errs []error
	parentDirs := make(map[string]struct{})
	for _, s := range sessions {
		if s.OutputPath == "" {
			continue
		}
		parentDir := filepath.Join(syncDir, s.OutputPath)
		parentDirs[parentDir] = struct{}{}
		dir := filepath.Join(parentDir, string(s.SessionID))
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", dir, err))
		}
	}

	// Remove empty host slug directories. os.Remove only succeeds on empty dirs.
	for dir := range parentDirs {
		_ = os.Remove(dir)
	}
	return errs
}

// pruneSyncDir is the transcript tree prune cleans up under.
func pruneSyncDir(cmd *cobra.Command) string {
	return filepath.Join(string(defaults.ResolveDataDirPathWith(dataDirOverride(cmd))), "peasant-sync")
}

// scanSessionDirs lists every "<hostSlug>/<sessionID>" directory under the
// transcript tree. A missing or unreadable tree yields an empty set: the residue
// notice is an extra disclosure, never a reason to fail a completed prune.
func scanSessionDirs(syncDir string) map[string]bool {
	found := map[string]bool{}
	hostSlugs, err := os.ReadDir(syncDir)
	if err != nil {
		return found
	}
	for _, hostSlug := range hostSlugs {
		if !hostSlug.IsDir() {
			continue
		}
		sessions, readErr := os.ReadDir(filepath.Join(syncDir, hostSlug.Name()))
		if readErr != nil {
			continue
		}
		for _, session := range sessions {
			if session.IsDir() {
				found[filepath.Join(hostSlug.Name(), session.Name())] = true
			}
		}
	}
	return found
}

// noticePruneResidue reports transcript directories that `prune --all` did not
// remove AND was never going to: ones already on disk with no database row, so
// no plan could have included them.
//
// It deliberately does not report a directory that appeared after the plan was
// frozen. Such a directory is a live session that arrived while the user was
// deciding, and leaving it alone is the guarantee the frozen plan exists to
// provide — reporting it as leftover would invite the user to delete the one
// thing the command protected. That is also why the remedy names the specific
// orphan paths and says NOT to remove the tree itself: the tree still holds
// every session that was never in scope.
//
// before is the set of session directories captured when the plan was frozen,
// which is what makes "was already there" distinguishable from "arrived since".
func noticePruneResidue(cmd *cobra.Command, before map[string]bool, planned []ingest.PruneSessionRow) {
	syncDir := pruneSyncDir(cmd)
	inPlan := make(map[string]bool, len(planned))
	for _, session := range planned {
		inPlan[filepath.Join(session.OutputPath, string(session.SessionID))] = true
	}

	var orphans []string
	for path := range scanSessionDirs(syncDir) {
		// Still in the plan means the delete failed; that is reported as an
		// error, not as residue. Absent from the pre-run snapshot means it
		// arrived during the run and is live.
		if inPlan[path] || !before[path] {
			continue
		}
		orphans = append(orphans, path)
	}
	if len(orphans) == 0 {
		return
	}
	sort.Strings(orphans)

	const limit = 10
	shown := orphans
	elided := ""
	if len(shown) > limit {
		shown = shown[:limit]
		elided = fmt.Sprintf(" (and %d more)", len(orphans)-limit)
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"warning: prune --all finished, but %d transcript director(ies) under %q have no database row, so no deletion plan could include them and they were left in place: %s%s; each still holds transcript files that peasant does not track; remove those specific paths if you no longer want them, and do NOT remove %q itself — it still holds sessions that were never in scope, including any that arrived while you were deciding\n",
		len(orphans), syncDir, strings.Join(shown, ", "), elided, syncDir)
}

// pruneSessionsToJSON converts PruneSessionRows to a JSON-friendly slice.
func pruneSessionsToJSON(sessions []ingest.PruneSessionRow) []map[string]any {
	result := make([]map[string]any, len(sessions))
	for i, s := range sessions {
		result[i] = map[string]any{
			"session_id":  string(s.SessionID),
			"harness":     s.Harness.String(),
			"project":     s.ProjectName,
			"git_remote":  s.GitRemote,
			"start_ms":    s.StartMs,
			"turn_count":  s.TurnCount,
			"output_path": s.OutputPath,
		}
	}
	return result
}

// truncate shortens a string to maxLen, adding "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
