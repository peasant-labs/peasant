package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_failure_counts.yaml
var indexFailureCountFixtureData []byte

// Each of the two requirements this corpus grew when it started driving the
// RENDERED warning gets its own rejection fixture, so the evidence that the
// requirement can fire sits beside the corpus it protects. Both rejection
// corpora satisfy every earlier requirement, or they would prove the wrong
// guard.
var (
	//go:embed testdata/index_failure_counts-reject-singular-only.yaml
	indexFailureCountRejectSingularData []byte
	//go:embed testdata/index_failure_counts-reject-no-row-session-gap.yaml
	indexFailureCountRejectNoGapData []byte
)

const indexFailureCountFixturePath = "cmd/peasant/testdata/index_failure_counts.yaml"

// indexFailureCountFloor is the row count this corpus must not fall below.
const indexFailureCountFloor = 14

type indexFailureCountDocument struct {
	ExpectedCaseCount int                     `yaml:"expectedCaseCount"`
	RequiredCases     []string                `yaml:"required_cases"`
	Cases             []indexFailureCountCase `yaml:"cases"`
}

type indexFailureCountCase struct {
	Name                 string               `yaml:"name"`
	Rows                 []indexFailureLogRow `yaml:"rows"`
	WantCount            int                  `yaml:"wantCount"`
	WantFailed           []string             `yaml:"wantFailed"`
	Entries              map[string]bool      `yaml:"entries"`
	WantEmpty            int                  `yaml:"wantEmpty"`
	WantRetained         int                  `yaml:"wantRetained"`
	WantEmptySessions    []string             `yaml:"wantEmptySessions"`
	WantRetainedSessions []string             `yaml:"wantRetainedSessions"`
}

type indexFailureLogRow struct {
	Session string `yaml:"session"`
	Outcome string `yaml:"outcome"`
}

// logOutcomes maps the corpus's spelling to the production outcome. Explicit,
// because a blank or misspelled value would otherwise decode to the empty
// outcome, which counts as neither a failure nor a success - a row could then
// silently test nothing while reading as a case.
var logOutcomes = map[string]ingest.IndexOutcome{
	"indexed":   ingest.IndexOutcomeIndexed,
	"reindexed": ingest.IndexOutcomeReindexed,
	"fallback":  ingest.IndexOutcomeFallback,
	"skipped":   ingest.IndexOutcomeSkipped,
	"error":     ingest.IndexOutcomeError,
}

