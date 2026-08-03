package testutil

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// SelectionOutcome is the fixture spelling of ingest.BranchMatch. Selection
// matching is three-valued, so a fixture column for it must be a closed set of
// three tokens: a boolean column silently collapses "withheld" into whichever
// of the two it happens to sit next to, which is how an inverted comparison
// stays green.
type SelectionOutcome string

const (
	// SelectionSelected expects ingest.BranchMatchYes.
	SelectionSelected SelectionOutcome = "selected"
	// SelectionRejected expects ingest.BranchMatchNo.
	SelectionRejected SelectionOutcome = "rejected"
	// SelectionWithheld expects ingest.BranchMatchWithheldConflict.
	SelectionWithheld SelectionOutcome = "withheld"
)

// AllSelectionOutcomes enumerates the three outcomes. It does not itself make
// any corpus cover them — a corpus that wants that passes this to
// RequireClosedSetCoverage.
var AllSelectionOutcomes = []SelectionOutcome{SelectionSelected, SelectionRejected, SelectionWithheld}

// BranchMatch decodes the fixture token, failing the corpus on anything outside
// the closed set rather than defaulting to a value — a default would turn a
// typo into a weaker expectation that still passes.
func (o SelectionOutcome) BranchMatch(t *testing.T, corpus, caseName string) ingest.BranchMatch {
	t.Helper()
	switch o {
	case SelectionSelected:
		return ingest.BranchMatchYes
	case SelectionRejected:
		return ingest.BranchMatchNo
	case SelectionWithheld:
		return ingest.BranchMatchWithheldConflict
	default:
		t.Fatalf("%s fixture case %q declares unknown selection outcome %q; use one of %v", corpus, caseName, string(o), AllSelectionOutcomes)
		return ingest.BranchMatchNo
	}
}

// FixtureField pairs a fixture key with the value it loaded, so a blank value is
// reported by the exact key a maintainer has to edit instead of one opaque
// "fixture is incomplete".
type FixtureField struct {
	Key   string
	Value string
}

// RequireFixtureFields fails when any required fixture value is blank. A blank
// value is the most expensive failure a fixture corpus has: it makes the
// assertion it feeds either trivially true (strings.Contains against an empty
// needle) or a comparison of two zero values, so the case keeps passing while
// proving nothing.
func RequireFixtureFields(t *testing.T, corpus, caseName string, fields []FixtureField) {
	t.Helper()
	for _, field := range fields {
		if strings.TrimSpace(field.Value) == "" {
			t.Fatalf("%s fixture case %q leaves %s blank; a blank value turns the assertion it feeds into a guaranteed pass — set %s to the exact value the run must observe",
				corpus, caseName, field.Key, field.Key)
		}
	}
}

// RequireClosedSetCoverage fails when the corpus does not exercise every member
// of a closed set of outcomes. It is the SECOND layer beside a row-count floor,
// not a replacement for one: a floor catches the count dropping, while this
// catches a behaviour disappearing, including when a row is swapped for another
// at the same count.
//
// CALLER'S OBLIGATION, which this cannot check: observed must be what each row
// actually ASSERTS — a value cross-checked against real behaviour, or computed
// from the row — rather than a label the row declares about itself. Pass a bare
// declared label and this reports coverage of claims nobody verified.
func RequireClosedSetCoverage[T comparable](t *testing.T, corpus, dimension string, closedSet []T, observed []T) {
	t.Helper()
	seen := make(map[T]struct{}, len(observed))
	for _, value := range observed {
		seen[value] = struct{}{}
	}
	for _, want := range closedSet {
		if _, ok := seen[want]; !ok {
			t.Fatalf("%s fixture covers no case with %s = %v; the corpus must exercise every value of that closed set, so add a case for it rather than lowering the expectation (observed: %v)",
				corpus, dimension, want, observed)
		}
	}
}
