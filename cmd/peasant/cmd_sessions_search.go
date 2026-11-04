package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// Numeric flags stay strings until validation so malformed integers, like
// out-of-range integers, become usage errors rather than pflag runtime errors.
type sessionsSearchFlags struct {
	limit, offset                  string
	project, harness, since, until string
	asJSON                         bool
}

type sessionsSearchPlan struct {
	query   string
	window  int
	offset  int
	filters codemap.SearchFilters
	asJSON  bool
	// inputWarnings names each session a --sessions-from review document
	// left out, so the run reports it rather than dropping it silently.
	inputWarnings []clicontract.Warning
}

type sessionsSearchOutput struct {
	clicontract.Meta
	schema.SearchPayload
}

func buildSessionsSearchCommand() *cobra.Command {
	var f sessionsSearchFlags
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search indexed session text by relevance",
		Long: "Search ranked full-text over indexed transcript text, already redacted at\n" +
			"ingest and potentially preview-capped. FTS matches whole tokens, not substrings\n" +
			"or regular expressions. Quote multi-word queries. Session ids and project hashes\n" +
			"resolve directly, ignoring scope filters with a warning.\n\n" +
			"At most " + strconv.Itoa(codemap.SearchMaxLimit) + " rows are returned per page; a larger --limit is clamped, and\n" +
			"--offset addresses at most " + strconv.Itoa(codemap.SearchMaxOffset) + " rows into the ranked stream. Unlike sessions\n" +
			"list, --limit 0 is refused, and subagent sessions are never hidden. A date-only\n" +
			"--until covers that whole day. Output is local and receives no publish-time\n" +
			"redaction pass. --json preserves the search payload (including score) alongside\n" +
			"warnings and explicit empty reasons.",
		// Errors belong on stderr, never a usage dump on JSON's stdout.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := buildSessionsSearchPlan(cmd, args, f)
			if err != nil {
				return err
			}
			return runSessionsSearch(cmd, plan)
		},
	}
	cmd.Flags().StringVar(&f.limit, "limit", strconv.Itoa(codemap.SearchDefaultLimit), "Maximum ranked rows per page (integer >= 1; values above "+strconv.Itoa(codemap.SearchMaxLimit)+" are clamped; 0 is refused)")
	cmd.Flags().StringVar(&f.offset, "offset", "0", "Ranked rows to skip (integer 0.."+strconv.Itoa(codemap.SearchMaxOffset)+")")
	cmd.Flags().StringVar(&f.project, "project", "", "Filter by project using the sessions list match (git remote URL or directory basename)")
	cmd.Flags().StringVar(&f.harness, "harness", "", "Filter by harness ("+joinHarnesses(defaults.AllHarnesses)+")")
	cmd.Flags().StringVar(&f.since, "since", "", "Only sessions starting after this time ("+timeFilterHelp+")")
	cmd.Flags().StringVar(&f.until, "until", "", "Only sessions starting before this time; a date includes that whole day ("+timeFilterHelp+")")
	cmd.Flags().BoolVar(&f.asJSON, defaults.JSONFlagName, false, "Output one JSON document, with score and CLI metadata")
	clicontract.RegisterSessionsFrom(cmd)
	return cmd
}