func loadIndexFailureCountFixture(data []byte) (indexFailureCountDocument, error) {
	var document indexFailureCountDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, indexFailureCountRuleError("typed YAML fields must match the document schema",
			"loader=first-document decode", fmt.Sprintf("fix=match the typed schema: %v", err))
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, indexFailureCountRuleError("exactly one YAML document is allowed",
			"loader=end-of-document check", fmt.Sprintf("fix=remove the second document: %v", err))
	}
	if len(document.Cases) == 0 || document.ExpectedCaseCount != len(document.Cases) {
		return document, indexFailureCountRuleError(
			fmt.Sprintf("declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d",
				document.ExpectedCaseCount, len(document.Cases)),
			"loader=case-count validation", "fix=set expectedCaseCount to the number of cases present")
	}
	seen := map[string]bool{}
	coveredOutcomes := map[ingest.IndexOutcome]bool{}
	sawMultiRowSingleSession, sawRecovery, sawPrintableRowSessionGap := false, false, false
	sawMoreThanOneFailingSession := false
	sawMixedCoverage, sawAllRetainedCoverage, sawAllEmptyCoverage := false, false, false
	// Deletion protection is by NAME, not by number: a bare count cannot tell
	// a deleted success arm from a corpus that never had it. The required
	// manifest must match the cases exactly, in both directions, so neither
	// dropping a required case nor swapping one out for a filler stays green.
	if len(document.RequiredCases) == 0 {
		return document, indexFailureCountRuleError(
			"no required cases are named",
			"loader=required-name manifest",
			"fix=name every case in required_cases; a corpus with no manifest protects nothing")
	}
	required := map[string]bool{}
	for _, name := range document.RequiredCases {
		if strings.TrimSpace(name) == "" || required[name] {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("required case name %q is blank or repeated", name),
				"loader=required-name manifest",
				"fix=give every required case a unique, behaviour-naming name")
		}
		required[name] = true
	}
	for index, testCase := range document.Cases {
		where := fmt.Sprintf("loader=case index %d", index)
		if strings.TrimSpace(testCase.Name) == "" || seen[testCase.Name] {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("case name %q is missing or duplicated", testCase.Name), where,
				"fix=give every case a unique, behaviour-naming name")
		}
		seen[testCase.Name] = true
		// A case that WARNS and holds more rows than the number it warns about.
		// Without one, the warning renders the same digit whether the summary
		// prints the row count or the session count, and the surface test is
		// green either way - which is how the printed number stayed unpinned
		// while the counter behind it was covered nine ways.
		if testCase.WantCount > 0 && len(testCase.Rows) > testCase.WantCount {
			sawPrintableRowSessionGap = true
		}
		// And a case where the answer is more than one. Without it, everything
		// this corpus asks for is satisfied by a counter that reports whether
		// ANY session failed rather than how many did - and the defect was a
		// wrong NUMBER, not a wrong yes/no.
		if testCase.WantCount > 1 {
			sawMoreThanOneFailingSession = true
		}
		if len(testCase.Rows) == 0 {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("case %q has no log rows", testCase.Name), where,
				"fix=give it at least one; an empty log is the trivial case and pins nothing")
		}
		perSession := map[string][]string{}
		rowSessions := map[string]bool{}
		for _, row := range testCase.Rows {
			if _, known := logOutcomes[row.Outcome]; !known {
				return document, indexFailureCountRuleError(
					fmt.Sprintf("case %q names the outcome %q, which the pipeline does not record", testCase.Name, row.Outcome),
					where, "fix=use one of indexed, reindexed, fallback, skipped, error")
			}
			coveredOutcomes[logOutcomes[row.Outcome]] = true
			perSession[row.Session] = append(perSession[row.Session], row.Outcome)
			rowSessions[row.Session] = true
		}
		// wantFailed is the classified MEMBERSHIP behind wantCount: which
		// sessions the run reports, in first-seen order, not just how many.
		// The two must agree, or the corpus states two answers at once.
		if len(testCase.WantFailed) != testCase.WantCount {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("case %q expects %d session(s) but names %d failed session(s)", testCase.Name, testCase.WantCount, len(testCase.WantFailed)),
				where, "fix=list exactly the failed sessions in wantFailed, in first-seen order")
		}
		seenFailed := map[string]bool{}
		for _, session := range testCase.WantFailed {
			if strings.TrimSpace(session) == "" || seenFailed[session] {
				return document, indexFailureCountRuleError(
					fmt.Sprintf("case %q names failed session %q blank or twice", testCase.Name, session),
					where, "fix=name each failed session once")
			}
			seenFailed[session] = true
			if !rowSessions[session] {
				return document, indexFailureCountRuleError(
					fmt.Sprintf("case %q names %q failed but records no row for it", testCase.Name, session),
					where, "fix=name only sessions the rows record")
			}
		}
		// The coverage split rides on the same cases: per-session entries
		// presence plus independent expected Empty/Retained counts and the
		// classified membership behind them. A case carries the split exactly
		// when it names entries; the loader then pins the section's internal
		// consistency so the coverage test asserts each count against the
		// production computation rather than against a second arithmetic.
		if testCase.Entries != nil {
			for session := range testCase.Entries {
				if !rowSessions[session] {
					return document, indexFailureCountRuleError(
						fmt.Sprintf("case %q records entries presence for %q but no row for it", testCase.Name, session),
						where, "fix=name only sessions the rows record")
				}
			}
			for failed := range seenFailed {
				if _, ok := testCase.Entries[failed]; !ok {
					return document, indexFailureCountRuleError(
						fmt.Sprintf("case %q names %q failed but records no entries presence for it", testCase.Name, failed),
						where, "fix=say whether every failed session holds stored entries; without it the split is undefined")
				}
			}
			if testCase.WantEmpty+testCase.WantRetained != testCase.WantCount {
				return document, indexFailureCountRuleError(
					fmt.Sprintf("case %q expects wantEmpty=%d + wantRetained=%d, which is not wantCount=%d", testCase.Name, testCase.WantEmpty, testCase.WantRetained, testCase.WantCount),
					where, "fix=state the split so it adds up: every failed session is either empty or retained")
			}
			if len(testCase.WantEmptySessions) != testCase.WantEmpty {
				return document, indexFailureCountRuleError(
					fmt.Sprintf("case %q expects wantEmpty=%d but names %d empty session(s)", testCase.Name, testCase.WantEmpty, len(testCase.WantEmptySessions)),
					where, "fix=name exactly the empty sessions in wantEmptySessions, in first-seen order")
			}
			if len(testCase.WantRetainedSessions) != testCase.WantRetained {
				return document, indexFailureCountRuleError(
					fmt.Sprintf("case %q expects wantRetained=%d but names %d retained session(s)", testCase.Name, testCase.WantRetained, len(testCase.WantRetainedSessions)),
					where, "fix=name exactly the retained sessions in wantRetainedSessions, in first-seen order")
			}
			// The two membership lists must partition the failed set, each in
			// first-seen order: filtering wantFailed by each list must give
			// the list back unchanged.
			emptySet := map[string]bool{}
			for _, session := range testCase.WantEmptySessions {
				if emptySet[session] || !seenFailed[session] {
					return document, indexFailureCountRuleError(
						fmt.Sprintf("case %q names empty session %q twice or outside the failed set", testCase.Name, session),
						where, "fix=name each failed session in exactly one of the two membership lists")
				}
				emptySet[session] = true
			}
			retainedSet := map[string]bool{}
			for _, session := range testCase.WantRetainedSessions {
				if retainedSet[session] || emptySet[session] || !seenFailed[session] {
					return document, indexFailureCountRuleError(
						fmt.Sprintf("case %q names retained session %q twice, as empty too, or outside the failed set", testCase.Name, session),
						where, "fix=name each failed session in exactly one of the two membership lists")
				}
				retainedSet[session] = true
			}
			var wantEmptyOrder, wantRetainedOrder []string
			for _, session := range testCase.WantFailed {
				switch {
				case emptySet[session]:
					wantEmptyOrder = append(wantEmptyOrder, session)
				case retainedSet[session]:
					wantRetainedOrder = append(wantRetainedOrder, session)
				default:
					return document, indexFailureCountRuleError(
						fmt.Sprintf("case %q leaves failed session %q in neither membership list", testCase.Name, session),
						where, "fix=name every failed session in exactly one of the two membership lists")
				}
			}
			for i, session := range wantEmptyOrder {
				if testCase.WantEmptySessions[i] != session {
					return document, indexFailureCountRuleError(
						fmt.Sprintf("case %q names empty sessions %v, want first-seen order %v", testCase.Name, testCase.WantEmptySessions, wantEmptyOrder),
						where, "fix=list the membership in the order the log first names the sessions")
				}
			}
			for i, session := range wantRetainedOrder {
				if testCase.WantRetainedSessions[i] != session {
					return document, indexFailureCountRuleError(
						fmt.Sprintf("case %q names retained sessions %v, want first-seen order %v", testCase.Name, testCase.WantRetainedSessions, wantRetainedOrder),
						where, "fix=list the membership in the order the log first names the sessions")
				}
			}
		} else if testCase.WantEmpty != 0 || testCase.WantRetained != 0 || len(testCase.WantEmptySessions) != 0 || len(testCase.WantRetainedSessions) != 0 {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("case %q states coverage expectations without entries presence", testCase.Name),
				where, "fix=either name entries for the failed sessions or drop the split; a split with nothing to compute it from asserts nothing")
		}
		// The split shapes the summary sentences answer to: a mixed case
		// prints both, an all-retained case prints only the retained
		// sentence with a measured Empty zero, an all-empty case only the
		// empty one. Each shape must stay represented.
		//
		// Only a case that carries entries counts: a case without them has no
		// split at all, and its zero-valued WantEmpty would otherwise register
		// as the all-retained shape, making that requirement unfalsifiable.
		if testCase.Entries != nil && testCase.WantCount > 0 {
			switch {
			case testCase.WantEmpty > 0 && testCase.WantRetained > 0:
				sawMixedCoverage = true
			case testCase.WantEmpty == 0:
				sawAllRetainedCoverage = true
			case testCase.WantRetained == 0:
				sawAllEmptyCoverage = true
			}
		}
		for _, outcomes := range perSession {
			if len(outcomes) > 1 {
				sawMultiRowSingleSession = true
			}
			// Classified through the SHARED production function in ingest,
			// not a list of my own. The classifier used to live here as a
			// switch and in the test as a second list, with nothing binding
			// the two, so the corpus agreed with production only by
			// coincidence: it used two of the five outcomes, and deleting
			// the other success arm was green.
			hasError, hasSuccess := false, false
			for _, outcome := range outcomes {
				if logOutcomes[outcome] == ingest.IndexOutcomeError {
					hasError = true
				}
				if ingest.IndexOutcomeCompleted(logOutcomes[outcome]) {
					hasSuccess = true
				}
			}
			if hasError && hasSuccess {
				sawRecovery = true
			}
		}
	}
	for name := range required {
		if !seen[name] {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("required case %q is missing", name),
				"loader=required-name manifest",
				"fix=restore the case; deleting a case the manifest names fails the corpus even when the counts are decremented to match")
		}
	}
	for name := range seen {
		if !required[name] {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("case %q is not in the required manifest", name),
				"loader=required-name manifest",
				"fix=name it in required_cases; a count-preserving swap that drops a required case for a filler must not stay green")
		}
	}
	// The two shapes the row-counting bug could not tell apart. Without the first
	// the count passes whether it counts rows or sessions; without the second it
	// passes whether or not a later success clears an earlier error.
	// Every outcome the pipeline can record, walked from the production
	// enumeration. The corpus once used two of the five, so the arm handling
	// reindexed was unreachable from it and deleting it was green; fallback is
	// recorded by --reindex too, but as a routing diagnostic before indexing,
	// never as a completion, so its two rows pin that it clears nothing.
	for _, outcome := range ingest.AllIndexOutcomes {
		if !coveredOutcomes[outcome] {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("no case records the %q outcome", outcome),
				"loader=closed-set outcome coverage",
				"fix=add a row using it; an outcome no row records is an arm of the counter nothing reaches, and the "+
					"counter decides a number printed directly beneath the session count a user reads")
		}
	}
	if !sawMultiRowSingleSession {
		return document, indexFailureCountRuleError(
			"no case gives ONE session more than one log row",
			"loader=multi-attempt coverage",
			"fix=add one; a session produces several rows in a single run - the drain loop and the stale sweep each "+
				"record an attempt - and without such a case a count over ROWS is indistinguishable from a count over "+
				"SESSIONS, which is exactly the defect that shipped")
	}
	if !sawRecovery {
		return document, indexFailureCountRuleError(
			"no case has a session that fails an attempt and then succeeds",
			"loader=recovery coverage",
			"fix=add one; a session the sweep recovers is not a failure, and without such a case the count passes "+
				"whether or not a later success clears an earlier error")
	}
	if !sawMoreThanOneFailingSession {
		return document, indexFailureCountRuleError(
			"no case expects more than one session to be reported",
			"loader=plural-count coverage",
			"fix=add one; every other requirement here is met by a counter that answers whether ANY session failed, and "+
				"the defect this corpus exists for was a wrong NUMBER printed beneath the session total, not a wrong "+
				"yes or no")
	}
	if !sawPrintableRowSessionGap {
		return document, indexFailureCountRuleError(
			"no case both warns and holds more log rows than the number it warns about",
			"loader=printable row/session gap coverage",
			"fix=add one; this corpus also drives the RENDERED warning, and where the row count equals the session count "+
				"the warning prints the same digit either way - so the number a user reads would be unpinned while the "+
				"counter behind it looked thoroughly covered, which is the state this corpus was in")
	}
	for shape, seen := range map[string]bool{"mixed (empty and retained)": sawMixedCoverage, "all-retained": sawAllRetainedCoverage, "all-empty": sawAllEmptyCoverage} {
		if !seen {
			return document, indexFailureCountRuleError(
				fmt.Sprintf("no case with entries presence covers the %s split", shape),
				"loader=coverage-split coverage",
				"fix=add one; each summary sentence answers to its own count, so a shape no case exercises is a sentence nothing checks")
		}
	}
	return document, nil
}

