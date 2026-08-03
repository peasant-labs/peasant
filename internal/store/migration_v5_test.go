package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// v5 test session IDs (valid UUIDs).
const (
	v5TestSID1 = "50000000-0000-0000-0000-000000000001"
	v5TestSID2 = "50000000-0000-0000-0000-000000000002"
)

// TestMigrationV5_SchemaAppliesCleanly verifies that migration v5 creates
// the session_entries_ext table with correct columns and index.
func TestMigrationV5_SchemaAppliesCleanly(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	ctx := context.Background()
	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer pool.Put(conn)

	// Verify table exists.
	var tableExists int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_entries_ext'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { tableExists = stmt.ColumnInt(0); return nil },
	})
	if tableExists != 1 {
		t.Error("session_entries_ext table not found after migration v5")
	}

	// Verify index exists.
	var idxExists int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_entries_ext_key'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { idxExists = stmt.ColumnInt(0); return nil },
	})
	if idxExists != 1 {
		t.Error("idx_entries_ext_key index not found after migration v5")
	}

	// Verify user_version >= 5 (V5 migration and all subsequent applied).
	var uv int
	sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { uv = stmt.ColumnInt(0); return nil },
	})
	if uv < 5 {
		t.Errorf("user_version: expected >= 5, got %d", uv)
	}
}

// TestMigrationV5_RoundTrip verifies that IndexSessionEntries writes ext rows
// for known keys in Extra JSON, and ListEntries re-hydrates them.
func TestMigrationV5_RoundTrip(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	sid := ingest.SessionID(v5TestSID1)
	seedTestSessionV5(t, ctx, s, string(sid))

	// Extra JSON with known keys.
	extra := `{"tokens_reasoning":500,"cache_read":100,"cache_write":50,"model_id":"claude-opus-4-6"}`
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleAssistant,
			Extra:      &extra,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Verify ext rows exist via raw SQL.
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer pool.Put(conn)

	var extCount int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error { extCount = stmt.ColumnInt(0); return nil },
	})
	if extCount != 4 {
		t.Errorf("expected 4 ext rows (tokens_reasoning, cache_read, cache_write, model_id), got %d", extCount)
	}

	// Verify ListEntries re-hydrates ext keys into Extra JSON.
	readEntries, err := s.ListEntries(ctx, sid)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(readEntries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(readEntries))
	}
	if readEntries[0].Extra == nil {
		t.Fatal("Extra should not be nil after re-hydration")
	}

	var extraMap map[string]any
	if err := json.Unmarshal([]byte(*readEntries[0].Extra), &extraMap); err != nil {
		t.Fatalf("unmarshal Extra: %v", err)
	}
	if v, ok := extraMap["tokens_reasoning"]; !ok || int(v.(float64)) != 500 {
		t.Errorf("tokens_reasoning: expected 500, got %v", extraMap["tokens_reasoning"])
	}
	if v, ok := extraMap["model_id"]; !ok || v.(string) != "claude-opus-4-6" {
		t.Errorf("model_id: expected claude-opus-4-6, got %v", extraMap["model_id"])
	}
}

// TestMigrationV5_UnknownFieldStaysInExtra verifies that unknown keys
// in Extra JSON are NOT promoted to the ext table.
func TestMigrationV5_UnknownFieldStaysInExtra(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	sid := ingest.SessionID(v5TestSID2)
	seedTestSessionV5(t, ctx, s, string(sid))

	// Extra JSON with only unknown keys.
	extra := `{"custom_field":"hello","my_metric":42}`
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleAssistant,
			Extra:      &extra,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// No ext rows should be created.
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer pool.Put(conn)

	var extCount int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error { extCount = stmt.ColumnInt(0); return nil },
	})
	if extCount != 0 {
		t.Errorf("expected 0 ext rows for unknown keys, got %d", extCount)
	}

	// Extra JSON should still be intact on read.
	readEntries, err := s.ListEntries(ctx, sid)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(readEntries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(readEntries))
	}
	if readEntries[0].Extra == nil {
		t.Fatal("Extra should not be nil for unknown fields")
	}
	var extraMap map[string]any
	if err := json.Unmarshal([]byte(*readEntries[0].Extra), &extraMap); err != nil {
		t.Fatalf("unmarshal Extra: %v", err)
	}
	if v, ok := extraMap["custom_field"]; !ok || v.(string) != "hello" {
		t.Errorf("custom_field: expected hello, got %v", extraMap["custom_field"])
	}
}

