package store_test

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

func TestStore_IndexSessionEntryBatch_WritesEntriesAndIndexState(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid1 := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
	sid2 := ingest.SessionID("bbbbbbbb-2222-4222-8222-bbbbbbbbbbbb")
	seedSession(t, s, string(sid1))
	seedSession(t, s, string(sid2))

	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{
		{SessionID: sid1, Entries: batchTestEntries(sid1, "alpha", 2), IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 1700000001000},
		{SessionID: sid2, Entries: batchTestEntries(sid2, "bravo", 1), IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 1700000002000},
	})
	assertBatchResult(t, results, 0, sid1, true)
	assertBatchResult(t, results, 1, sid2, true)
	assertEntryCount(t, s, sid1, 2)
	assertEntryCount(t, s, sid2, 1)
	assertIndexState(t, s, sid1, ingest.CurrentIndexVersion, 1700000001000)
	assertIndexState(t, s, sid2, ingest.CurrentIndexVersion, 1700000002000)
}

func TestStore_IndexSessionEntryBatch_SavepointKeepsLaterSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	badSession := ingest.SessionID("cccccccc-3333-4333-8333-cccccccccccc")
	goodSession := ingest.SessionID("dddddddd-4444-4444-8444-dddddddddddd")
	missingSession := ingest.SessionID("eeeeeeee-5555-4555-8555-eeeeeeeeeeee")
	seedSession(t, s, string(badSession))
	seedSession(t, s, string(goodSession))

	oldEntries := batchTestEntries(badSession, "old", 1)
	if err := s.IndexSessionEntries(ctx, badSession, oldEntries); err != nil {
		t.Fatalf("seed old entries: %v", err)
	}

	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{
		{
			SessionID: badSession,
			Entries: []schema.SessionEntry{{
				SessionID:      missingSession,
				EntryIndex:     0,
				Harness:        defaults.HarnessClaudeCode,
				EntryType:      schema.EntryTypeText,
				Role:           schema.RoleUser,
				ContentPreview: strPtr("bad foreign key"),
			}},
			IndexVersion: ingest.CurrentIndexVersion,
			IndexedAtMs:  1700000003000,
		},
		{SessionID: goodSession, Entries: batchTestEntries(goodSession, "good", 1), IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 1700000004000},
	})

	assertBatchResult(t, results, 0, badSession, false)
	assertBatchResult(t, results, 1, goodSession, true)
	assertEntryCount(t, s, badSession, 1)
	assertEntryContent(t, s, badSession, "old-0")
	assertEntryCount(t, s, goodSession, 1)
	assertEntryContent(t, s, goodSession, "good-0")
	assertIndexState(t, s, goodSession, ingest.CurrentIndexVersion, 1700000004000)
}

func TestStore_IndexSessionEntryBatch_SkipsUnchangedEntriesAndUpdatesIndexState(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-1111-4111-8111-999999999999")
	seedSession(t, s, string(sid))
	entries := batchTestEntries(sid, "same", 2)

	first := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Entries: entries, IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 1700000005000}})
	assertBatchResult(t, first, 0, sid, true)
	if first[0].Skipped {
		t.Fatal("first write reported skipped, want replacement for a previously unindexed session")
	}
	firstHash := sessionEntriesHash(t, s, sid)
	if len(firstHash) != 64 {
		t.Fatalf("session_entries_hash length = %d, want 64", len(firstHash))
	}

	second := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Entries: entries, IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 1700000006000}})
	assertBatchResult(t, second, 0, sid, true)
	if !second[0].Skipped {
		t.Fatal("second identical write did not report skipped")
	}
	if got := sessionEntriesHash(t, s, sid); got != firstHash {
		t.Fatalf("session_entries_hash changed after skipped write: %q != %q", got, firstHash)
	}
	assertEntryCount(t, s, sid, 2)
	assertIndexState(t, s, sid, ingest.CurrentIndexVersion, 1700000006000)
}