func indexFailureCountRuleError(what, where, fix string) error {
	return fmt.Errorf(
		"index failure count fixture rule failed: %s; a malformed corpus invalidates the only evidence that the warning "+
			"a user reads reports the right number; where=%s %s; when=test fixture loading; "+
			"impact=a summary could contradict the line printed directly above it; %s",
		what, indexFailureCountFixturePath, where, fix)
}

func TestLoadIndexFailureCountFixture_RejectsACorpusThatNeverExpectsMoreThanOne(t *testing.T) {
	t.Parallel()
	_, err := loadIndexFailureCountFixture(indexFailureCountRejectSingularData)
	if err == nil || !strings.Contains(err.Error(), "more than one session to be reported") {
		t.Fatalf("error = %v, want rejection of a corpus that cannot tell a COUNT from a yes/no; every other requirement "+
			"here is satisfied by a counter reporting whether any session failed", err)
	}
}

func TestLoadIndexFailureCountFixture_RejectsACorpusWithNoRowSessionGapItWarnsAbout(t *testing.T) {
	t.Parallel()
	_, err := loadIndexFailureCountFixture(indexFailureCountRejectNoGapData)
	if err == nil || !strings.Contains(err.Error(), "more log rows than the number it warns about") {
		t.Fatalf("error = %v, want rejection of a corpus whose warning cases all hold one row per session; the rendered "+
			"warning then shows the same digit whether the summary counts rows or sessions, which is how the printed "+
			"number stayed unpinned while the counter behind it was covered nine ways", err)
	}
}