// TestMigrationV5_NilExtraNoExtRows verifies that entries with nil Extra
// produce no ext rows.
func TestMigrationV5_NilExtraNoExtRows(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	sid := ingest.SessionID(v5TestSID1)
	seedTestSessionV5(t, ctx, s, string(sid))

	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleUser,
			Extra:      nil,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer pool.Put(conn)

	var extCount int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error { extCount = stmt.ColumnInt(0); return nil },
	})
	if extCount != 0 {
		t.Errorf("expected 0 ext rows for nil Extra, got %d", extCount)
	}
}

// TestMigrationV5_MixedKnownAndUnknownFields verifies that entries with
// both known and unknown Extra keys get ext rows only for known keys,
// while unknown keys remain accessible via Extra JSON.
func TestMigrationV5_MixedKnownAndUnknownFields(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	sid := ingest.SessionID(v5TestSID1)
	seedTestSessionV5(t, ctx, s, string(sid))

	// Mixed: known keys + unknown key.
	extra := `{"tokens_reasoning":200,"model_id":"claude-sonnet-4-6","custom_tag":"important"}`
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleAssistant,
			Extra:      &extra,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer pool.Put(conn)

	// 2 ext rows: tokens_reasoning + model_id (not custom_tag).
	var extCount int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error { extCount = stmt.ColumnInt(0); return nil },
	})
	if extCount != 2 {
		t.Errorf("expected 2 ext rows (tokens_reasoning + model_id), got %d", extCount)
	}

	// ListEntries should have all keys merged in Extra.
	readEntries, err := s.ListEntries(ctx, sid)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(readEntries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(readEntries))
	}
	if readEntries[0].Extra == nil {
		t.Fatal("Extra should not be nil")
	}
	var extraMap map[string]any
	if err := json.Unmarshal([]byte(*readEntries[0].Extra), &extraMap); err != nil {
		t.Fatalf("unmarshal Extra: %v", err)
	}

	// Known keys should be present (from ext re-hydration).
	if v, ok := extraMap["tokens_reasoning"]; !ok || int(v.(float64)) != 200 {
		t.Errorf("tokens_reasoning: expected 200, got %v", extraMap["tokens_reasoning"])
	}
	if v, ok := extraMap["model_id"]; !ok || v.(string) != "claude-sonnet-4-6" {
		t.Errorf("model_id: expected claude-sonnet-4-6, got %v", extraMap["model_id"])
	}
	// Unknown key should be present (from extra column).
	if v, ok := extraMap["custom_tag"]; !ok || v.(string) != "important" {
		t.Errorf("custom_tag: expected important, got %v", extraMap["custom_tag"])
	}
}

// TestMigrationV5_CascadeDelete verifies that deleting session_entries
// cascades to session_entries_ext.
func TestMigrationV5_CascadeDelete(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	sid := ingest.SessionID(v5TestSID1)
	seedTestSessionV5(t, ctx, s, string(sid))

	extra := `{"tokens_reasoning":300,"model_id":"claude-opus-4-6"}`
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleAssistant,
			Extra:      &extra,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{})
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() { _ = pool.Close() })

	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer pool.Put(conn)

	// Verify ext rows exist.
	var extBefore int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error { extBefore = stmt.ColumnInt(0); return nil },
	})
	if extBefore == 0 {
		t.Fatal("expected ext rows before delete")
	}

	// Enable foreign keys on this connection (store.Open sets it, but raw pool doesn't).
	sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = ON`, nil)

	// Delete session_entries (FK CASCADE should delete ext rows).
	sqlitex.ExecuteTransient(conn, `DELETE FROM session_entries WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
	})

	var extAfter int
	sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_entries_ext WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args:       []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error { extAfter = stmt.ColumnInt(0); return nil },
	})
	if extAfter != 0 {
		t.Errorf("expected 0 ext rows after CASCADE delete, got %d", extAfter)
	}
}

// seedTestSessionV5 creates the FK chain required for session_entries in v5 tests.
func seedTestSessionV5(t *testing.T, ctx context.Context, s *store.Store, sessionID string) {
	t.Helper()

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	ph, err := ingest.NewProjectHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("NewProjectHash: %v", err)
	}
	hs, err := ingest.NewHostSlug(testutil.TestHostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug: %v", err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	srcPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}

	ingested := int64(3000)
	entry := ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Model:         model,
			HostSlug:      hs,
			Timestamp:     ingest.TimestampInfo{Start: 1000, End: 2000, Ingested: &ingested},
			Source:        ingest.SourceInfo{FilePath: string(srcPath), Format: ingest.SourceFormatJSONL},
			Project:       ingest.ProjectInfo{Hash: ph, Name: "test-project", FilePath: "/home/test/project"},
			Stats:         ingest.StatsInfo{TurnCount: 10, ToolCallCount: 5},
		},
		Session: ingest.DiscoveredSession{
			SessionID: sid, Harness: defaults.HarnessClaudeCode, SourceFormat: ingest.SourceFormatJSONL,
		},
	}

	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
}
