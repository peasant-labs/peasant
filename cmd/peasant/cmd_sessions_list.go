package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/spf13/cobra"
)

// sessionListEntry is the JSON-serializable representation of a session for the list command.
type sessionListEntry struct {
	ID      string `json:"id"`
	Date    string `json:"date"`
	Project string `json:"project"`
	Turns   int    `json:"turns"`
	Tokens  int    `json:"tokens"`
	Preview string `json:"preview"`
}

// buildSessionsListCommand constructs the `peasant sessions list` subcommand.
func buildSessionsListCommand() *cobra.Command {
	var (
		project string
		since   string
		until   string
		harness string
		tag     string
		sort    string
		reverse bool
		limit   int
		asJSON  bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		Long:  "List ingested sessions with optional filtering. Outputs a table by default or JSON with --json.",
		RunE: func(cmd *cobra.Command, args []string) error {
			db, cleanup, err := openDB(cmd)
			if err != nil {
				return err
			}
			defer cleanup()

			f, err := buildSessionListFilter(project, since, until, harness, tag, sort, reverse, limit)
			if err != nil {
				return err
			}

			return listSessionsShared(cmd, db, f, asJSON)
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "Filter by project name (matches git remote URL or directory basename)")
	cmd.Flags().StringVar(&since, "since", "", "Show sessions starting after this date (e.g. 7d, 24h, 2026-01-01)")
	cmd.Flags().StringVar(&until, "until", "", "Show sessions starting before this date (e.g. 7d, 24h, 2026-01-01)")
	cmd.Flags().StringVar(&harness, "harness", "", "Filter by harness (claude-code, gemini-cli, codex, opencode)")
	cmd.Flags().StringVar(&tag, "tag", "", "Filter by session tag")
	cmd.Flags().StringVar(&sort, "sort", string(defaults.SessionSortDate), "Sort by field (date, turns, tokens, project)")
	cmd.Flags().BoolVar(&reverse, "reverse", false, "Reverse sort order (ascending instead of descending)")
	cmd.Flags().IntVar(&limit, "limit", defaults.SessionListDefaultLimit, "Maximum number of sessions to show (0 = no limit)")
	cmd.Flags().BoolVar(&asJSON, defaults.JSONFlagName, false, "Output as JSON array")

	return cmd
}

// listSessionsShared runs the session listing and renders to cmd's output.
// It is exported for use by the context command to show a session
// list when no session is specified (R15 no-session fallback).
func listSessionsShared(cmd *cobra.Command, db *store.Store, f store.SessionListFilter, asJSON bool) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	rows, err := db.ListSessionsFiltered(ctx, f)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}

	// Build session ID list for bulk preview fetch (one query instead of N).
	sessionIDs := make([]string, len(rows))
	for i, row := range rows {
		sessionIDs[i] = row.SessionID
	}
	previews, pErr := db.FirstUserMessageBulk(ctx, sessionIDs)
	if pErr != nil {
		previews = map[string]string{} // degrade gracefully; entries will show empty preview
	}

	// Build entries with first-user-message previews.
	entries := make([]sessionListEntry, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, sessionListEntry{
			ID:      row.SessionID,
			Date:    formatSessionDate(row.StartMs),
			Project: projectDisplayName(row.CanonicalRemote, row.ProjectName),
			Turns:   row.TurnCount,
			Tokens:  row.TokensTotal,
			Preview: previews[row.SessionID], // empty string when key absent
		})
	}

	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	// Table output.
	if len(entries) == 0 {
		fmt.Fprintln(out, "no sessions found")
		return nil
	}

	printSessionTable(cmd, entries)

	// Footer: count of sessions shown and total when limit was applied.
	if f.Limit > 0 && len(entries) >= f.Limit {
		// We hit the limit — query total count to show "N of M".
		total, err := countTotalSessions(ctx, db, f)
		if err == nil && total > len(entries) {
			fmt.Fprintf(out, "\n%d of %d sessions — use --limit to show more\n", len(entries), total)
		}
	}

	return nil
}

// buildSessionListFilter parses CLI flag values into a SessionListFilter.
func buildSessionListFilter(project, since, until, harness, tag, sort string, reverse bool, limit int) (store.SessionListFilter, error) {
	f := store.SessionListFilter{
		SortDesc: !reverse, // default is DESC (newest first); --reverse flips to ASC
		Limit:    limit,
	}

	// Sort field.
	sortField := defaults.SessionSortField(sort)
	if sort != "" && !sortField.IsValid() {
		return f, fmt.Errorf("invalid --sort value %q: must be one of date, turns, tokens, project", sort)
	}
	f.SortField = sortField

	// Optional string filters.
	if project != "" {
		f.ProjectName = &project
	}
	if tag != "" {
		f.Tag = &tag
	}
	if harness != "" {
		h := defaults.Harness(harness)
		if !h.IsKnown() {
			return f, fmt.Errorf("invalid --harness value %q: must be one of %s",
				harness, joinHarnesses(defaults.AllHarnesses))
		}
		hStr := string(h)
		f.ModelHarness = &hStr
	}

	// Parse --since and --until as duration (7d, 24h) or date (YYYY-MM-DD).
	if since != "" {
		ms, err := parseDateOrDuration(since, true)
		if err != nil {
			return f, fmt.Errorf("invalid --since value %q: %w", since, err)
		}
		f.StartFrom = &ms
	}
	if until != "" {
		ms, err := parseDateOrDuration(until, false)
		if err != nil {
			return f, fmt.Errorf("invalid --until value %q: %w", until, err)
		}
		f.StartBefore = &ms
	}

	return f, nil
}

