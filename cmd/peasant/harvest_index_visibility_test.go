package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// indexFailureRemedy is the action the warning tells a user to take.
//
// Written out here rather than read from cmd_harvest.go: a needle taken from the
// code it checks is two operands from one source and passes however the code is
// edited. This one fails if the remedy is dropped or reworded, which is the
// point - the warning names a silent failure, so it has to name the one command
// that reports the reason for each session.
const indexFailureRemedy = "Run 'peasant harvest --json' for the reason on each."

// A session that imports but cannot be INDEXED is empty everywhere downstream -
// the transcript viewer, search, metrics, and anything published. The dispatch
// refuses rather than guessing, and its message says so in as many words:
// "no entries were indexed for this session, and the run says so instead of
// guessing."
//
// The run did not say so. The summary counts index failures as neither errors
// nor indexed sessions, so it printed "1 sessions (1 new, ..., 0 errors)" and
// SKIPPED the index line entirely, because that line was gated on Indexed > 0.
// Exit code 0. The warning carrying the whole actionable message goes to slog,
// which harvest redirects to io.Discard whenever the progress renderer is a TTY -
// so it is thrown away on exactly the interactive run a person is watching. The
// only surfaces that carried it were the index_log table and --json.
//
// This drives the real summary writer over an index log containing a refusal.
func TestHarvestSummary_ReportsSessionsThatImportedButWereNotIndexed(t *testing.T) {
	t.Parallel()
	failed := "ingest: cannot index session: its indexer did not declare where its entries come from"
	log := []ingest.IndexLogEntry{
		{SessionID: "s1", Outcome: ingest.IndexOutcomeError, ErrorMessage: &failed},
	}
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{New: 1, Indexed: 0, Computed: 0},
		IndexLog: log,
	}

	var out strings.Builder
	printSummary(&out, result, false, false, t.TempDir(), "", nil, 0)
	printed := out.String()

	if !strings.Contains(printed, "NOT indexed") {
		t.Errorf("a session imported and was never indexed, and the run does not say so. It is stored empty: absent from "+
			"the viewer, from search, from metrics, and from anything published, while the summary reports a clean "+
			"import.\ngot:\n%s", printed)
	}
	// The index line itself must appear too, or the counts that explain the
	// warning are missing.
	if !strings.Contains(printed, "index:") {
		t.Errorf("the index line is suppressed on a run where indexing failed, which is the run it matters most on:\n%s",
			printed)
	}
}

// TestHarvestSummary_SaysNothingExtraWhenIndexingSucceeded is the over-reporting
// half: a clean run must not grow a warning. A summary that always warns is as
// useless as one that never does.
func TestHarvestSummary_SaysNothingExtraWhenIndexingSucceeded(t *testing.T) {
	t.Parallel()
	result := &ingest.PipelineResult{
		Summary:  ingest.PipelineSummary{New: 1, Indexed: 1, Computed: 1},
		IndexLog: []ingest.IndexLogEntry{{SessionID: "s1", Outcome: ingest.IndexOutcomeIndexed}},
	}
	var out strings.Builder
	printSummary(&out, result, false, false, t.TempDir(), "", nil, 0)
	if printed := out.String(); strings.Contains(printed, "NOT indexed") {
		t.Errorf("a fully indexed run still warns about unindexed sessions:\n%s", printed)
	}
}

// TestHarvestSummary_PrintsTheNumberOfSessionsItCounted drives the real summary
// writer over every shape an index log can take.
//
// The defect the counter exists to correct was a WRONG PRINTED NUMBER: the run
// said "1 sessions (1 new, ...)" and then, on the very next line, "2 session(s)
// were imported but NOT indexed" - falsifiable against the line directly above
// it. countIndexFailures was then pinned by a nine-row corpus and the PRINTED
// number was not: the two tests above drive the real renderer but assert only
// that "NOT indexed" and "index:" appear, over a log carrying one failing
// session, so neither could tell a count over log ROWS from a count over
// SESSIONS. The one value the whole fix exists to correct was the one value the
// surface did not read.
//
// So the same corpus now feeds the counter AND the render. Cases where one
// session records several rows make the two counts different numbers at the
// surface a user reads them on.
//
// ITS ACCEPTANCE TEST, stated as the production edits that must turn it red:
// printing len(result.IndexLog) rather than countIndexFailures' result; dropping
// the count from the warning; dropping the remedy from it.
func TestHarvestSummary_PrintsTheNumberOfSessionsItCounted(t *testing.T) {
	t.Parallel()
	document, err := loadIndexFailureCountFixture(indexFailureCountFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			log := make([]ingest.IndexLogEntry, 0, len(testCase.Rows))
			indexed := map[ingest.SessionID]bool{}
			sessions := map[ingest.SessionID]bool{}
			for _, row := range testCase.Rows {
				id := ingest.SessionID(row.Session)
				log = append(log, ingest.IndexLogEntry{SessionID: id, Outcome: logOutcomes[row.Outcome]})
				sessions[id] = true
				// Built through the production classifier so the summary counts
				// this run reports agree with the log it is handed, rather than
				// with a second opinion written here.
				if IndexOutcomeEndedIndexed(logOutcomes[row.Outcome]) {
					indexed[id] = true
				}
			}
			result := &ingest.PipelineResult{
				Summary: ingest.PipelineSummary{
					New:     len(sessions),
					Indexed: len(indexed),
				},
				IndexLog: log,
			}

			var out strings.Builder
			printSummary(&out, result, false, false, t.TempDir(), "", nil, 0)
			printed := out.String()

			if testCase.WantCount == 0 {
				if strings.Contains(printed, "NOT indexed") {
					t.Errorf("this run has nothing left unindexed, and the summary warns anyway. A warning that fires on a "+
						"clean run is as useless as one that never fires.\nlog: %+v\ngot:\n%s", testCase.Rows, printed)
				}
				return
			}
			// The count, inside the sentence that carries it. It cannot be
			// satisfied by the session total one line above, which is what the
			// shipped defect contradicted.
			warning := fmt.Sprintf("%d session(s) were imported but NOT indexed", testCase.WantCount)
			if !strings.Contains(printed, warning) {
				t.Errorf("the summary must warn %q. This log records %d row(s) across %d session(s), so a count over rows "+
					"prints a different number here - and the number it prints sits directly beneath the session total, "+
					"where a user can falsify it at a glance.\nlog: %+v\ngot:\n%s",
					warning, len(testCase.Rows), len(sessions), testCase.Rows, printed)
			}
			if !strings.Contains(printed, indexFailureRemedy) {
				t.Errorf("the warning names a failure the run is otherwise silent about, and then does not say how to see "+
					"the reason for it. It must carry %q.\ngot:\n%s", indexFailureRemedy, printed)
			}
		})
	}
}
