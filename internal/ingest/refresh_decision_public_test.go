package ingest

import "testing"

func TestPublicRefreshDecisionDelegates(t *testing.T) {
	evidence := RefreshEvidence{
		StoredRow:           true,
		AdapterVersion:      1,
		AdapterTarget:       2,
		AdapterStampPresent: true,
		AdapterCanExtract:   true,
		IndexProofPresent:   true,
	}
	if got, want := DecideRefresh(evidence), decideRefresh(evidence); got != want {
		t.Fatalf("public decision = %+v; canonical decision = %+v", got, want)
	}
}