// indexFailureRequiredCases pins the corpus membership in code. The fixture
// carries its own required_cases manifest, but a deletion that edits both the
// cases and that manifest together would still satisfy the loader - so the
// names live here too, and dropping a case means editing this list, where the
// weakening shows in the diff rather than hiding behind decremented counts.
var indexFailureRequiredCases = []string{
	"a-clean-run-reports-nothing",
	"one-session-failing-once-is-one-session",
	"one-session-failing-twice-is-still-ONE-session",
	"a-session-the-sweep-recovers-is-not-a-failure",
	"recovery-counts-whichever-order-the-rows-arrive-in",
	"distinct-failing-sessions-are-counted-separately",
	"a-reindexed-session-is-not-a-failure",
	"a-fallback-does-not-clear-an-error",
	"a-routing-fallback-then-error-is-still-a-failure",
	"a-skip-does-not-clear-an-error",
	"a-skipped-only-session-is-not-a-failure-at-all",
	"failed-sessions-split-empty-and-retained",
	"failed-sessions-all-kept-their-entries",
	"failed-sessions-all-empty",
}

func TestIndexFailureCountFixture_RequiresNamedCases(t *testing.T) {
	t.Parallel()
	document, err := loadIndexFailureCountFixture(indexFailureCountFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.RequiredCases) != len(indexFailureRequiredCases) {
		t.Fatalf("required_cases holds %d name(s), want %d; add the new case to the manifest in %s and to indexFailureRequiredCases together",
			len(document.RequiredCases), len(indexFailureRequiredCases), indexFailureCountFixturePath)
	}
	for _, name := range indexFailureRequiredCases {
		found := false
		for _, required := range document.RequiredCases {
			if required == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("required case %q is missing from required_cases in %s; restore it - a corpus that never names a success arm stays green when that arm is deleted",
				name, indexFailureCountFixturePath)
		}
	}
}

