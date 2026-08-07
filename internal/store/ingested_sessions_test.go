package store

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

const (
	ingestedSessionWithMetrics    = "50000000-0000-0000-0000-000000000001"
	ingestedSessionWithoutMetrics = "50000000-0000-0000-0000-000000000002"
)

// TestAllIngestedSessionsKeepsSessionsWithoutMetrics pins the join AllIngestedSessions
// depends on. Metrics are written beside a session and deleted BEFORE it (prune
// removes the metrics row, then the session row), so a session can outlive its
// metrics. Requiring a metrics row would drop such a session from the result,
// and a caller that reads this as "everything the store holds" would then treat
// an already-ingested session as new and pay to resolve it all over again.
func TestAllIngestedSessionsKeepsSessionsWithoutMetrics(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s, err := Open(filepath.Join(t.TempDir(), "ingested.db"), WithPoolSize(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for i, sessionID := range []string{ingestedSessionWithMetrics, ingestedSessionWithoutMetrics} {
		entry := makeStoreEntry(t, sessionID,
			// One project and host slug per session so neither row can mask the other.
			repeatHex(t, i+1), "github.com--acme--ingested", defaults.HarnessClaudeCode, int64(1000+i), 0, 0)
		if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("InsertSessions(%s): %v", sessionID, err)
		}
	}

	// The ingest write path always creates the metrics row beside the session,
	// so reaching the state under test means taking that row back out.
	conn, err := s.pool.Take(ctx)
	if err != nil {
		t.Fatalf("take connection: %v", err)
	}
	err = sqlitex.ExecuteTransient(conn, "DELETE FROM session_metrics WHERE session_id = ?", &sqlitex.ExecOptions{
		Args: []any{ingestedSessionWithoutMetrics},
	})
	changed := conn.Changes()
	s.pool.Put(conn)
	if err != nil {
		t.Fatalf("delete metrics row: %v", err)
	}
	if changed != 1 {
		t.Fatalf("deleting the metrics row removed %d rows, want 1; the ingest write path no longer creates one, so this test no longer builds the state it claims", changed)
	}

	rows, err := s.AllIngestedSessions(ctx)
	if err != nil {
		t.Fatalf("AllIngestedSessions: %v", err)
	}
	byID := make(map[string]IngestedSessionRow, len(rows))
	for _, row := range rows {
		byID[row.SessionID] = row
	}

	if _, ok := byID[ingestedSessionWithMetrics]; !ok {
		t.Errorf("session %s with metrics is missing from the result", ingestedSessionWithMetrics)
	}
	row, ok := byID[ingestedSessionWithoutMetrics]
	if !ok {
		t.Fatalf("session %s was dropped because it has no metrics row; the store still holds it, so the read must still return it", ingestedSessionWithoutMetrics)
	}
	if row.Title != "" {
		t.Errorf("title = %q, want empty for a session with no metrics row", row.Title)
	}
	if row.IngestedMs == 0 {
		t.Error("ingested timestamp is 0; the diff rule cannot classify a source against it")
	}
	if row.SchemaVersion != ingest.CurrentSchemaVersion {
		t.Errorf("schema version = %d, want %d", row.SchemaVersion, ingest.CurrentSchemaVersion)
	}
}

// repeatHex builds a distinct valid project hash for the nth seeded session.
func repeatHex(t *testing.T, n int) string {
	t.Helper()
	const width = 64
	digit := byte('0' + n)
	out := make([]byte, width)
	for i := range out {
		out[i] = digit
	}
	return string(out)
}
