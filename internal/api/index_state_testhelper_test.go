package api

import (
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

// MarkStoredSessionsIndexed records a completed index pass for every session
// currently in db, so discovery lists them.
//
// InsertSessions writes a session row and its metrics but no entries, leaving
// indexed_at NULL, exactly as the ingest normalize stage does before the index
// stage runs. Discovery withholds a session with a NULL indexed_at because it
// is not yet viewable (opening it would show no turns). A test that seeds a
// session it expects to be browsable must therefore also mark it indexed, which
// is what the index stage does in production via UpdateIndexState. Tests that
// deliberately exercise the not-yet-indexed case must NOT call this.
//
// It is exported so both the internal (package api) and external (package
// api_test) test files can share the one definition.
func MarkStoredSessionsIndexed(t *testing.T, db *store.Store) {
	t.Helper()
	rows, err := db.AllSessions(t.Context())
	if err != nil {
		t.Fatalf("mark stored sessions indexed: list sessions: %v", err)
	}
	// A fixed, deterministic stamp: the value does not matter to discovery, only
	// that indexed_at is non-NULL.
	const indexedAtMs = int64(1705276800000)
	for i := range rows {
		if err := db.UpdateIndexState(
			t.Context(),
			ingest.SessionID(rows[i].SessionID),
			ingest.CurrentIndexVersion,
			indexedAtMs,
		); err != nil {
			t.Fatalf("mark stored sessions indexed: %s: %v", rows[i].SessionID, err)
		}
	}
}