// parseDateOrDuration parses a date or duration string into a Unix millisecond timestamp.
// Durations are relative to now: "7d" means 7 days ago, "24h" means 24 hours ago.
// fromSide=true computes "now minus duration" (for --since); false also computes "now minus
// duration" (for --until — the cutoff is still in the past, just the upper bound).
func parseDateOrDuration(s string, fromSide bool) (int64, error) {
	now := time.Now()

	// Try RFC3339 / date-only formats first.
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			if fromSide {
				return t.UnixMilli(), nil
			}
			// For --until with a date, use end-of-day.
			return t.Add(24 * time.Hour).UnixMilli(), nil
		}
	}

	// Parse as duration: support "7d", "24h", "30m", standard Go duration + days.
	d, err := parseDurationWithDays(s)
	if err != nil {
		return 0, fmt.Errorf("expected a duration (e.g. 7d, 24h) or date (YYYY-MM-DD): %w", err)
	}
	// Both --since and --until with a duration subtract from now: "7d" means 7 days ago.
	return now.Add(-d).UnixMilli(), nil
}

// parseDurationWithDays extends time.ParseDuration to support "Nd" (N days) notation.
// "7d" → 7 * 24 * time.Hour. Mixed forms like "7d2h" are NOT supported.
func parseDurationWithDays(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		daysStr := s[:len(s)-1]
		days := 0
		if _, err := fmt.Sscanf(daysStr, "%d", &days); err != nil || days < 0 {
			return 0, fmt.Errorf("invalid day count in %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// printSessionTable writes an aligned tabular output of sessions.
// Columns: ID (full), date (10 chars), project (up to 20 chars), turns, tokens, preview.
func printSessionTable(cmd *cobra.Command, entries []sessionListEntry) {
	out := cmd.OutOrStdout()

	// Compute max column widths for aligned output.
	idWidth := 2   // minimum "ID" header width
	projWidth := 7 // minimum "Project" header width
	for _, e := range entries {
		if len(e.ID) > idWidth {
			idWidth = len(e.ID)
		}
		if n := utf8.RuneCountInString(e.Project); n > projWidth {
			projWidth = n
		}
	}

	// Header.
	fmtStr := fmt.Sprintf("%%-%ds  %%-10s  %%-%ds  %%5s  %%7s  %%s\n", idWidth, projWidth)
	fmt.Fprintf(out, fmtStr, "ID", "Date", "Project", "Turns", "Tokens", "Preview")
	fmt.Fprintln(out, strings.Repeat("-", idWidth+2+10+2+projWidth+2+5+2+7+2+10))
	rowFmt := fmt.Sprintf("%%-%ds  %%-10s  %%-%ds  %%5d  %%7s  %%s\n", idWidth, projWidth)
	for _, e := range entries {
		fmt.Fprintf(out, rowFmt,
			e.ID,
			e.Date,
			e.Project,
			e.Turns,
			formatTokens(e.Tokens),
			e.Preview,
		)
	}
}

// formatSessionDate formats a Unix millisecond timestamp to a YYYY-MM-DD date string.
func formatSessionDate(ms int64) string {
	if ms == 0 {
		return "unknown"
	}
	return time.Unix(ms/1000, 0).UTC().Format("2006-01-02")
}

// projectDisplayName returns the best human-readable project name, using the
// SAME formatter as every other surface (Home/Map picker, breadcrumbs,
// command palette, TUI — internal/projectlabel.Label): prefers
// "host:owner/repo" derived from canonical_remote, falling back to the
// basename of canonical_cwd when no remote is configured. This used to be a
// third, CLI-only implementation that dropped the host prefix and truncated
// GitLab subgroup paths to two segments — a real user-visible divergence
// from the web picker. The CLI keeps
// its OWN basename (not the full path) as the no-remote fallback, since a
// terminal table column favors brevity — that's the one deliberate,
// documented difference from the picker's full-cwd fallback.
func projectDisplayName(remote *string, cwdPath string) string {
	r := ""
	if remote != nil {
		r = *remote
	}
	return projectlabel.Label(r, projectBasename(cwdPath))
}

// projectBasename extracts the last path component from a project path (canonical_cwd).
// If the path is empty, returns "unknown".
func projectBasename(path string) string {
	if path == "" {
		return "unknown"
	}
	// Trim trailing slashes.
	path = strings.TrimRight(path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	return path[idx+1:]
}

// formatTokens formats a token count to a human-readable string (e.g. "1.2k", "4.5M").
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// countTotalSessions returns the total count of sessions matching f (ignoring limit).
// Used for the footer display. Delegates to CountSessionsFiltered which issues a
// SELECT COUNT(*) — O(1) rather than fetching all rows and len()-ing the result.
func countTotalSessions(ctx context.Context, db *store.Store, f store.SessionListFilter) (int, error) {
	unlimited := f
	unlimited.Limit = 0
	return db.CountSessionsFiltered(ctx, unlimited)
}

// joinHarnesses returns a brace-wrapped, comma-separated list of harness identifiers for error messages.
// Example output: {claude-code, gemini-cli, codex, opencode}
func joinHarnesses(harnesses []defaults.Harness) string {
	names := make([]string, len(harnesses))
	for i, h := range harnesses {
		names[i] = string(h)
	}
	return "{" + strings.Join(names, ", ") + "}"
}
