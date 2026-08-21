package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestOpenCodeOrphanPartDecodeTolerance proves that an orphan part row with BLOB
// data and an orphan part row with an invalid identifier are each dropped with
// the orphan-part-dropped warning while the session still materializes.
func TestOpenCodeOrphanPartDecodeTolerance(t *testing.T) {
	materialized := testfixture.MaterializeByName(t, "legacy-orphan-tolerance")
	root, err := ingest.NewResolvedPath(filepath.Dir(materialized.Path))
	if err != nil {
		t.Fatalf("resolve synthetic OpenCode root: %v", err)
	}
	const session = "ses_3cd91f52effeXd3QAJ54jOyzO1"
	// Orphan parts point at message ids with no message row in the session.
	withCanonicalConnection(t, materialized.Path, func(connection *sqlite.Conn) error {
		if err := sqlitex.Execute(connection, `INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES(?1, ?2, ?3, ?4, ?4, ?5)`, &sqlitex.ExecOptions{Args: []any{"part_blob_orphan", "msg_absent_orphan", session, 1100, []byte{0, 1, 2, 3}}}); err != nil {
			return err
		}
		return sqlitex.Execute(connection, `INSERT INTO part(id, message_id, session_id, time_created, time_updated, data) VALUES(?1, ?2, ?3, ?4, ?4, ?5)`, &sqlitex.ExecOptions{Args: []any{" bad orphan id ", "msg_absent_orphan2", session, 1200, `{"type":"text","text":"invalid id orphan"}`}})
	})

	adapter := parentClockAdapter(t)
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatalf("discover the orphan-bearing session: %v", err)
	}
	var host *ingest.DiscoveredSession
	for index := range discovered {
		if string(discovered[index].SessionID) == session {
			host = &discovered[index]
		}
	}
	if host == nil {
		t.Fatalf("discovery = %+v, want the orphan host session", discovered)
	}
	metadata, data, err := adapter.MaterializeTranscript(t.Context(), *host)
	if err != nil {
		t.Fatalf("orphan-bearing session failed to materialize: %v", err)
	}
	if len(data) == 0 || metadata == nil {
		t.Fatalf("orphan-bearing session materialized empty: data=%d metadata=%v", len(data), metadata)
	}
	dropped := 0
	for _, warning := range metadata.Diagnostics.Warnings {
		if warning.ErrorType == string(ingest.OpenCodeGraphOrphanPartDropped) {
			dropped++
		}
	}
	if dropped < 2 {
		t.Fatalf("orphan drops = %d, want at least the BLOB-data and invalid-id orphans dropped with a warning: %+v", dropped, metadata.Diagnostics.Warnings)
	}
}
