package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// Test session IDs — each test uses a unique UUID to avoid collisions.
const (
	v4TestSID1 = "10000000-0000-0000-0000-000000000001"
	v4TestSID2 = "10000000-0000-0000-0000-000000000002"
	v4TestSID3 = "10000000-0000-0000-0000-000000000003"
	v4TestSID4 = "10000000-0000-0000-0000-000000000004"
)

// TestMigrationV4_AppliesCleanly verifies that migration v4 adds the
// full-depth indexing columns and all existing rows get depth=0.
func TestMigrationV4_AppliesCleanly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	seedTestSession(t, ctx, s, v4TestSID1)
	sid := ingest.SessionID(v4TestSID1)

	// Insert a depth=0 entry (v1 style: Depth field defaults to 0).
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

	// Read back and verify depth=0.
	got, err := s.ListEntries(ctx, sid)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	if got[0].Depth != 0 {
		t.Errorf("expected Depth=0, got %d", got[0].Depth)
	}
	if got[0].ParentIndex != nil {
		t.Errorf("expected ParentIndex=nil, got %v", got[0].ParentIndex)
	}
	if got[0].ToolInput != nil {
		t.Errorf("expected ToolInput=nil, got %v", got[0].ToolInput)
	}
	if got[0].ToolOutput != nil {
		t.Errorf("expected ToolOutput=nil, got %v", got[0].ToolOutput)
	}
}

// TestMigrationV4_Depth1RoundTrip verifies depth=1 entries can be written
// and read back with all v4 fields preserved.
func TestMigrationV4_Depth1RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	seedTestSession(t, ctx, s, v4TestSID2)
	sid := ingest.SessionID(v4TestSID2)

	parentIdx := 0
	toolInput := `{"file_path":"/src/main.go","content":"package main"}`
	toolOutput := `{"result":"ok"}`
	toolUseID := "toolu_abc123"

	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeToolUse,
			Role:       ingest.RoleAssistant,
			Depth:      0,
			HasToolUse: true,
		},
		{
			SessionID:   sid,
			EntryIndex:  1,
			Harness:     defaults.HarnessClaudeCode,
			EntryType:   ingest.EntryTypeToolUse,
			Role:        ingest.RoleAssistant,
			Depth:       1,
			ParentIndex: &parentIdx,
			ToolInput:   &toolInput,
			ToolCallID:  &toolUseID,
			HasToolUse:  true,
		},
		{
			SessionID:   sid,
			EntryIndex:  2,
			Harness:     defaults.HarnessClaudeCode,
			EntryType:   ingest.EntryTypeToolResult,
			Role:        ingest.RoleTool,
			Depth:       1,
			ParentIndex: &parentIdx,
			ToolOutput:  &toolOutput,
			ToolCallID:  &toolUseID,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	got, err := s.ListEntries(ctx, sid)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}

	// Verify depth=0 entry.
	if got[0].Depth != 0 {
		t.Errorf("entry[0]: expected Depth=0, got %d", got[0].Depth)
	}
	if got[0].ParentIndex != nil {
		t.Errorf("entry[0]: expected ParentIndex=nil, got %v", got[0].ParentIndex)
	}

	// Verify depth=1 tool_use entry.
	if got[1].Depth != 1 {
		t.Errorf("entry[1]: expected Depth=1, got %d", got[1].Depth)
	}
	if got[1].ParentIndex == nil || *got[1].ParentIndex != 0 {
		t.Errorf("entry[1]: expected ParentIndex=0, got %v", got[1].ParentIndex)
	}
	if got[1].ToolInput == nil || *got[1].ToolInput != toolInput {
		t.Errorf("entry[1]: ToolInput mismatch: got %v", got[1].ToolInput)
	}
	if got[1].ToolCallID == nil || *got[1].ToolCallID != toolUseID {
		t.Errorf("entry[1]: ToolCallID mismatch: got %v", got[1].ToolCallID)
	}

	// Verify depth=1 tool_result entry.
	if got[2].Depth != 1 {
		t.Errorf("entry[2]: expected Depth=1, got %d", got[2].Depth)
	}
	if got[2].ToolOutput == nil || *got[2].ToolOutput != toolOutput {
		t.Errorf("entry[2]: ToolOutput mismatch: got %v", got[2].ToolOutput)
	}
}

