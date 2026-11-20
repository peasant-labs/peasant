package ingest

import "testing"

func TestStoredRowWithMissingPairNeedsNativeRefresh(t *testing.T) {
	if got, want := decideRefresh(refreshEvidence{StoredRow: true, PairMissing: true}), (RefreshDecision{Reason: RefreshReasonPairMissing, Lane: RefreshLaneNative}); got != want {
		t.Fatalf("decision = %+v, want %+v", got, want)
	}
}
