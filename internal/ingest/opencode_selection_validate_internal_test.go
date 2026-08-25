package ingest

import (
	"testing"
)

// TestSelectCanonicalOpenCodeCandidatesRejectsUnknownProvenance proves the
// selection boundary fails loudly on an unknown provenance instead of letting
// precedence() rank it as zero and silently misorder a winner.
func TestSelectCanonicalOpenCodeCandidatesRejectsUnknownProvenance(t *testing.T) {
	sessionID := SessionID("ses_3cd91f52effeXd3QAJ54jOyzv5")
	candidate := openCodeSessionCandidate{
		session:    DiscoveredSession{SessionID: sessionID, Harness: HarnessOpenCode},
		identity:   OpenCodeSelectedSourceIdentity{SessionID: sessionID, Representation: OpenCodeRepresentationCurrentSQLite, Path: ResolvedPath("/synthetic/opencode.db")},
		provenance: OpenCodeCandidateProvenance("event_history"),
	}
	if _, err := selectCanonicalOpenCodeCandidates([]openCodeSessionCandidate{candidate}); err == nil {
		t.Fatal("selection accepted a candidate with an unknown provenance; it must reject it at the boundary")
	}
}
