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
const indexFailureCountFloor = 10

type indexFailureCountDocument struct {
	ExpectedCaseCount int                     `yaml:"expectedCaseCount"`
	Cases             []indexFailureCountCase `yaml:"cases"`
}

type indexFailureCountCase struct {
	Name      string               `yaml:"name"`
	Rows      []indexFailureLogRow `yaml:"rows"`
	WantCount int                  `yaml:"wantCount"`
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
		for _, row := range testCase.Rows {
			if _, known := logOutcomes[row.Outcome]; !known {
				return document, indexFailureCountRuleError(
					fmt.Sprintf("case %q names the outcome %q, which the pipeline does not record", testCase.Name, row.Outcome),
					where, "fix=use one of indexed, reindexed, fallback, skipped, error")
			}
			coveredOutcomes[logOutcomes[row.Outcome]] = true
			perSession[row.Session] = append(perSession[row.Session], row.Outcome)
		}
		for _, outcomes := range perSession {
			if len(outcomes) > 1 {
				sawMultiRowSingleSession = true
			}
			// Classified through the PRODUCTION function, not a list of my own.
			// These were written twice with nothing binding them, and the corpus
			// agreed with production only by coincidence - it used two of the five
			// outcomes, so deleting the other two success arms was green.
			hasError, hasSuccess := false, false
			for _, outcome := range outcomes {
				if logOutcomes[outcome] == ingest.IndexOutcomeError {
					hasError = true
				}
				if IndexOutcomeEndedIndexed(logOutcomes[outcome]) {
					hasSuccess = true
				}
			}
			if hasError && hasSuccess {
				sawRecovery = true
			}
		}
	}
	// The two shapes the row-counting bug could not tell apart. Without the first
	// the count passes whether it counts rows or sessions; without the second it
	// passes whether or not a later success clears an earlier error.
	// Every outcome the pipeline can record, walked from the production
	// enumeration. The corpus used two of the five, so the arms handling the other
	// three were unreachable from it and deleting two of them was green -
	// reindexed and fallback are both recorded by --reindex, so the gap was
	// reachable rather than theoretical.
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

// TestCountIndexFailures_CountsSessionsNotLogRows drives the production counter.
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
		})
	}
}
