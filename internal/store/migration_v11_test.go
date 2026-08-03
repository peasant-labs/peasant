package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/schema"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// v11 test session IDs (valid UUIDs).
const (
	v11TestSID1 = "b1100000-0000-0000-0000-000000000001"
	v11TestSID2 = "b1100000-0000-0000-0000-000000000002"
)

// TestMigrationV11_SchemaAppliesCleanly verifies that migration v11 renames
// tool_use_id → tool_call_id and adds tool_kind + stop_reason columns.
func TestMigrationV11_SchemaAppliesCleanly(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify tool_call_id column exists (renamed from tool_use_id).
	toolCallIDExists := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name='tool_call_id'`)
	if toolCallIDExists != 1 {
		t.Error("session_entries missing tool_call_id column after migration v11 (renamed from tool_use_id)")
	}

	// Verify tool_use_id column no longer exists (renamed to tool_call_id).
	toolUseIDExists := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name='tool_use_id'`)
	if toolUseIDExists != 0 {
		t.Error("session_entries still has tool_use_id column; should have been renamed to tool_call_id by migration v11")
	}

	// Verify tool_kind column exists (TEXT, nullable).
	toolKindExists := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name='tool_kind'`)
	if toolKindExists != 1 {
		t.Error("session_entries missing tool_kind column after migration v11")
	}

	// Verify tool_kind is nullable (notnull=0).
	toolKindNotNull := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name='tool_kind' AND "notnull"=0`)
	if toolKindNotNull != 1 {
		t.Error("tool_kind should be nullable (notnull=0)")
	}

	// Verify stop_reason column exists (TEXT, nullable).
	stopReasonExists := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name='stop_reason'`)
	if stopReasonExists != 1 {
		t.Error("session_entries missing stop_reason column after migration v11")
	}

	// Verify stop_reason is nullable (notnull=0).
	stopReasonNotNull := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name='stop_reason' AND "notnull"=0`)
	if stopReasonNotNull != 1 {
		t.Error("stop_reason should be nullable (notnull=0)")
	}

	// Verify user_version >= 11 (V11 migration and all subsequent applied).
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 11 {
		t.Errorf("user_version: expected >= 11, got %d", uv)
	}
}

// TestMigrationV11_ToolCallIDRoundTrip verifies that the renamed tool_call_id
// column works end-to-end: insert via raw SQL, read back.
func TestMigrationV11_ToolCallIDRoundTrip(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	sid := ingest.SessionID(v11TestSID1)
	seedTestSessionV11(t, ctx, s, string(sid))

	toolCallID := "toolu_v11_abc123"
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeToolUse,
			Role:       ingest.RoleAssistant,
			HasToolUse: true,
			ToolCallID: &toolCallID,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Verify tool_call_id was stored correctly via raw SQL.
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

	var storedToolCallID string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT tool_call_id FROM session_entries WHERE session_id = ? AND entry_index = 0`,
		&sqlitex.ExecOptions{
			Args: []any{string(sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				storedToolCallID = stmt.ColumnText(0)
				return nil
			},
		},
	); err != nil {
		t.Fatalf("query tool_call_id: %v", err)
	}

	if storedToolCallID != toolCallID {
		t.Errorf("tool_call_id: expected %q, got %q", toolCallID, storedToolCallID)
	}
}

// TestMigrationV11_ToolKindAndStopReasonNullByDefault verifies that tool_kind
// and stop_reason are NULL for entries that don't set them.
func TestMigrationV11_ToolKindAndStopReasonNullByDefault(t *testing.T) {
	t.Parallel()

	dbPath := storetest.CopyGoldenDB(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	sid := ingest.SessionID(v11TestSID2)
	seedTestSessionV11(t, ctx, s, string(sid))

	// Insert an entry without ToolKind or StopReason.
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleAssistant,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Verify both columns are NULL via raw SQL.
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

	var toolKindNull, stopReasonNull bool
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT tool_kind IS NULL, stop_reason IS NULL FROM session_entries WHERE session_id = ? AND entry_index = 0`,
		&sqlitex.ExecOptions{
			Args: []any{string(sid)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				toolKindNull = stmt.ColumnInt(0) == 1
				stopReasonNull = stmt.ColumnInt(1) == 1
				return nil
			},
		},
	); err != nil {
		t.Fatalf("query nulls: %v", err)
	}

	if !toolKindNull {
		t.Error("expected tool_kind to be NULL for entry without ToolKind set")
	}
	if !stopReasonNull {
		t.Error("expected stop_reason to be NULL for entry without StopReason set")
	}
}

// TestMigrationV11_ToolKindAndStopReasonRawSQL verifies that tool_kind and
// stop_reason columns accept valid string values via raw SQL insert.
func TestMigrationV11_ToolKindAndStopReasonRawSQL(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Insert FK rows. V23+: host_slugs(opaque_id, host_slug, ...); sessions uses opaque_host_id.
	if err := sqlitex.ExecuteTransient(conn, `INSERT OR IGNORE INTO projects (project_hash, canonical_cwd) VALUES ('v11hash','/p')`, nil); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const v11SlugOpaqueID = "11aa22bb33cc44dd55ee66ff7788990011aa22bb33cc44dd55ee66ff77889900"
	if err := sqlitex.ExecuteTransient(conn, `INSERT OR IGNORE INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+v11SlugOpaqueID+`','v11slug','git@test')`, nil); err != nil {
		t.Fatalf("insert host_slug: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO sessions (session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format) VALUES ('v11-raw-sess','claude-code','m','`+v11SlugOpaqueID+`','v11hash',1,2,3,'/f','jsonl')`, nil); err != nil {
		t.Fatalf("insert session: %v", err)
	}

	// Insert entry with tool_kind and stop_reason via raw SQL.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role, tool_call_id, tool_kind, stop_reason)
		 VALUES ('v11-raw-sess', 0, 'claude', 'tool_use', 'assistant', 'toolu_test', 'read', 'end_turn')`, nil)
	if err != nil {
		t.Fatalf("insert session_entries with v11 columns: %v", err)
	}

	// Read back and verify.
	gotToolKind := queryText(t, conn, `SELECT tool_kind FROM session_entries WHERE session_id='v11-raw-sess' AND entry_index=0`)
	if gotToolKind != "read" {
		t.Errorf("tool_kind: expected %q, got %q", "read", gotToolKind)
	}

	gotStopReason := queryText(t, conn, `SELECT stop_reason FROM session_entries WHERE session_id='v11-raw-sess' AND entry_index=0`)
	if gotStopReason != "end_turn" {
		t.Errorf("stop_reason: expected %q, got %q", "end_turn", gotStopReason)
	}
}

// seedTestSessionV11 creates the FK chain required for session_entries in v11 tests.
func seedTestSessionV11(t *testing.T, ctx context.Context, s *store.Store, sessionID string) {
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
