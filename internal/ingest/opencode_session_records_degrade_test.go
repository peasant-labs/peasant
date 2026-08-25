package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestOpenCodeSessionRecordsDegradePerRow proves that one undecodable session
// row is dropped with a diagnostic while every other session keeps its parent
// link and clock. Before the per-row degrade the whole session-record read
// aborted on the first bad row and every session lost its parent link.
func TestOpenCodeSessionRecordsDegradePerRow(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "session-records-degrade")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	const child = "ses_3cd91f52effeXd3QAJ54jOyzB1"
	const parent = "ses_3cd91f52effeXd3QAJ54jOyzB2"
	// Link the child to the parent with a good row, then corrupt the parent's
	// own session row by storing a non-integer time_updated. The parent row must
	// be dropped without losing the child's parent link.
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		if err := sqlitex.Execute(connection, `UPDATE session SET parent_id = ?2 WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{child, parent}}); err != nil {
			return err
		}
		return sqlitex.Execute(connection, `UPDATE session SET time_updated = 'not-an-integer' WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{parent}})
	})

	adapter := parentClockAdapter(t)
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover with a corrupt session row: %v", err)
	}
	var childSession *ingest.DiscoveredSession
	for index := range discovered {
		if string(discovered[index].SessionID) == child {
			childSession = &discovered[index]
		}
	}
	if childSession == nil {
		t.Fatalf("degrade discovery = %+v, want the child session discovered", discovered)
	}
	if childSession.ParentUUID == nil || string(*childSession.ParentUUID) != parent {
		t.Fatalf("child parent link = %v, want %q kept while the corrupt parent row is dropped", childSession.ParentUUID, parent)
	}
	if diagnostic := discoveryFailedDiagnostic(adapter.CandidateEvidence(), materialized.Path); diagnostic == nil {
		t.Fatalf("the dropped corrupt session row recorded no diagnostic: evidence=%+v", adapter.CandidateEvidence())
	}
}
