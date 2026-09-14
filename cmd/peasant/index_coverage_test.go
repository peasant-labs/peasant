package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// coverageStubReader answers entries membership from the corpus's per-session
// presence: a session without entries is reported without.
type coverageStubReader struct {
	without map[ingest.SessionID]bool
}

func (s *coverageStubReader) SessionsWithoutEntries(_ context.Context, ids []ingest.SessionID) (map[ingest.SessionID]bool, error) {
	out := make(map[ingest.SessionID]bool, len(ids))
	for _, id := range ids {
		out[id] = s.without[id]
	}
	return out, nil
}

// coverageCaseLog builds the production index log for a corpus case.
func coverageCaseLog(testCase indexFailureCountCase) []ingest.IndexLogEntry {
	log := make([]ingest.IndexLogEntry, 0, len(testCase.Rows))
	for _, row := range testCase.Rows {
		log = append(log, ingest.IndexLogEntry{
			SessionID: ingest.SessionID(row.Session),
			Outcome:   logOutcomes[row.Outcome],
		})
	}
	return log
}

// coverageCases returns the corpus cases that pin the coverage split.
func coverageCases(t *testing.T) []indexFailureCountCase {
	t.Helper()
	document, err := loadIndexFailureCountFixture(indexFailureCountFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	var cases []indexFailureCountCase
	for _, testCase := range document.Cases {
		if testCase.Entries != nil {
			cases = append(cases, testCase)
		}
	}
	if len(cases) == 0 {
		t.Fatal("no corpus case pins the coverage split; the summary sentences and the JSON field would be untested")
	}
	return cases
}

// TestIndexCoverage_MatchesCorpusSplit drives the production computation from
// the corpus log plus the corpus presence map, and pins the independent
// expected counts and the classified membership behind them.
func TestIndexCoverage_MatchesCorpusSplit(t *testing.T) {
	t.Parallel()
	for _, testCase := range coverageCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			log := coverageCaseLog(testCase)
			reader := &coverageStubReader{without: map[ingest.SessionID]bool{}}
			for session, hasEntries := range testCase.Entries {
				reader.without[ingest.SessionID(session)] = !hasEntries
			}
			coverage, err := ingest.ComputeIndexCoverage(context.Background(), reader, ingest.FailedIndexSessions(log))
			if err != nil {
				t.Fatalf("ComputeIndexCoverage: %v", err)
			}
			if coverage == nil {
				t.Fatal("ComputeIndexCoverage answered nil for a readable store; nil means unavailable, never zero")
			}
			if coverage.FailedAttempts != testCase.WantCount {
				t.Errorf("FailedAttempts = %d, want %d", coverage.FailedAttempts, testCase.WantCount)
			}
			if coverage.Empty != testCase.WantEmpty {
				t.Errorf("Empty = %d, want %d", coverage.Empty, testCase.WantEmpty)
			}
			if coverage.FailedRetained != testCase.WantRetained {
				t.Errorf("FailedRetained = %d, want %d", coverage.FailedRetained, testCase.WantRetained)
			}
			if coverage.FailedAttempts != coverage.Empty+coverage.FailedRetained {
				t.Errorf("FailedAttempts = %d, want Empty + FailedRetained = %d + %d",
					coverage.FailedAttempts, coverage.Empty, coverage.FailedRetained)
			}
			// Membership, not just the numbers: which failed sessions the
			// run reports empty, through the same production computation.
			failed := ingest.FailedIndexSessions(log)
			var gotEmpty, gotRetained []string
			for _, id := range failed {
				if reader.without[id] {
					gotEmpty = append(gotEmpty, string(id))
				} else {
					gotRetained = append(gotRetained, string(id))
				}
			}
			if fmt.Sprintf("%v", gotEmpty) != fmt.Sprintf("%v", testCase.WantEmptySessions) {
				t.Errorf("empty membership = %v, want %v", gotEmpty, testCase.WantEmptySessions)
			}
			if fmt.Sprintf("%v", gotRetained) != fmt.Sprintf("%v", testCase.WantRetainedSessions) {
				t.Errorf("retained membership = %v, want %v", gotRetained, testCase.WantRetainedSessions)
			}
		})
	}
}

