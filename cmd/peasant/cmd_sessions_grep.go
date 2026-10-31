package main

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

const (
	grepDefaultMaxMatches = 100
	grepDefaultPerSession = 10
	grepDefaultSnippet    = 200
	grepMaxSnippet        = 2000
)

// sessionGrepMatch is one match in the JSON document.
type sessionGrepMatch struct {
	SessionID        string `json:"sessionId"`
	EntryIndex       int    `json:"entryIndex"`
	Role             string `json:"role"`
	Depth            int    `json:"depth"`
	Field            string `json:"field"`
	Snippet          string `json:"snippet"`
	SnippetTruncated bool   `json:"snippetTruncated"`
}

// sessionGrepOutput is the JSON document of the grep command.
type sessionGrepOutput struct {
	clicontract.Meta
	Pattern         string             `json:"pattern"`
	Mode            string             `json:"mode"`
	IgnoreCase      bool               `json:"ignoreCase"`
	Matches         []sessionGrepMatch `json:"matches"`
	SessionsScanned int                `json:"sessionsScanned"`
	Truncated       bool               `json:"truncated"`
	Reason          string             `json:"reason"`
}

// sessionGrepOptions holds the raw flag values.
type sessionGrepOptions struct {
	regexp, ignoreCase, asJSON      bool
	roles, entryTypes               []string
	maxMatches, perSession, snippet int
	session, project, since, until  string
	harness                         string
}

// sessionGrepPlan is a validated invocation. Building one touches no store.
type sessionGrepPlan struct {
	query     store.EntryMatchQuery
	filter    store.SessionListFilter
	requested []string // ids named explicitly, in the order given
	explicit  bool     // the scope was narrowed by --sessions-from or --session
}