// buildSessionsSearchPlan resolves every input before any store is opened.
func buildSessionsSearchPlan(cmd *cobra.Command, args []string, f sessionsSearchFlags) (*sessionsSearchPlan, error) {
	if len(args) != 1 {
		return nil, clicontract.NewUsageError(fmt.Sprintf("%s takes one search query, got %d; wrap a multi-word query in quotes", cmd.CommandPath(), len(args)))
	}
	// Count bytes, as the HTTP route and codemap's query guard do.
	if len(strings.TrimSpace(args[0])) < 2 {
		return nil, clicontract.NewUsageError("The search request could not start because the query has fewer than two non-whitespace characters in cmd/peasant.buildSessionsSearchPlan. No store was opened. Enter at least two characters and retry.")
	}
	window, err := strconv.Atoi(f.limit)
	if err != nil || window < 1 {
		return nil, clicontract.NewUsageError(fmt.Sprintf("invalid --limit value %q: use an integer >= 1; values above %d are clamped, and 0 does not mean no limit for sessions search. No store was opened.", f.limit, codemap.SearchMaxLimit))
	}
	window = codemap.ClampSearchLimit(window)
	offset, err := strconv.Atoi(f.offset)
	if err != nil || offset < 0 {
		return nil, clicontract.NewUsageError(fmt.Sprintf("invalid --offset value %q: use an integer >= 0 counting ranked rows to skip. No store was opened.", f.offset))
	}
	if offset > codemap.SearchMaxOffset {
		return nil, clicontract.NewUsageError(fmt.Sprintf("invalid --offset value %q: a ranked page can start at most %d rows into the stream, because one page materialises limit+offset rows per index before it pages them. Narrow the query instead of paging deeper. No store was opened.", f.offset, codemap.SearchMaxOffset))
	}
	list, err := buildSessionListFilter(f.project, "", f.since, f.until, f.harness, "", string(defaults.SessionSortDate), false, 0)
	if err != nil {
		return nil, clicontract.NewUsageError(err.Error() + "; correct the filter and retry. No store was opened.")
	}
	fromList, _, err := clicontract.ResolveSessionsFrom(cmd)
	if err != nil {
		return nil, err
	}
	ids := fromList.IDs
	for i, id := range ids {
		if _, err := ingest.NewSessionID(id); err != nil {
			return nil, clicontract.NewUsageError(fmt.Sprintf("invalid session id %q at position %d in --sessions-from: %v; correct the input list and retry. No store was opened.", id, i+1, err))
		}
	}
	return &sessionsSearchPlan{
		query: args[0], window: window, offset: offset, asJSON: f.asJSON,
		filters:       codemap.SearchFilters{ProjectName: list.ProjectName, Harness: list.ModelHarness, StartFromMs: list.StartFrom, StartBeforeMs: list.StartBefore, SessionIDs: ids},
		inputWarnings: fromList.Warnings(),
	}, nil
}

func runSessionsSearch(cmd *cobra.Command, plan *sessionsSearchPlan) error {
	db, cleanup, err := openDBForRead(cmd)
	if err != nil {
		return err
	}
	defer cleanup()
	svc := codemap.NewService(db, nil, nil)
	payload, err := svc.SearchRankedWindowFiltered(cmd.Context(), plan.query, plan.window, plan.offset, plan.filters)
	if err != nil {
		return fmt.Errorf("sessions search: %w", err)
	}
	warnings := clicontract.NewCollector(cmd.ErrOrStderr(), plan.asJSON)
	for _, warning := range plan.inputWarnings {
		warnings.Warn(warning)
	}
	if len(payload.Results) > 0 && codemap.IsIdentityQuery(plan.query) && !plan.filters.IsZero() {
		warnings.Warn(clicontract.Warning{Code: clicontract.WarningFilterIgnored, Message: "the query names a session id or project hash, so the filters did not apply", SessionID: string(payload.Results[0].SessionID)})
	}
	var empty clicontract.EmptyCode
	var reason string
	if len(payload.Results) == 0 {
		empty, reason, err = sessionsSearchEmptyReason(cmd, db, svc, plan)
		if err != nil {
			return fmt.Errorf("sessions search: explain empty result: %w", err)
		}
	}
	if plan.asJSON {
		return clicontract.WriteJSON(cmd.OutOrStdout(), sessionsSearchOutput{Meta: warnings.Meta(empty, reason), SearchPayload: *payload})
	}
	if reason != "" {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), reason)
		return err
	}
	renderSessionsSearchTable(cmd, payload, plan)
	return nil
}

