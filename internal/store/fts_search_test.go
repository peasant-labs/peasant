package store_test

// V34 FTS5 behavior, end-to-end through the public store API: the sync triggers
// must keep session_entries_fts in step with the DELETE+INSERT-per-session
// IndexSessionEntries write path and with PruneSessions. Lives in package
// store_test to reuse seedSession + openTestStore.

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
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
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT COUNT(*) FROM session_entries_fts WHERE session_id = ? AND session_entries_fts MATCH ?`,
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