// buildSessionsGrepCommand constructs `peasant sessions grep`.
func buildSessionsGrepCommand() *cobra.Command {
	var opts sessionGrepOptions

	cmd := &cobra.Command{
		Use:   "grep <pattern>",
		Short: "Search entries across sessions",
		Long: `Search the stored entries of many sessions for a literal string or a Go
regular expression, and print each match with its session id, entry index, role,
depth and a bounded snippet.

The search covers entry content, tool input and tool output, in main sessions and
subagent sessions alike. The default is a case-sensitive literal match. --regexp
selects RE2 syntax, which has no backtracking blow-up, so a pattern cannot hang
the command. Each field of an entry reports its first match.

A session's text is searched whole when its capture holds it whole. A session
that holds only bounded previews is searched as stored and gets the warning
capture_bounded, so a match past the preview cannot be found.

This command prints text from the store and does not redact it. The output is
local and is as sensitive as sessions context. It reads the store only, and it
does not search session files on disk.

--sessions-from limits the search to a list of sessions and intersects with
--project, --since, --until and --harness. It cannot be combined with --session.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return nil
			}
			return clicontract.NewUsageError(fmt.Sprintf(
				"%s takes one pattern, got %d\n"+
					"Fix: quote the pattern as one argument.",
				cmd.CommandPath(), len(args)))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := planSessionsGrep(cmd, args[0], opts)
			if err != nil {
				return err
			}
			return runSessionsGrep(cmd, args[0], opts, plan)
		},
	}

	flags := cmd.Flags()
	flags.BoolVar(&opts.regexp, "regexp", false, "Treat the pattern as a Go (RE2) regular expression")
	flags.BoolVar(&opts.ignoreCase, "ignore-case", false, "Match without regard to case")
	flags.StringSliceVar(&opts.roles, "role", nil, "Only search these roles (user, assistant, tool, system)")
	flags.StringSliceVar(&opts.entryTypes, "entry-type", nil, "Only search these entry types (text, tool_use, tool_result, thinking, system, error, result)")
	flags.IntVar(&opts.maxMatches, "max-matches", grepDefaultMaxMatches, "Stop after this many matches (0 = no limit)")
	flags.IntVar(&opts.perSession, "max-per-session", grepDefaultPerSession, "Keep at most this many matches per session (0 = no limit)")
	flags.IntVar(&opts.snippet, "snippet-chars", grepDefaultSnippet, fmt.Sprintf("Characters of text in each snippet, around the match (1 to %d)", grepMaxSnippet))
	clicontract.RegisterSessionsFrom(cmd)
	flags.StringVar(&opts.session, "session", "", "Search one session id")
	flags.StringVar(&opts.project, "project", "", "Filter by project name (matches git remote URL or directory basename)")
	flags.StringVar(&opts.since, "since", "", "Search sessions starting after this date (e.g. 7d, 24h, 2026-01-01)")
	flags.StringVar(&opts.until, "until", "", "Search sessions starting before this date (e.g. 7d, 24h, 2026-01-01)")
	flags.StringVar(&opts.harness, "harness", "", "Filter by harness (claude-code, gemini-cli, codex, opencode)")
	flags.BoolVar(&opts.asJSON, defaults.JSONFlagName, false, "Output as one JSON document")
	return cmd
}

// planSessionsGrep validates every flag. Each refusal is a usage error, raised
// before the store opens.
func planSessionsGrep(cmd *cobra.Command, pattern string, opts sessionGrepOptions) (sessionGrepPlan, error) {
	var plan sessionGrepPlan
	refuse := func(format string, args ...any) (sessionGrepPlan, error) {
		return plan, clicontract.NewUsageError(fmt.Sprintf(format, args...))
	}
	if pattern == "" {
		return refuse("the pattern is empty: it would match every entry\nFix: give a pattern to search for.")
	}
	expression := pattern
	if !opts.regexp {
		expression = regexp.QuoteMeta(pattern)
	}
	if opts.ignoreCase {
		expression = "(?i)" + expression
	}
	compiled, err := regexp.Compile(expression)
	if err != nil {
		return refuse("invalid regular expression %q: %v\nFix: use Go RE2 syntax, or drop --regexp for a literal match.", pattern, err)
	}
	if opts.maxMatches < 0 {
		return refuse("invalid --max-matches value %d: it must be 0 (no limit) or more", opts.maxMatches)
	}
	if opts.perSession < 0 {
		return refuse("invalid --max-per-session value %d: it must be 0 (no limit) or more", opts.perSession)
	}
	if opts.snippet < 1 || opts.snippet > grepMaxSnippet {
		return refuse("invalid --snippet-chars value %d: it must be from 1 to %d", opts.snippet, grepMaxSnippet)
	}
	roles := make([]schema.Role, 0, len(opts.roles))
	for _, raw := range opts.roles {
		role := schema.Role(raw)
		if !role.IsValid() {
			return refuse("invalid --role value %q: must be one of user, assistant, tool, system", raw)
		}
		roles = append(roles, role)
	}
	entryTypes := make([]schema.EntryType, 0, len(opts.entryTypes))
	for _, raw := range opts.entryTypes {
		entryType := schema.EntryType(raw)
		if !entryType.IsValid() {
			return refuse("invalid --entry-type value %q: must be one of text, tool_use, tool_result, thinking, system, error, result", raw)
		}
		entryTypes = append(entryTypes, entryType)
	}
	listed, set, err := clicontract.ResolveSessionsFrom(cmd)
	if err != nil {
		return plan, err
	}
	if opts.session != "" {
		listed = []string{opts.session}
	}
	for _, id := range listed {
		if _, err := schema.NewSessionID(id); err != nil {
			return refuse("invalid session id %q: %v\nFix: use `peasant sessions list` to find a valid session id.", id, err)
		}
	}
	filter, err := buildSessionListFilter(opts.project, opts.session, opts.since, opts.until, opts.harness, "", string(defaults.SessionSortDate), false, 0)
	if err != nil {
		return refuse("%v", err)
	}
	plan.filter = filter
	plan.requested = listed
	plan.explicit = set || opts.session != ""
	plan.query = store.EntryMatchQuery{
		Pattern: compiled, Roles: roles, EntryTypes: entryTypes,
		MaxMatches: opts.maxMatches, PerSession: opts.perSession, SnippetChars: opts.snippet,
	}
	return plan, nil
}

// runSessionsGrep runs a validated plan. It writes nothing to the store.
func runSessionsGrep(cmd *cobra.Command, pattern string, opts sessionGrepOptions, plan sessionGrepPlan) error {
	ctx := cmd.Context()
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	warnings := clicontract.NewCollector(cmd.ErrOrStderr(), opts.asJSON)
	plan.query.SessionIDs, err = resolveGrepScope(cmd, db, plan, warnings)
	if err != nil {
		return err
	}
	result, err := db.MatchEntries(ctx, plan.query)
	if err != nil {
		return fmt.Errorf("sessions grep: %w", err)
	}
	for _, id := range result.BoundedSessions {
		warnings.Warn(clicontract.Warning{
			Code:      clicontract.WarningCaptureBounded,
			Message:   "this capture stores bounded previews, so text past a preview was not searched; run `peasant harvest --force` to recapture the session from its source",
			SessionID: string(id),
		})
	}
	for _, skip := range result.Skipped {
		warnings.Warn(clicontract.Warning{
			Code:      clicontract.WarningSessionRefused,
			Message:   "this session could not be searched to its end: " + skip.Reason,
			SessionID: string(skip.SessionID),
		})
	}

	out := sessionGrepOutput{
		Pattern: pattern, Mode: "literal", IgnoreCase: opts.ignoreCase,
		Matches:         make([]sessionGrepMatch, 0, len(result.Matches)),
		SessionsScanned: result.SessionsScanned,
		Truncated:       result.HitMaxMatches || result.HitPerSession,
	}
	if opts.regexp {
		out.Mode = "regexp"
	}
	for _, match := range result.Matches {
		out.Matches = append(out.Matches, sessionGrepMatch{
			SessionID: string(match.SessionID), EntryIndex: match.EntryIndex, Role: match.Role, Depth: match.Depth,
			Field: match.Field, Snippet: match.Snippet, SnippetTruncated: match.SnippetTruncated,
		})
	}
	emptyCode, reason, err := explainGrepResult(cmd, db, plan, opts, result)
	if err != nil {
		return err
	}
	out.Reason = reason

	if opts.asJSON {
		out.Meta = warnings.Meta(emptyCode, "")
		if emptyCode != "" {
			out.EmptyReason = reason
		}
		return clicontract.WriteJSON(cmd.OutOrStdout(), out)
	}
	if reason != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", reason)
	}
	for _, match := range out.Matches {
		fmt.Fprintf(cmd.OutOrStdout(), "%s [%d] %s depth=%d  %s\n",
			match.SessionID, match.EntryIndex, match.Role, match.Depth, escapeSnippetControls(match.Snippet))
	}
	return nil
}

// resolveGrepScope turns the list filter and any named ids into the sessions
// to read, newest first. It lists every session, subagent sessions included: a
// search that skipped them would report a clean result for text they hold. An
// id the store does not hold is named in a warning; one the filters exclude is
// the intersection doing its job.
func resolveGrepScope(cmd *cobra.Command, db *store.Store, plan sessionGrepPlan, warnings *clicontract.Collector) ([]schema.SessionID, error) {
	ctx := cmd.Context()
	rows, err := db.ListSessionsFiltered(ctx, plan.filter)
	if err != nil {
		return nil, fmt.Errorf("sessions grep: %w", err)
	}
	ids := make([]schema.SessionID, 0, len(rows))
	inScope := make(map[string]bool, len(rows))
	for _, row := range rows {
		inScope[row.SessionID] = true
		if !plan.explicit || slices.Contains(plan.requested, row.SessionID) {
			ids = append(ids, schema.SessionID(row.SessionID))
		}
	}
	for _, id := range plan.requested {
		if inScope[id] {
			continue
		}
		row, err := db.SessionByID(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("sessions grep: %w", err)
		}
		if row == nil {
			warnings.Warn(clicontract.Warning{
				Code:      clicontract.WarningSessionRefused,
				Message:   "the store holds no such session",
				SessionID: id,
			})
		}
	}
	return ids, nil
}

// explainGrepResult says why a result is empty or cut.
func explainGrepResult(cmd *cobra.Command, db *store.Store, plan sessionGrepPlan, opts sessionGrepOptions, result store.EntryMatchResult) (clicontract.EmptyCode, string, error) {
	switch {
	case result.HitMaxMatches:
		return "", fmt.Sprintf("stopped at --max-matches %d: more matches exist", opts.maxMatches), nil
	case result.HitPerSession:
		return "", fmt.Sprintf("at least one session has more matches than --max-per-session %d", opts.perSession), nil
	case len(result.Matches) > 0:
		return "", "", nil
	case len(plan.query.SessionIDs) > 0:
		return clicontract.EmptyNoMatchingEntries, fmt.Sprintf("no entry in %s matched", sessionCountWords(result.SessionsScanned)), nil
	}
	stored, err := db.CountSessionsFiltered(cmd.Context(), store.SessionListFilter{})
	if err != nil {
		return "", "", fmt.Errorf("sessions grep: %w", err)
	}
	if stored == 0 {
		return clicontract.EmptyStoreHasNoSessions, "the store holds no session; run `peasant harvest` first", nil
	}
	return clicontract.EmptyFiltersExcludedAll, "scope selected 0 sessions", nil
}

func sessionCountWords(count int) string {
	if count == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", count)
}

// escapeSnippetControls shows control characters as escapes, so one match stays
// on one line and stored text cannot drive the terminal.
func escapeSnippetControls(text string) string {
	var b strings.Builder
	for _, r := range text {
		if !unicode.IsControl(r) {
			b.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteRune(r)
		b.WriteString(quoted[1 : len(quoted)-1])
	}
	return b.String()
}