func sessionsSearchEmptyReason(cmd *cobra.Command, db *store.Store, svc *codemap.Service, plan *sessionsSearchPlan) (clicontract.EmptyCode, string, error) {
	ctx := cmd.Context()
	anySessions, err := db.HasAnySessions(ctx)
	if err != nil {
		return "", "", err
	}
	if !anySessions {
		return clicontract.EmptyStoreHasNoSessions, "the store holds no sessions; run `peasant harvest` to import sessions", nil
	}
	offsetReason := fmt.Sprintf("no ranked row exists at offset %d for %q; rerun with a smaller --offset", plan.offset, plan.query)
	identity := codemap.IsIdentityQuery(plan.query)
	if identity && plan.offset > 0 {
		return clicontract.EmptyNoMatchingEntries, offsetReason, nil
	}
	// Identity lookup is filter-blind and does not query FTS, even for an
	// unknown identifier. Do not probe transcript tokens for these queries.
	if !identity {
		streamNonEmpty := false
		if plan.offset > 0 {
			streamNonEmpty, err = svc.SearchMatchesAny(ctx, plan.query, plan.filters)
			if err != nil {
				return "", "", err
			}
		}
		// At offset 0 the empty page already proves the filtered stream empty.
		if !plan.filters.IsZero() && !streamNonEmpty {
			outside, err := svc.SearchMatchesAny(ctx, plan.query, codemap.SearchFilters{})
			if err != nil {
				return "", "", err
			}
			if outside {
				return clicontract.EmptyFiltersExcludedAll, fmt.Sprintf("no entry matches %q inside the filters, but it matches outside them; drop or widen the filters", plan.query), nil
			}
		}
		if plan.offset > 0 && streamNonEmpty {
			return clicontract.EmptyNoMatchingEntries, offsetReason, nil
		}
	}
	if identity {
		return clicontract.EmptyNoMatchingEntries, fmt.Sprintf("no stored session or project has identifier %q; check the identifier or run `peasant sessions list`", plan.query), nil
	}
	return clicontract.EmptyNoMatchingEntries, fmt.Sprintf("no indexed entry matches %q; FTS matches whole tokens, not substrings, so try a shorter or different term; if sessions were just ingested, run `peasant harvest index`", plan.query), nil
}

// searchTableCell collapses permitted line whitespace only after escaping other
// terminal controls. The JSON payload is never changed by table formatting.
func searchTableCell(text string) string {
	return strings.Join(strings.Fields(escapePromptControls(text)), " ")
}

func renderSessionsSearchTable(cmd *cobra.Command, payload *schema.SearchPayload, plan *sessionsSearchPlan) {
	rows := make([][5]string, 0, len(payload.Results)+1)
	rows = append(rows, [5]string{"SESSION", "PROJECT", "ROLE", "ENTRY", "SNIPPET"})
	for _, result := range payload.Results {
		id := string(result.SessionID)
		if strings.TrimSpace(plan.query) != id {
			id = store.TruncateToRunes(id, 12)
		}
		project := strings.TrimRight(result.Project, "/\\")
		if i := strings.LastIndexAny(project, "/\\"); i >= 0 {
			project = project[i+1:]
		}
		snippet := searchTableCell(result.Snippet)
		if snippet == "" {
			snippet = "—"
		}
		if utf8.RuneCountInString(snippet) > 120 {
			snippet = store.TruncateToRunes(snippet, 120) + "…"
		}
		rows = append(rows, [5]string{id, searchTableCell(project), searchTableCell(string(result.Role)), strconv.Itoa(result.EntryIndex), snippet})
	}
	var widths [4]int
	for _, row := range rows {
		for i := range widths {
			widths[i] = max(widths[i], utf8.RuneCountInString(row[i]))
		}
	}
	out := cmd.OutOrStdout()
	for n, row := range rows {
		fmt.Fprintf(out, "%-*s  %-*s  %-*s  %*s  %s\n", widths[0], row[0], widths[1], row[1], widths[2], row[2], widths[3], row[3], row[4])
		if n == 0 {
			fmt.Fprintf(out, "%s  %s  %s  %s  %s\n", strings.Repeat("-", widths[0]), strings.Repeat("-", widths[1]), strings.Repeat("-", widths[2]), strings.Repeat("-", widths[3]), strings.Repeat("-", 12))
		}
	}
	if len(payload.Results) == plan.window && !codemap.IsIdentityQuery(plan.query) {
		fmt.Fprintf(out, "\nnext page: --offset %d\n", plan.offset+len(payload.Results))
	}
}
