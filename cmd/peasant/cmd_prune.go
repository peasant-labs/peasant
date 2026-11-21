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
	"github.com/peasant-labs/peasant/internal/store"
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
		orphans    bool
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

			// --orphans needs every database row in hand to know which
			// directories have none; only --all queries every row.
			if orphans && !all {
				return fmt.Errorf("--orphans requires --all: an orphan is a transcript directory with no database row, and only --all reads every row, so any narrower selection cannot tell an orphan from a session it merely left out; nothing was deleted; re-run as `peasant prune --all --orphans`")
			}

			// Require at least one filter.
			if !filter.All && !unselected && len(filter.SessionIDs) == 0 && filter.Project == nil &&
				filter.Harness == nil && filter.Before == nil && filter.After == nil {
				return fmt.Errorf("at least one filter is required (--session, --project, --harness, --before, --after, --all, or --unselected)")
			}

			// --unselected is mutually exclusive with all other filter flags.
			// Mixing them is ambiguous: --unselected overrides the filter anyway.
			if unselected && (filter.All || len(filter.SessionIDs) > 0 ||
				filter.Project != nil || filter.Harness != nil ||
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

			openStore := openDB
			if dryRun {
				openStore = openDBForRead
			}
			db, cleanup, err := openStore(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			ctx := cmd.Context()

			// --orphans: snapshot the transcript tree BEFORE the query, so that
			// every directory seen here whose row was already committed is
			// matched by the query below. Scanning after the query would let a
			// session ingested in between look like an orphan.
			var dirsBeforeQuery map[string]bool
			if orphans {
				dirsBeforeQuery = scanSessionDirs(pruneSyncDir(cmd))
			}

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

			// The orphan set is enumerated once, here, before anything is
			// previewed or deleted. It is the only set of directories --orphans
			// may remove: nothing found later is ever added to it.
			var orphanDirs []string
			if orphans {
				orphanDirs = enumeratePruneOrphans(dirsBeforeQuery, sessions)
			}

			if len(sessions) == 0 && len(orphanDirs) == 0 {
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
			switch {
			case orphans:
				sessionDirsBeforeRun = dirsBeforeQuery
			case all:
				sessionDirsBeforeRun = scanSessionDirs(pruneSyncDir(cmd))
			}

			// Print preview.
			if !jsonOutput {
				printPrunePreview(cmd, plannedSessions)
				if orphans {
					printPruneOrphanPreview(cmd, orphanDirs)
				}
			}

			// Dry run: preview only.
			if dryRun {
				if jsonOutput {
					payload := map[string]any{
						"dry_run":  true,
						"sessions": pruneSessionsToJSON(plannedSessions),
					}
					if orphans {
						payload["orphans"] = orphanDirsToJSON(orphanDirs)
					}
					return json.NewEncoder(cmd.OutOrStdout()).Encode(payload)
				}
				if orphans {
					fmt.Fprintf(cmd.OutOrStdout(), "\ndry run: %d session(s) and %d orphaned transcript director(ies) would be deleted\n", len(plannedSessions), len(orphanDirs))
					return nil
				}
				fmt.Fprintf(cmd.OutOrStdout(), "\ndry run: %d session(s) would be deleted\n", len(plannedSessions))
				return nil
			}

			// Safety check: require confirmation.
			if !confirm {
				agreed, err := confirmPrune(cmd, plannedSessions, orphans, orphanDirs)
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

			// Orphan cleanup: only the enumerated, previewed, confirmed list.
			var orphanOutcome pruneOrphanOutcome
			if orphans {
				orphanOutcome = removePruneOrphans(ctx, cmd, db, orphanDirs)
				result.Errors = append(result.Errors, orphanOutcome.errs...)
			}
			if all {
				noticePruneResidue(cmd, sessionDirsBeforeRun, plannedSessions, orphanDirs)
			}

			if jsonOutput {
				errStrs := make([]string, len(result.Errors))
				for i, e := range result.Errors {
					errStrs[i] = e.Error()
				}
				payload := map[string]any{
					"deleted": result.Deleted,
					"errors":  errStrs,
				}
				if orphans {
					payload["orphans_removed"] = orphanDirsToJSON(orphanOutcome.removed)
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(payload)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "deleted %d session(s)\n", result.Deleted)
			if orphans {
				fmt.Fprintf(cmd.OutOrStdout(), "removed %d orphaned transcript director(ies)\n", len(orphanOutcome.removed))
			}
			for _, e := range result.Errors {
				fmt.Fprintf(cmd.OutOrStderr(), "warning: %v\n", e)
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&sessionIDs, "session", nil, "Session IDs to prune (repeatable)")
	cmd.Flags().StringVar(&project, "project", "", "Prune sessions of one project: its hash, path, directory name or git remote (owner/repo, host/owner/repo or a clone URL)")
	cmd.Flags().StringVar(&harness, "harness", "", "Prune sessions by harness ("+joinHarnesses(defaults.Harnesses())+")")
	cmd.Flags().StringVar(&before, "before", "", "Prune sessions started before this time ("+timeFilterHelp+")")
	cmd.Flags().StringVar(&after, "after", "", "Prune sessions started after this time ("+timeFilterHelp+")")
	cmd.Flags().BoolVar(&all, "all", false, "Prune all sessions")
	cmd.Flags().BoolVar(&unselected, "unselected", false, "Prune sessions not selected by harness, project, clone, branch, or explicit session rules (requires selection.mode=selected)")
	cmd.Flags().BoolVar(&orphans, "orphans", false, "With --all, also delete transcript directories under peasant-sync that have no database row. "+
		"They are listed in the preview and covered by the same confirmation, and only that listed set is removed. "+
		"An orphan is left by an earlier prune that could not remove its directory, a session written just before a store failure, or an interrupted ingest")
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
			Branch:      row.GitBranch,
			SessionID:   row.SessionID,
		}
	}
	candidates, err := prepareSelectionCandidates(ctx, inputs, resolver)
	if err != nil {
		return nil, fmt.Errorf("prepare prune selection: resolve the complete stored-session cohort before matching clones: %w; no session was deleted; retry the command", err)
	}
	unselected := make([]ingest.PruneSessionRow, 0, len(rows))
	for index, candidate := range candidates {
		if matcher.MatchBranchCandidate(candidate) != ingest.BranchMatchYes {
			unselected = append(unselected, rows[index])
		}
	}
	return unselected, nil
}

// buildPruneFilter constructs a PruneFilter from CLI flag values.
func buildPruneFilter(sessionIDStrs []string, project, harness, before, after string, all bool) (ingest.PruneFilter, error) {
	filter := ingest.PruneFilter{All: all}
	if all && (len(sessionIDStrs) > 0 || project != "" || harness != "" || before != "" || after != "") {
		return filter, fmt.Errorf("--all cannot be combined with --session, --project, --harness, --before or --after: --all selects every session and would ignore the filter; nothing was deleted; drop --all to prune only the filtered sessions")
	}

	for _, raw := range sessionIDStrs {
		sid, err := ingest.NewSessionID(raw)
		if err != nil {
			return filter, fmt.Errorf("invalid session ID %q: %w", raw, err)
		}
		filter.SessionIDs = append(filter.SessionIDs, sid)
	}

	if project != "" {
		filter.Project = &project
	}

	if harness != "" {
		p := ingest.Harness(harness)
		if !p.IsKnown() {
			return filter, fmt.Errorf("invalid --harness value %q: must be one of %s", harness, joinHarnesses(defaults.Harnesses()))
		}
		filter.Harness = &p
	}

	if before != "" {
		t, err := parseTimeFilter(before, time.Now())
		if err != nil {
			return filter, fmt.Errorf("invalid --before time %q: %w", before, err)
		}
		ms := t.UnixMilli()
		filter.Before = &ms
	}

	if after != "" {
		t, err := parseTimeFilter(after, time.Now())
		if err != nil {
			return filter, fmt.Errorf("invalid --after time %q: %w", after, err)
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
//
// With --orphans the question also names the enumerated orphan count, so the
// directories it removes are inside the same consent as the sessions.
func confirmPrune(cmd *cobra.Command, plannedSessions []ingest.PruneSessionRow, orphans bool, orphanDirs []string) (bool, error) {
	subject := fmt.Sprintf("%d session(s)", len(plannedSessions))
	if orphans {
		subject = fmt.Sprintf("%d session(s) and %d orphaned transcript director(ies)", len(plannedSessions), len(orphanDirs))
	}
	in := cmd.InOrStdin()
	file, isFile := in.(*os.File)
	if !isFile || !term.IsTerminal(int(file.Fd())) {
		return false, fmt.Errorf("non-interactive terminal: `peasant prune` refused to prompt for consent to delete %s because its standard input is not a terminal, so an affirmative answer cannot be attributed to a person; nothing was deleted; re-run with --confirm to proceed without a prompt, or run the command from an interactive shell", subject)
	}
	fmt.Fprintf(cmd.OutOrStderr(), "\nDelete %s? [y/N]: ", subject)
	return readConsent(file), nil
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
//
// orphanDirs is the set --orphans enumerated and the user confirmed. Those
// directories were acted on: a failure to remove one is reported as an error,
// and one that gained a row since is no longer residue, so neither is listed.
func noticePruneResidue(cmd *cobra.Command, before map[string]bool, planned []ingest.PruneSessionRow, orphanDirs []string) {
	syncDir := pruneSyncDir(cmd)
	inPlan := make(map[string]bool, len(planned)+len(orphanDirs))
	for _, session := range planned {
		inPlan[filepath.Join(session.OutputPath, string(session.SessionID))] = true
	}
	for _, orphan := range orphanDirs {
		inPlan[orphan] = true
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

// enumeratePruneOrphans returns, sorted, the "<hostSlug>/<name>" directories in
// dirs that belong to no row of rows. rows must come from an --all query taken
// AFTER dirs was scanned, so a directory whose row was committed before the
// scan is matched.
//
// A directory stays out of the set when its name equals ANY row's session ID,
// whatever host slug the row records: deleting a live session's transcripts
// because its slug differs would be exactly the overreach --orphans must not
// have.
func enumeratePruneOrphans(dirs map[string]bool, rows []ingest.PruneSessionRow) []string {
	known := make(map[string]bool, len(rows))
	for _, row := range rows {
		known[string(row.SessionID)] = true
	}
	var orphans []string
	for path := range dirs {
		if known[filepath.Base(path)] {
			continue
		}
		orphans = append(orphans, path)
	}
	sort.Strings(orphans)
	return orphans
}

// printPruneOrphanPreview lists every orphaned directory --orphans would
// remove. Nothing is elided: the list is what the user consents to.
func printPruneOrphanPreview(cmd *cobra.Command, orphanDirs []string) {
	fmt.Fprintf(cmd.OutOrStdout(), "\nOrphaned transcript directories to delete (%d), under %q, with no database row:\n", len(orphanDirs), pruneSyncDir(cmd))
	for _, orphan := range orphanDirs {
		fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", orphan)
	}
}

// pruneOrphanStore is the read the orphan cleanup re-checks against.
type pruneOrphanStore interface {
	QueryPrunableSessions(ctx context.Context, filter ingest.PruneFilter) ([]ingest.PruneSessionRow, error)
}

var _ pruneOrphanStore = (*store.Store)(nil)

// pruneOrphanOutcome is what removePruneOrphans did with the confirmed list.
type pruneOrphanOutcome struct {
	removed []string
	errs    []error
}

// removePruneOrphans removes the confirmed orphan list and nothing else. It
// never re-scans the tree, so a directory that appeared after enumeration is
// never touched.
//
// Just before removing, it re-reads the store and drops any listed directory
// that has gained a database row since it was enumerated: a session ingested
// while the user was deciding is live, not an orphan. The re-check can only
// shrink the list. When it cannot be made, nothing is removed.
func removePruneOrphans(ctx context.Context, cmd *cobra.Command, db pruneOrphanStore, orphanDirs []string) pruneOrphanOutcome {
	var outcome pruneOrphanOutcome
	if len(orphanDirs) == 0 {
		return outcome
	}
	rows, err := db.QueryPrunableSessions(ctx, ingest.PruneFilter{All: true})
	if err != nil {
		outcome.errs = append(outcome.errs, fmt.Errorf("re-check the %d orphaned transcript director(ies) against the database before removing them: %w; none of them was removed; re-run `peasant prune --all --orphans` to retry", len(orphanDirs), err))
		return outcome
	}
	adopted := make(map[string]bool, len(rows))
	for _, row := range rows {
		adopted[string(row.SessionID)] = true
	}

	syncDir := pruneSyncDir(cmd)
	parentDirs := make(map[string]struct{})
	for _, orphan := range orphanDirs {
		if adopted[filepath.Base(orphan)] {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: kept %s: it gained a database row after the preview, so it is a live session rather than an orphan\n", orphan)
			continue
		}
		dir := filepath.Join(syncDir, orphan)
		if err := os.RemoveAll(dir); err != nil && !os.IsNotExist(err) {
			outcome.errs = append(outcome.errs, fmt.Errorf("remove orphaned transcript directory %s: %w", dir, err))
			continue
		}
		outcome.removed = append(outcome.removed, orphan)
		parentDirs[filepath.Dir(dir)] = struct{}{}
	}
	// Remove host slug directories the removal emptied. os.Remove only
	// succeeds on an empty directory.
	for dir := range parentDirs {
		_ = os.Remove(dir)
	}
	return outcome
}

// orphanDirsToJSON keeps an empty list as [] rather than null in JSON output.
func orphanDirsToJSON(orphanDirs []string) []string {
	if orphanDirs == nil {
		return []string{}
	}
	return orphanDirs
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
