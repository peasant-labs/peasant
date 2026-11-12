package store_test

// V76 migration tests (package `store_test`, external): the end-to-end FTS5
// behavior through the public store API: the union sync triggers must keep
// session_search_fts in step with the DELETE+INSERT-per-session
// IndexSessionEntries write path and with PruneSessions. V76 re-pointed the
// session_entries_ai/ad/au triggers at the union index and dropped the
// retired session_entries_fts. Lives in package store_test to reuse
// seedSession + openTestStore.

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// ftsMatchCount returns how many FTS rows for sessionID match the query.
func ftsMatchCount(t *testing.T, s *store.Store, sessionID, query string) int {
	t.Helper()
	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer s.Pool().Put(conn)
	var n int
	// The union FTS index carries the mirror postings the production read
	// serves; the per-part exclusion keeps carrier rows out of results.
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT COUNT(*) FROM session_search_fts WHERE session_id = ? AND session_search_fts MATCH ?`,
		&sqlitex.ExecOptions{
			Args:       []any{sessionID, query},
			ResultFunc: func(stmt *sqlite.Stmt) error { n = int(stmt.ColumnInt64(0)); return nil },
		}); err != nil {
		t.Fatalf("fts match %q: %v", query, err)
	}
	return n
}

func entry(sid ingest.SessionID, idx int, role ingest.Role, etype ingest.EntryType, preview string) schema.SessionEntry {
	p := preview
	return schema.SessionEntry{
		SessionID: sid, EntryIndex: idx, Harness: defaults.HarnessClaudeCode,
		EntryType: etype, Role: role, ContentPreview: &p,
	}
}

func TestFTS_IndexSyncsAndReindexDoesNotDuplicate(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "11111111-1111-1111-1111-111111111111"
	seedSession(t, s, sid)
	sessionID := ingest.SessionID(sid)

	if err := s.IndexSessionEntries(ctx, sessionID, []schema.SessionEntry{
		entry(sessionID, 0, ingest.RoleUser, ingest.EntryTypeText, "explain photosynthesis in plants"),
		entry(sessionID, 1, ingest.RoleAssistant, ingest.EntryTypeText, "chlorophyll absorbs light"),
	}); err != nil {
		t.Fatalf("index: %v", err)
	}

	// INSERT trigger synced the FTS index.
	if got := ftsMatchCount(t, s, sid, "photosynthesis"); got != 1 {
		t.Errorf("after index: match 'photosynthesis' = %d, want 1", got)
	}

	// Re-index with different content: the DELETE (ad trigger) must drop stale
	// terms and the INSERT (ai trigger) add the new — no duplicates, no leftovers.
	if err := s.IndexSessionEntries(ctx, sessionID, []schema.SessionEntry{
		entry(sessionID, 0, ingest.RoleUser, ingest.EntryTypeText, "describe mitochondria function"),
	}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if got := ftsMatchCount(t, s, sid, "photosynthesis"); got != 0 {
		t.Errorf("after reindex: stale 'photosynthesis' = %d, want 0 (delete trigger)", got)
	}
	if got := ftsMatchCount(t, s, sid, "mitochondria"); got != 1 {
		t.Errorf("after reindex: 'mitochondria' = %d, want 1", got)
	}
}

func TestFTS_PruneClears(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "22222222-2222-2222-2222-222222222222"
	seedSession(t, s, sid)
	sessionID := ingest.SessionID(sid)

	if err := s.IndexSessionEntries(ctx, sessionID, []schema.SessionEntry{
		entry(sessionID, 0, ingest.RoleUser, ingest.EntryTypeText, "uniquetoken kangaroo"),
	}); err != nil {
		t.Fatalf("index: %v", err)
	}
	if got := ftsMatchCount(t, s, sid, "kangaroo"); got != 1 {
		t.Fatalf("pre-prune: 'kangaroo' = %d, want 1", got)
	}

	if _, err := s.PruneSessions(ctx, []ingest.SessionID{sessionID}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := ftsMatchCount(t, s, sid, "kangaroo"); got != 0 {
		t.Errorf("post-prune: 'kangaroo' = %d, want 0 (prune fires delete trigger)", got)
	}
}