// TestMigrationV4_NullPointerFields verifies that nil pointer fields
// (ToolInput, ToolOutput, ParentIndex) are stored as SQL NULL.
func TestMigrationV4_NullPointerFields(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	seedTestSession(t, ctx, s, v4TestSID3)
	sid := ingest.SessionID(v4TestSID3)

	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleAssistant,
			Depth:      1,
			// ParentIndex, ToolInput, ToolOutput all nil
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Verify via raw SQL that the columns are NULL.
	pool := s.PoolForTest()
	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	defer pool.Put(conn)

	var parentNull, toolInNull, toolOutNull bool
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT parent_index IS NULL, tool_input IS NULL, tool_output IS NULL FROM session_entries WHERE session_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{v4TestSID3},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				parentNull = stmt.ColumnInt(0) == 1
				toolInNull = stmt.ColumnInt(1) == 1
				toolOutNull = stmt.ColumnInt(2) == 1
				return nil
			},
		},
	); err != nil {
		t.Fatalf("query nulls: %v", err)
	}

	if !parentNull {
		t.Error("expected parent_index to be NULL")
	}
	if !toolInNull {
		t.Error("expected tool_input to be NULL")
	}
	if !toolOutNull {
		t.Error("expected tool_output to be NULL")
	}
}

// TestMigrationV4_V1SessionsQueryable verifies that v1-style queries
// (WHERE depth=0) still work and return existing rows.
func TestMigrationV4_V1SessionsQueryable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s := openTestStore(t)

	seedTestSession(t, ctx, s, v4TestSID4)
	sid := ingest.SessionID(v4TestSID4)

	parentIdx := 0
	entries := []schema.SessionEntry{
		{
			SessionID:  sid,
			EntryIndex: 0,
			Harness:    defaults.HarnessClaudeCode,
			EntryType:  ingest.EntryTypeText,
			Role:       ingest.RoleAssistant,
			Depth:      0,
		},
		{
			SessionID:   sid,
			EntryIndex:  1,
			Harness:     defaults.HarnessClaudeCode,
			EntryType:   ingest.EntryTypeText,
			Role:        ingest.RoleAssistant,
			Depth:       1,
			ParentIndex: &parentIdx,
		},
	}

	if err := s.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// All entries (both depths) returned by ListEntries.
	all, err := s.ListEntries(ctx, sid)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 entries total, got %d", len(all))
	}

	// Verify v1 compat: depth=0 only query via raw SQL.
	pool := s.PoolForTest()
	conn, err := pool.Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	defer pool.Put(conn)

	var depth0Count int
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT COUNT(*) FROM session_entries WHERE session_id = ? AND depth = 0`,
		&sqlitex.ExecOptions{
			Args: []any{v4TestSID4},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				depth0Count = stmt.ColumnInt(0)
				return nil
			},
		},
	); err != nil {
		t.Fatalf("query depth=0: %v", err)
	}
	if depth0Count != 1 {
		t.Errorf("expected 1 depth=0 entry, got %d", depth0Count)
	}
}

// seedTestSession inserts the minimal rows needed for FK constraints:
// projects, host_slugs, sessions.
func seedTestSession(t *testing.T, ctx context.Context, s *store.Store, sessionID string) {
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
			Timestamp: ingest.TimestampInfo{
				Start:    1000,
				End:      2000,
				Ingested: &ingested,
			},
			Source: ingest.SourceInfo{
				FilePath: string(srcPath),
				Format:   ingest.SourceFormatJSONL,
			},
			Project: ingest.ProjectInfo{
				Hash:     ph,
				Name:     "test-project",
				FilePath: "/home/test/project",
			},
			Stats: ingest.StatsInfo{
				TurnCount:     10,
				ToolCallCount: 5,
			},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      defaults.HarnessClaudeCode,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}

	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
}