// TestHarvestSummary_PrintsCoverageSplit drives the real summary writer with
// the corpus coverage and pins that each sentence answers to its own count:
// omitted at its own zero, present with its own number otherwise.
func TestHarvestSummary_PrintsCoverageSplit(t *testing.T) {
	t.Parallel()
	for _, testCase := range coverageCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			log := coverageCaseLog(testCase)
			reader := &coverageStubReader{without: map[ingest.SessionID]bool{}}
			for session, hasEntries := range testCase.Entries {
				reader.without[ingest.SessionID(session)] = !hasEntries
			}
			coverage, err := ingest.ComputeIndexCoverage(context.Background(), reader, ingest.FailedIndexSessions(log))
			if err != nil {
				t.Fatal(err)
			}
			result := &ingest.PipelineResult{IndexLog: log, IndexCoverage: coverage}
			var out strings.Builder
			printSummary(&out, result, false, false, t.TempDir(), "", nil, 0)
			printed := out.String()

			if testCase.WantEmpty > 0 {
				if want := fmt.Sprintf("%d session(s) have no stored entries", testCase.WantEmpty); !strings.Contains(printed, want) {
					t.Errorf("the summary must warn %q.\ngot:\n%s", want, printed)
				}
			} else if strings.Contains(printed, "have no stored entries") {
				t.Errorf("nothing is empty, yet the summary warns about empty sessions:\n%s", printed)
			}
			if testCase.WantRetained > 0 {
				if want := fmt.Sprintf("%d session(s) failed to re-index but kept their previous entries", testCase.WantRetained); !strings.Contains(printed, want) {
					t.Errorf("the summary must report %q.\ngot:\n%s", want, printed)
				}
			} else if strings.Contains(printed, "kept their previous entries") {
				t.Errorf("nothing was retained, yet the summary reports retained sessions:\n%s", printed)
			}
			if testCase.WantCount > 0 {
				if !strings.Contains(printed, indexFailureRemedy) {
					t.Errorf("the warning names failures the run is otherwise silent about; it must carry %q.\ngot:\n%s", indexFailureRemedy, printed)
				}
			} else if strings.Contains(printed, indexFailureRemedy) {
				t.Errorf("a run with no failures must not carry the failure remedy:\n%s", printed)
			}
			// The measured split replaces the bare attempt count: with
			// coverage the summary must never claim the bare number, since
			// that sentence cannot tell empty from retained.
			if strings.Contains(printed, "were imported but NOT indexed") {
				t.Errorf("measured coverage must replace the bare attempt count, not repeat it:\n%s", printed)
			}
		})
	}
}

// TestPrintJSON_CarriesCoverage drives the real JSON serialization with the
// corpus coverage and pins that a measured zero serializes as 0 while an
// unavailable run omits the field entirely.
func TestPrintJSON_CarriesCoverage(t *testing.T) {
	t.Parallel()
	for _, testCase := range coverageCases(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			log := coverageCaseLog(testCase)
			reader := &coverageStubReader{without: map[ingest.SessionID]bool{}}
			for session, hasEntries := range testCase.Entries {
				reader.without[ingest.SessionID(session)] = !hasEntries
			}
			coverage, err := ingest.ComputeIndexCoverage(context.Background(), reader, ingest.FailedIndexSessions(log))
			if err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			if err := printJSON(&out, &ingest.PipelineResult{IndexLog: log, IndexCoverage: coverage}); err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
				t.Fatalf("printJSON output is not JSON: %v\n%s", err, out.String())
			}
			raw, ok := decoded["indexCoverage"]
			if !ok {
				t.Fatalf("measured coverage is missing from the JSON output:\n%s", out.String())
			}
			fields, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("indexCoverage is not an object:\n%s", out.String())
			}
			for field, want := range map[string]int{
				"failedAttempts": testCase.WantCount,
				"empty":          testCase.WantEmpty,
				"failedRetained": testCase.WantRetained,
			} {
				got, ok := fields[field].(float64)
				if !ok || int(got) != want {
					t.Errorf("indexCoverage.%s = %v, want %d:\n%s", field, fields[field], want, out.String())
				}
			}
		})
	}
}

// TestPrintJSON_OmitsUnavailableCoverage pins the other half of the
// contract: an unavailable run omits the field rather than serializing a
// false zero, so a measured zero and an absent answer stay distinguishable.
func TestPrintJSON_OmitsUnavailableCoverage(t *testing.T) {
	t.Parallel()
	log := []ingest.IndexLogEntry{{SessionID: "s1", Outcome: ingest.IndexOutcomeError}}
	var out strings.Builder
	if err := printJSON(&out, &ingest.PipelineResult{IndexLog: log}); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("printJSON output is not JSON: %v\n%s", err, out.String())
	}
	if raw, ok := decoded["indexCoverage"]; ok {
		t.Errorf("unavailable coverage serialized as %v, want the field omitted:\n%s", raw, out.String())
	}
}
