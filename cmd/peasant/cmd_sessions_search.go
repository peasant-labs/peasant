package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
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
}

type sessionsSearchOutput struct {
	clicontract.Meta
	schema.SearchPayload
}

// buildSessionsSearchPlan resolves every input before any store is opened.
func buildSessionsSearchPlan(cmd *cobra.Command, args []string, f sessionsSearchFlags) (*sessionsSearchPlan, error) {
	if len(args) != 1 {
		return nil, clicontract.NewUsageError(fmt.Sprintf("%s takes one search query, got %d; wrap a multi-word query in quotes", cmd.CommandPath(), len(args)))
	}
	// Count bytes, as the HTTP route and codemap's query guard do.
	if len(strings.TrimSpace(args[0])) < 2 {
		return nil, clicontract.NewUsageError("The search request could not start because the query has fewer than two non-whitespace characters in cmd/peasant.runSessionsSearch. No store was opened. Enter at least two characters and retry.")
	}
	window, err := strconv.Atoi(f.limit)
	if err != nil || window < 1 || window > codemap.SearchMaxLimit {
		return nil, clicontract.NewUsageError(fmt.Sprintf("invalid --limit value %q: use an integer in 1..%d; 0 does not mean no limit for sessions search. No store was opened.", f.limit, codemap.SearchMaxLimit))
	}
	offset, err := strconv.Atoi(f.offset)
	if err != nil || offset < 0 {
		return nil, clicontract.NewUsageError(fmt.Sprintf("invalid --offset value %q: use an integer >= 0 counting ranked rows to skip. No store was opened.", f.offset))
	}
	list, err := buildSessionListFilter(f.project, "", f.since, f.until, f.harness, "", string(defaults.SessionSortDate), false, 0)
	if err != nil {
		return nil, clicontract.NewUsageError(err.Error() + "; correct the filter and retry. No store was opened.")
	}
	ids, _, err := clicontract.ResolveSessionsFrom(cmd)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if _, err := ingest.NewSessionID(id); err != nil {
			return nil, clicontract.NewUsageError(fmt.Sprintf("invalid session id %q at position %d in --sessions-from: %v; correct the input list and retry. No store was opened.", id, i+1, err))
		}
	}
	return &sessionsSearchPlan{
		query: args[0], window: window, offset: offset, asJSON: f.asJSON,
		filters: codemap.SearchFilters{ProjectName: list.ProjectName, Harness: list.ModelHarness, StartFromMs: list.StartFrom, StartBeforeMs: list.StartBefore, SessionIDs: ids},
	}, nil
}
