package ingest_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func discoverDanglingChild(t *testing.T, parentID string) (*ingest.DiscoveredSession, []ingest.OpenCodeProbeResult, string) {
	t.Helper()
	const child = "ses_3cd91f52effeXd3QAJ54jOyzP1"
	materialized := testfixture.MaterializeByName(t, "session-parent-no-clock")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, `INSERT INTO session(id, parent_id, time_created) VALUES(?1, ?2, ?3)`, &sqlitex.ExecOptions{Args: []any{child, parentID, 1000}})
	})
	adapter := parentClockAdapter(t)
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover child with parent %q: %v", parentID, err)
	}
	var childSession *ingest.DiscoveredSession
	for index := range discovered {
		if string(discovered[index].SessionID) == child {
			childSession = &discovered[index]
		}
	}
	if childSession == nil {
		t.Fatalf("discovery = %+v, want the child session discovered", discovered)
	}
	return childSession, adapter.CandidateEvidence(), materialized.Path
}

// TestOpenCodeDanglingParentIsIngestedAsRoot proves that a discovered session
// whose parent this run did not discover is ingested as a root with a named
// diagnostic, while a session whose parent is present keeps the link.
func TestOpenCodeDanglingParentIsIngestedAsRoot(t *testing.T) {
	const child = "ses_3cd91f52effeXd3QAJ54jOyzP1"
	const presentParent = "ses_3cd91f52effeXd3QAJ54jOyzP2"
	const absentParent = "ses_3cd91f52effeXd3QAJ54jOyzP9"

	t.Run("present parent keeps the link", func(t *testing.T) {
		childSession, evidence, path := discoverDanglingChild(t, presentParent)
		if childSession.ParentUUID == nil || string(*childSession.ParentUUID) != presentParent {
			t.Fatalf("child parent = %v, want the discovered parent %q", childSession.ParentUUID, presentParent)
		}
		if diagnostic := discoveryFailedDiagnostic(evidence, path); diagnostic != nil {
			t.Fatalf("a present-parent child recorded a discovery diagnostic %+v; it must not", diagnostic)
		}
	})

	t.Run("absent parent becomes a root with a diagnostic", func(t *testing.T) {
		childSession, evidence, path := discoverDanglingChild(t, absentParent)
		if childSession.ParentUUID != nil {
			t.Fatalf("child parent = %q, want a root because the parent was not discovered", *childSession.ParentUUID)
		}
		diagnostic := discoveryFailedDiagnostic(evidence, path)
		if diagnostic == nil {
			t.Fatalf("an undiscovered parent recorded no diagnostic: evidence=%+v", evidence)
		}
		if !strings.Contains(diagnostic.What, child) || !strings.Contains(diagnostic.What, absentParent) {
			t.Fatalf("diagnostic = %q, want it to name the child %q and the missing parent %q", diagnostic.What, child, absentParent)
		}
	})
}
