package ingest_test

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestRefreshReasonsUseTheOutcomeVocabulary pins the one-spelling rule: every
// refreshing decision reason is the code of an OutcomeReason refresh constant,
// and the closed decision set is those eleven plus source_compare and none.
func TestRefreshReasonsUseTheOutcomeVocabulary(t *testing.T) {
	want := make(map[ingest.RefreshReason]ingest.OutcomeReason)
	for _, outcome := range ingest.AllOutcomeReasons {
		if outcome.Kind() != ingest.OutcomeKindRefresh {
			continue
		}
		want[ingest.RefreshReason(outcome.Code())] = outcome
	}
	if len(want) != 11 {
		t.Fatalf("outcome vocabulary defines %d refresh reasons, want 11", len(want))
	}
	for raw, outcome := range want {
		reason, err := ingest.NewRefreshReason(string(raw))
		if err != nil {
			t.Fatalf("NewRefreshReason(%q): %v", raw, err)
		}
		if reason.OutcomeReason() != outcome {
			t.Fatalf("decision reason %q maps to outcome %s, want %s", raw, reason.OutcomeReason().Code(), outcome.Code())
		}
	}
	if got, wantLen := len(ingest.AllRefreshReasons), len(want)+2; got != wantLen {
		t.Fatalf("AllRefreshReasons has %d entries, want %d (eleven refreshes plus source_compare and none)", got, wantLen)
	}
	seen := make(map[ingest.RefreshReason]bool, len(ingest.AllRefreshReasons))
	for _, reason := range ingest.AllRefreshReasons {
		if seen[reason] {
			t.Fatalf("AllRefreshReasons repeats %q", reason)
		}
		seen[reason] = true
		if reason == ingest.RefreshReasonSourceCompare || reason == ingest.RefreshReasonNone {
			if reason.OutcomeReason() != 0 {
				t.Fatalf("%q writes outcome row %s; it must write none", reason, reason.OutcomeReason().Code())
			}
			continue
		}
		if want[reason] == 0 {
			t.Fatalf("decision reason %q is not an OutcomeReason refresh constant", reason)
		}
	}
	if !seen[ingest.RefreshReasonSourceCompare] || !seen[ingest.RefreshReasonNone] {
		t.Fatal("AllRefreshReasons omits source_compare or none")
	}
}
