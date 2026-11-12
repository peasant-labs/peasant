package store

// V35 migration tests (package `store`, internal): the structural assertions —
// the external-content FTS5 virtual table + its three sync triggers exist on a
// freshly opened DB. V76 re-pointed the same three triggers at the union
// session_search_fts index and dropped the retired session_entries_fts, so the
// assertions below name the union objects. End-to-end sync behavior
// (insert/reindex/prune through the public API) is covered by fts_search_test.go in package store_test, which has
// the seedSession + IndexSessionEntries helpers. Reuses execT/scalarInt from
// migration_v33_test.go.

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrationV35_FTSObjectsExist(t *testing.T) {
	t.Parallel()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer s.Pool().Put(conn)

	if uv := scalarInt(t, conn, "PRAGMA user_version"); uv < 35 {
		t.Errorf("user_version: expected >= 35, got %d", uv)
	}

	// The external-content FTS5 virtual table carrying the entries half.
	if got := scalarInt(t, conn,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_search_fts'"); got != 1 {
		t.Errorf("session_search_fts virtual table: expected 1, got %d", got)
	}

	// The three sync triggers (insert/delete/update).
	for _, trig := range []string{"session_entries_ai", "session_entries_ad", "session_entries_au"} {
		if got := scalarInt(t, conn,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name='"+trig+"'"); got != 1 {
			t.Errorf("trigger %s: expected 1, got %d", trig, got)
		}
	}

	// The FTS table is queryable (MATCH on an empty index returns nothing, not an error).
	if got := scalarInt(t, conn,
		"SELECT COUNT(*) FROM session_search_fts WHERE session_search_fts MATCH 'anything'"); got != 0 {
		t.Errorf("empty FTS MATCH: expected 0, got %d", got)
	}
}