func TestStore_IndexSessionEntryBatch_RewritesWhenProjectionRowsAreMissing(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := ingest.SessionID("99999999-2222-4222-8222-999999999999")
	seedSession(t, s, string(sid))
	entries := batchTestEntries(sid, "projected", 1)
	entries[0].Extra = strPtr(`{"model_id":"claude-opus-4-6"}`)

	first := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Entries: entries, IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 1700000007000}})
	assertBatchResult(t, first, 0, sid, true)
	firstHash := sessionEntriesHash(t, s, sid)
	deleteSessionEntryExtRows(t, s, sid)
	if got := sessionEntryExtCount(t, s, sid); got != 0 {
		t.Fatalf("session_entries_ext rows after delete = %d, want 0", got)
	}

	second := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Entries: entries, IndexVersion: ingest.CurrentIndexVersion, IndexedAtMs: 1700000008000}})
	assertBatchResult(t, second, 0, sid, true)
	if second[0].Skipped {
		t.Fatal("write reported skipped while a derived projection row was missing")
	}
	if got := sessionEntriesHash(t, s, sid); got != firstHash {
		t.Fatalf("session_entries_hash changed after projection repair rewrite: %q != %q", got, firstHash)
	}
	if got := sessionEntryExtCount(t, s, sid); got != 1 {
		t.Fatalf("session_entries_ext rows after rewrite = %d, want 1", got)
	}
	assertIndexState(t, s, sid, ingest.CurrentIndexVersion, 1700000008000)
}

func batchTestEntries(sessionID ingest.SessionID, prefix string, count int) []schema.SessionEntry {
	entries := make([]schema.SessionEntry, count)
	for i := range entries {
		entries[i] = schema.SessionEntry{
			SessionID:      sessionID,
			EntryIndex:     i,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      schema.EntryTypeText,
			Role:           schema.RoleUser,
			ContentPreview: strPtr(prefix + "-" + string(rune('0'+i))),
		}
	}
	return entries
}

func assertBatchResult(t *testing.T, results []ingest.SessionEntryWriteResult, index int, sessionID ingest.SessionID, wantWritten bool) {
	t.Helper()
	if len(results) <= index {
		t.Fatalf("batch returned %d result(s), want result at index %d", len(results), index)
	}
	got := results[index]
	if got.SessionID != sessionID {
		t.Fatalf("results[%d].SessionID = %s, want %s", index, got.SessionID, sessionID)
	}
	if got.Written != wantWritten {
		t.Fatalf("results[%d].Written = %v, want %v, err=%v", index, got.Written, wantWritten, got.Err)
	}
	if wantWritten && got.Err != nil {
		t.Fatalf("results[%d].Err = %v, want nil", index, got.Err)
	}
	if !wantWritten && got.Err == nil {
		t.Fatalf("results[%d].Err = nil, want failure", index)
	}
}

func assertEntryCount(t *testing.T, s *store.Store, sessionID ingest.SessionID, want int) {
	t.Helper()
	entries, err := s.ListEntries(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListEntries(%s): %v", sessionID, err)
	}
	if len(entries) != want {
		t.Fatalf("ListEntries(%s) returned %d entries, want %d", sessionID, len(entries), want)
	}
}

func assertEntryContent(t *testing.T, s *store.Store, sessionID ingest.SessionID, want string) {
	t.Helper()
	entries, err := s.ListEntries(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ListEntries(%s): %v", sessionID, err)
	}
	if len(entries) == 0 || entries[0].ContentPreview == nil || *entries[0].ContentPreview != want {
		t.Fatalf("ListEntries(%s)[0].ContentPreview = %v, want %q", sessionID, entries, want)
	}
}

func assertIndexState(t *testing.T, s *store.Store, sessionID ingest.SessionID, wantVersion int, wantIndexedAt int64) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	var gotVersion int
	var gotIndexedAt int64
	err := sqlitex.ExecuteTransient(conn, `SELECT index_version, indexed_at FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			gotVersion = stmt.ColumnInt(0)
			gotIndexedAt = stmt.ColumnInt64(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("read index state for %s: %v", sessionID, err)
	}
	if gotVersion != wantVersion || gotIndexedAt != wantIndexedAt {
		t.Fatalf("index state for %s = version %d at %d, want version %d at %d", sessionID, gotVersion, gotIndexedAt, wantVersion, wantIndexedAt)
	}
}

func deleteSessionEntryExtRows(t *testing.T, s *store.Store, sessionID ingest.SessionID) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{string(sessionID)}}); err != nil {
		t.Fatalf("delete session_entries_ext rows for %s: %v", sessionID, err)
	}
}

func sessionEntryExtCount(t *testing.T, s *store.Store, sessionID ingest.SessionID) int {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	count := 0
	err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sessionID)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("count session_entries_ext rows for %s: %v", sessionID, err)
	}
	return count
}

func sessionEntriesHash(t *testing.T, s *store.Store, sessionID ingest.SessionID) string {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	return queryText(t, conn, `SELECT COALESCE(session_entries_hash, '') FROM sessions WHERE session_id = ?`, string(sessionID))
}