// TestCountIndexFailures_CountsSessionsNotLogRows drives the production counter
// and the shared classifier behind it: the count AND the classified membership.
func TestCountIndexFailures_CountsSessionsNotLogRows(t *testing.T) {
	t.Parallel()
	document, err := loadIndexFailureCountFixture(indexFailureCountFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Cases) < indexFailureCountFloor {
		t.Fatalf("the corpus holds %d cases, below the floor of %d", len(document.Cases), indexFailureCountFloor)
	}
	for _, testCase := range document.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			log := make([]ingest.IndexLogEntry, 0, len(testCase.Rows))
			for _, row := range testCase.Rows {
				log = append(log, ingest.IndexLogEntry{
					SessionID: ingest.SessionID(row.Session),
					Outcome:   logOutcomes[row.Outcome],
				})
			}
			if got := countIndexFailures(log); got != testCase.WantCount {
				t.Errorf("countIndexFailures reported %d, want %d. The summary prints this directly beneath its session "+
					"count, so a wrong number here contradicts the line above it and is the first thing a user notices.\n"+
					"log: %+v", got, testCase.WantCount, testCase.Rows)
			}
			// Membership, not just the number: which sessions the run reports,
			// in first-seen order, through the shared ingest classifier.
			failed := ingest.FailedIndexSessions(log)
			if len(failed) != len(testCase.WantFailed) {
				t.Fatalf("FailedIndexSessions reported %v, want %v.\nlog: %+v", failed, testCase.WantFailed, testCase.Rows)
			}
			for i, session := range testCase.WantFailed {
				if string(failed[i]) != session {
					t.Errorf("FailedIndexSessions[%d] = %q, want %q (full: %v, want %v).\nlog: %+v",
						i, failed[i], session, failed, testCase.WantFailed, testCase.Rows)
				}
			}
		})
	}
}
