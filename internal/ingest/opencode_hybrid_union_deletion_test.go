package ingest_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
)

// TestOpenCodeHybridUnionSkipsDeletedSessions proves the two joined rules of a
// hybrid OpenCode database. Discovery unions both projections, so a session that
// lives only in the legacy tables is discovered as a legacy winner while a
// session in both is a current winner. The session table is authoritative for
// existence, so a discovered session with no row there was deleted from OpenCode
// and is skipped with a diagnostic rather than resurrected from its historical
// message or session_message rows.
func TestOpenCodeHybridUnionSkipsDeletedSessions(t *testing.T) {
	const (
		legacyOnlyKept = "ses_3cd91f52effeXd3QAJ54jOyzG1"
		inBothKept     = "ses_3cd91f52effeXd3QAJ54jOyzG2"
		legacyDeleted  = "ses_3cd91f52effeXd3QAJ54jOyzG3"
		currentDeleted = "ses_3cd91f52effeXd3QAJ54jOyzG4"
	)
	materialized := testfixture.MaterializeByName(t, "hybrid-deleted-sessions-skipped")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	adapter := parentClockAdapter(t)
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover hybrid database with deleted sessions: %v", err)
	}
	origins := make(map[string]ingest.TranscriptOrigin, len(discovered))
	for _, session := range discovered {
		origins[string(session.SessionID)] = session.TranscriptOrigin
	}

	// The union keeps the legacy-only session as a legacy winner.
	if origin, ok := origins[legacyOnlyKept]; !ok || origin != ingest.TranscriptOriginOpenCodeLegacySQLite {
		t.Fatalf("legacy-only session %q origin = %v present = %t, want a legacy SQLite winner", legacyOnlyKept, origins[legacyOnlyKept], ok)
	}
	// A session present in both projections is a current winner.
	if origin, ok := origins[inBothKept]; !ok || origin != ingest.TranscriptOriginOpenCodeCurrentSQLite {
		t.Fatalf("session %q in both projections origin = %v present = %t, want a current SQLite winner", inBothKept, origins[inBothKept], ok)
	}
	// A deleted session with only legacy rows is skipped, not resurrected.
	if origin, resurrected := origins[legacyDeleted]; resurrected {
		t.Fatalf("deleted legacy-only session %q was resurrected as %v; it must be skipped", legacyDeleted, origin)
	}
	// A deleted session with only a current row is skipped, not resurrected.
	if origin, resurrected := origins[currentDeleted]; resurrected {
		t.Fatalf("deleted current session %q was resurrected as %v; it must be skipped", currentDeleted, origin)
	}
	if len(discovered) != 2 {
		t.Fatalf("hybrid discovery returned %d sessions, want only the two undeleted ones: %+v", len(discovered), discovered)
	}

	diagnostic := discoveryFailedDiagnostic(adapter.CandidateEvidence(), materialized.Path)
	if diagnostic == nil {
		t.Fatalf("deleted sessions recorded no diagnostic: evidence = %+v", adapter.CandidateEvidence())
	}
	if !strings.Contains(diagnostic.What, legacyDeleted) || !strings.Contains(diagnostic.What, currentDeleted) {
		t.Fatalf("deletion diagnostic = %q, want it to name the deleted sessions %q and %q", diagnostic.What, legacyDeleted, currentDeleted)
	}
	if !strings.Contains(diagnostic.What, "deleted from OpenCode") {
		t.Fatalf("deletion diagnostic = %q, want it to explain the sessions were deleted from OpenCode", diagnostic.What)
	}
}
