package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// execConn executes a no-result SQL statement on a raw sqlite.Conn.
// Used to inject data bypassing the Store's high-level API in push tests.
func execConn(conn *sqlite.Conn, sql string) error {
	return sqlitex.ExecuteTransient(conn, sql, nil)
}

// seedPublicationCursor sets legacy cursor state without exposing a production
// API that could bypass authoritative receipt persistence.
func seedPublicationCursor(t *testing.T, s *store.Store, sessionID ingest.SessionID, pushedAtMs int64, license schema.License) {
	t.Helper()
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)
	var licenseArg any
	if license != "" {
		licenseArg = license.String()
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET pushed_at = ?, license_id = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{pushedAtMs, licenseArg, sessionID.String()}}); err != nil {
		t.Fatalf("seed legacy publication cursor for %s: %v", sessionID, err)
	}
	if changes := conn.Changes(); changes != 1 {
		t.Fatalf("seed legacy publication cursor for %s changed %d rows, want 1", sessionID, changes)
	}
}

// ---------------------------------------------------------------------------
// Migration v4 tests
// ---------------------------------------------------------------------------

// TestStore_MigrationV4_PushedAtColumn verifies that the pushed_at column
// exists on the sessions table after migration v4 applies.
func TestStore_MigrationV4_PushedAtColumn(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify user_version >= 4 (V4 migration and all subsequent applied).
	var userVersion int
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			userVersion = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("query user_version: %v", err)
	}
	if userVersion < 4 {
		t.Errorf("expected user_version >= 4 after all migrations, got %d", userVersion)
	}

	// Insert a session and verify pushed_at defaults to NULL.
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	pushedAtNullCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM sessions WHERE pushed_at IS NULL AND session_id = ?`,
		"11111111-1111-1111-1111-111111111111")
	if pushedAtNullCount != 1 {
		t.Errorf("expected pushed_at IS NULL for new session, count = %d", pushedAtNullCount)
	}
}

// TestStore_MigrationV4_PushLogTable verifies that the push_log table
// was created with the correct schema (STRICT, all expected columns).
func TestStore_MigrationV4_PushLogTable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify push_log table exists.
	count := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='push_log'`)
	if count != 1 {
		t.Fatalf("push_log table not found after migration v4")
	}

	// Verify STRICT mode: inserting TEXT into INTEGER column should fail.
	err := execConn(conn, `INSERT INTO push_log (started_at, village_url) VALUES ('not-a-number', 'https://example.com')`)
	if err == nil {
		t.Error("expected STRICT mode to reject TEXT in INTEGER column, but insert succeeded")
	}
}

// ---------------------------------------------------------------------------
// InsertPushLog tests
// ---------------------------------------------------------------------------

// TestStore_InsertPushLog_Basic verifies that InsertPushLog records a row
// with all fields correctly persisted.
func TestStore_InsertPushLog_Basic(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UnixMilli()
	finishedAt := now + 5000
	errMsg := "partial failure"
	entry := ingest.PushLogEntry{
		StartedAt:       now,
		FinishedAt:      &finishedAt,
		VillageURL:      "https://village.example.com",
		SessionsPushed:  3,
		SessionsUpdated: 1,
		SessionsSkipped: 2,
		SessionsFailed:  0,
		ErrorMessage:    &errMsg,
		UserID:          "user-123",
		Username:        "testuser",
	}

	if err := s.InsertPushLog(ctx, entry); err != nil {
		t.Fatalf("InsertPushLog: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	count := queryInt(t, conn, `SELECT COUNT(*) FROM push_log`)
	if count != 1 {
		t.Errorf("push_log: expected 1 row, got %d", count)
	}

	pushed := queryInt(t, conn, `SELECT sessions_pushed FROM push_log WHERE id = 1`)
	if pushed != 3 {
		t.Errorf("sessions_pushed: expected 3, got %d", pushed)
	}

	updated := queryInt(t, conn, `SELECT sessions_updated FROM push_log WHERE id = 1`)
	if updated != 1 {
		t.Errorf("sessions_updated: expected 1, got %d", updated)
	}

	skipped := queryInt(t, conn, `SELECT sessions_skipped FROM push_log WHERE id = 1`)
	if skipped != 2 {
		t.Errorf("sessions_skipped: expected 2, got %d", skipped)
	}

	url := queryText(t, conn, `SELECT village_url FROM push_log WHERE id = 1`)
	if url != "https://village.example.com" {
		t.Errorf("village_url: expected %q, got %q", "https://village.example.com", url)
	}

	gotErrMsg := queryText(t, conn, `SELECT error_message FROM push_log WHERE id = 1`)
	if gotErrMsg != errMsg {
		t.Errorf("error_message: expected %q, got %q", errMsg, gotErrMsg)
	}

	username := queryText(t, conn, `SELECT username FROM push_log WHERE id = 1`)
	if username != "testuser" {
		t.Errorf("username: expected %q, got %q", "testuser", username)
	}
}

// TestStore_InsertPushLog_NullableFields verifies that nil FinishedAt and
// nil ErrorMessage are persisted as SQL NULL.
func TestStore_InsertPushLog_NullableFields(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := ingest.PushLogEntry{
		StartedAt:    time.Now().UnixMilli(),
		FinishedAt:   nil,
		VillageURL:   "https://village.example.com",
		ErrorMessage: nil,
	}

	if err := s.InsertPushLog(ctx, entry); err != nil {
		t.Fatalf("InsertPushLog: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	nullFinishedAtCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM push_log WHERE finished_at IS NULL`)
	if nullFinishedAtCount != 1 {
		t.Errorf("expected finished_at IS NULL, count = %d", nullFinishedAtCount)
	}

	nullErrMsgCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM push_log WHERE error_message IS NULL`)
	if nullErrMsgCount != 1 {
		t.Errorf("expected error_message IS NULL, count = %d", nullErrMsgCount)
	}
}

// ---------------------------------------------------------------------------
// UnpushedSessions tests
// ---------------------------------------------------------------------------

// TestStore_UnpushedSessions_ReturnsNewSessions verifies that freshly ingested
// sessions (pushed_at IS NULL) are returned with all fields populated.
func TestStore_UnpushedSessions_ReturnsNewSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "66666666-6666-6666-6666-666666666666", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500),
		makeStoreEntry(t, "77777777-7777-7777-7777-777777777777", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000060000, 2000, 800),
	}
	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	rows, err := s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("UnpushedSessions: expected 2 rows, got %d", len(rows))
	}

	// Ordered by start_ms DESC: row[0] is the later session.
	row := rows[0]
	if row.SessionID != "77777777-7777-7777-7777-777777777777" {
		t.Errorf("SessionID: expected %q, got %q", "77777777-7777-7777-7777-777777777777", row.SessionID)
	}
	if row.ModelHarness != string(defaults.HarnessClaudeCode) {
		t.Errorf("ModelHarness: expected %q, got %q", string(defaults.HarnessClaudeCode), row.ModelHarness)
	}
	if row.PushedAt != nil {
		t.Errorf("PushedAt: expected nil for new session, got %v", row.PushedAt)
	}
	if row.InputTokens != 2000 {
		t.Errorf("InputTokens: expected 2000, got %d", row.InputTokens)
	}
	if row.OutputTokens != 800 {
		t.Errorf("OutputTokens: expected 800, got %d", row.OutputTokens)
	}
	if row.TokensTotal != 2800 {
		t.Errorf("TokensTotal: expected 2800, got %d", row.TokensTotal)
	}
	if row.SourceFilePath == "" {
		t.Error("SourceFilePath: expected non-empty string")
	}
	if row.SourceFormat == "" {
		t.Error("SourceFormat: expected non-empty string")
	}
}

// TestStore_UnpushedSessions_PopulatesParentID exercises the REAL push-sessions
// SQL (parent_id SELECT col 21 + scanText(21)) end-to-end: it inserts a real
// root parent and a real subagent (non-empty parent_id) through InsertSessions,
// runs the actual UnpushedSessions query, and asserts PushSessionRow.ParentID is
// the parent UUID for the subagent and "" for the root. A column-index off-by-one
// would fail here (the StubPushStore-based push test cannot catch that).
func TestStore_UnpushedSessions_PopulatesParentID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	const (
		parentID = "11111111-1111-1111-1111-111111111111"
		subID    = "22222222-2222-2222-2222-222222222222"
	)
	hash := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

	parent := makeStoreEntry(t, parentID, hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
	sub := makeStoreEntry(t, subID, hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000060000, 200, 80)
	parentSID, err := ingest.NewSessionID(parentID)
	if err != nil {
		t.Fatalf("NewSessionID(parent): %v", err)
	}
	sub.Metadata.ParentUUID = &parentSID // makes sessions.parent_id non-NULL for the subagent

	if err := s.InsertSessions(ctx, []ingest.StoreEntry{parent, sub}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	rows, err := s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions: %v", err)
	}

	byID := map[string]ingest.PushSessionRow{}
	for _, r := range rows {
		byID[r.SessionID] = r
	}
	subRow, ok := byID[subID]
	if !ok {
		t.Fatalf("subagent %q missing from push rows: %+v", subID, rows)
	}
	if subRow.ParentID != parentID {
		t.Errorf("subagent ParentID = %q, want %q (real parent_id scan)", subRow.ParentID, parentID)
	}
	rootRow, ok := byID[parentID]
	if !ok {
		t.Fatalf("root %q missing from push rows", parentID)
	}
	if rootRow.ParentID != "" {
		t.Errorf("root ParentID = %q, want \"\" (NULL parent_id COALESCE'd)", rootRow.ParentID)
	}
}

// TestStore_UnpushedSessions_PopulatesGitRemote verifies the push read path
// surfaces host_slugs.git_remote on PushSessionRow (col 20) for branch-aware
// selection.
func TestStore_UnpushedSessions_PopulatesGitRemote(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	remote := "git@github.com:user/repo.git"
	branch := "main"
	hash := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	entry := makeStoreEntry(t, "88888888-8888-8888-8888-888888888888", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
	entry.Metadata.Git = ingest.GitContext{Remote: &remote, Branch: &branch}

	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	rows, err := s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].GitRemote != remote {
		t.Errorf("GitRemote: expected %q, got %q", remote, rows[0].GitRemote)
	}
}

// TestStore_UnpushedSessions_ExcludesSessionsWithoutMetrics verifies that
// sessions without a session_metrics row are excluded (JOIN, not LEFT JOIN).
func TestStore_UnpushedSessions_ExcludesSessionsWithoutMetrics(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Insert a session without metrics row by bypassing InsertSessions.
	// V23+: host_slugs uses (opaque_id, host_slug, ...); sessions uses opaque_host_id FK.
	const noMetricsOpaqueID = "ee00ff11ee00ff11ee00ff11ee00ff11ee00ff11ee00ff11ee00ff11ee00ff11"
	conn := takeConn(t, s.PoolForTest())
	err := execConn(conn, `INSERT INTO projects (project_hash, canonical_cwd) VALUES ('eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee', '/path')`)
	if err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("insert project: %v", err)
	}
	err = execConn(conn, `INSERT INTO host_slugs (opaque_id, host_slug) VALUES ('`+noMetricsOpaqueID+`', 'github.com-user-nometrics')`)
	if err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("insert host_slug: %v", err)
	}
	err = execConn(conn, `INSERT INTO sessions (
		session_id, model_harness, model_id, opaque_host_id, project_hash,
		start_ms, end_ms, ingested_ms, source_path, source_format, schema_version
	) VALUES (
		'88888888-8888-8888-8888-888888888888', 'claude-code', 'claude-opus-4-6',
		'`+noMetricsOpaqueID+`',
		'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee',
		1700000000000, 1700060000000, 1700120000000,
		'/test/path/session.jsonl', 'jsonl', 1
	)`)
	s.PoolForTest().Put(conn)
	if err != nil {
		t.Fatalf("insert session without metrics: %v", err)
	}

	rows, err := s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions: %v", err)
	}

	// The session without metrics must NOT appear.
	for _, row := range rows {
		if row.SessionID == "88888888-8888-8888-8888-888888888888" {
			t.Errorf("UnpushedSessions returned session without metrics: %s", row.SessionID)
		}
	}
}

// TestStore_UnpushedSessions_ReturnsReingested verifies that a pushed session
// re-ingested (ingested_ms > pushed_at) reappears in UnpushedSessions.
func TestStore_UnpushedSessions_ReturnsReingested(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	entry := makeStoreEntry(t, "99999999-9999-9999-9999-999999999999", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// Seed a legacy publication cursor at time T1 (before ingested_ms).
	pushedAtMs := int64(1700000500000) // between startMs and ingested_ms
	seedPublicationCursor(t, s, "99999999-9999-9999-9999-999999999999", pushedAtMs, "")

	// Session's ingested_ms (startMs+120000 = 1700000120000) < pushedAtMs (1700000500000)
	// so the session is considered already pushed — should NOT appear.
	rows, err := s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions after push: %v", err)
	}
	for _, row := range rows {
		if row.SessionID == "99999999-9999-9999-9999-999999999999" {
			t.Errorf("session should not appear in UnpushedSessions after being pushed")
		}
	}

	// Simulate re-ingest: set ingested_ms to a time AFTER pushed_at.
	conn := takeConn(t, s.PoolForTest())
	err = execConn(conn, `UPDATE sessions SET ingested_ms = 1700001000000 WHERE session_id = '99999999-9999-9999-9999-999999999999'`)
	s.PoolForTest().Put(conn)
	if err != nil {
		t.Fatalf("update ingested_ms: %v", err)
	}

	// Now the session should reappear (ingested_ms > pushed_at).
	rows, err = s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions after re-ingest: %v", err)
	}
	found := false
	for _, row := range rows {
		if row.SessionID == "99999999-9999-9999-9999-999999999999" {
			found = true
			if row.PushedAt == nil {
				t.Error("PushedAt should not be nil for re-ingested session")
			} else if *row.PushedAt != pushedAtMs {
				t.Errorf("PushedAt: expected %d, got %d", pushedAtMs, *row.PushedAt)
			}
		}
	}
	if !found {
		t.Error("re-ingested session not returned by UnpushedSessions")
	}
}

// ---------------------------------------------------------------------------
// UnpushedSessionsByProvider tests
// ---------------------------------------------------------------------------

// TestStore_UnpushedSessionsByProvider_FiltersProvider verifies that only
// sessions with the matching model_harness are returned.
func TestStore_UnpushedSessionsByProvider_FiltersProvider(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash1 := "1111111111111111111111111111111111111111111111111111111111111111"
	hash2 := "2222222222222222222222222222222222222222222222222222222222222222"
	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", hash1, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000000000, 1000, 500),
		makeStoreEntry(t, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", hash2, "github.com-user-repo2", defaults.HarnessOpenCode, 1700000060000, 2000, 800),
	}
	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	claudeRows, err := s.UnpushedSessionsByProvider(ctx, string(defaults.HarnessClaudeCode))
	if err != nil {
		t.Fatalf("UnpushedSessionsByProvider(claude): %v", err)
	}
	if len(claudeRows) != 1 {
		t.Errorf("expected 1 claude session, got %d", len(claudeRows))
	}
	if len(claudeRows) > 0 && claudeRows[0].ModelHarness != string(defaults.HarnessClaudeCode) {
		t.Errorf("ModelHarness: expected %q, got %q", string(defaults.HarnessClaudeCode), claudeRows[0].ModelHarness)
	}

	openCodeRows, err := s.UnpushedSessionsByProvider(ctx, string(defaults.HarnessOpenCode))
	if err != nil {
		t.Fatalf("UnpushedSessionsByProvider(opencode): %v", err)
	}
	if len(openCodeRows) != 1 {
		t.Errorf("expected 1 opencode session, got %d", len(openCodeRows))
	}
}

// ---------------------------------------------------------------------------
// AllPushableSessions tests
// ---------------------------------------------------------------------------

// TestStore_AllPushableSessions_IncludesPushed verifies that AllPushableSessions
// returns all sessions regardless of pushed_at (for --force flag).
func TestStore_AllPushableSessions_IncludesPushed(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "3333333333333333333333333333333333333333333333333333333333333333"
	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "cccccccc-cccc-cccc-cccc-cccccccccccc", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500),
		makeStoreEntry(t, "dddddddd-dddd-dddd-dddd-dddddddddddd", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000060000, 2000, 800),
	}
	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// Seed one legacy publication cursor so the read paths exercise both states.
	seedPublicationCursor(t, s, "cccccccc-cccc-cccc-cccc-cccccccccccc", 1700001000000, "")

	// UnpushedSessions should return only 1 (second session still unpushed).
	unpushed, err := s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions: %v", err)
	}
	if len(unpushed) != 1 {
		t.Errorf("UnpushedSessions: expected 1, got %d", len(unpushed))
	}

	// AllPushableSessions should return both (--force includes pushed sessions).
	all, err := s.AllPushableSessions(ctx)
	if err != nil {
		t.Fatalf("AllPushableSessions: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("AllPushableSessions: expected 2, got %d", len(all))
	}
}

// ---------------------------------------------------------------------------
// SessionsWithoutMetrics tests
// ---------------------------------------------------------------------------

// TestStore_SessionsWithoutMetrics_ReturnsHeldBack verifies that sessions
// without a session_metrics row are returned for warning the user.
func TestStore_SessionsWithoutMetrics_ReturnsHeldBack(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Insert a session with metrics (normal path via InsertSessions).
	hash1 := "4444444444444444444444444444444444444444444444444444444444444444"
	withMetrics := makeStoreEntry(t, "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee", hash1, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{withMetrics}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// Insert a session WITHOUT metrics row (simulate incomplete ingest).
	// V23+: host_slugs uses (opaque_id, host_slug, ...); sessions uses opaque_host_id FK.
	const noMetrics2OpaqueID = "55ff55ff55ff55ff55ff55ff55ff55ff55ff55ff55ff55ff55ff55ff55ff55ff"
	conn := takeConn(t, s.PoolForTest())
	err := execConn(conn, `INSERT INTO projects (project_hash, canonical_cwd) VALUES ('5555555555555555555555555555555555555555555555555555555555555555', '/path2')`)
	if err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("insert project: %v", err)
	}
	err = execConn(conn, `INSERT INTO host_slugs (opaque_id, host_slug) VALUES ('`+noMetrics2OpaqueID+`', 'github.com-user-nometrics2')`)
	if err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("insert host_slug: %v", err)
	}
	err = execConn(conn, `INSERT INTO sessions (
		session_id, model_harness, model_id, opaque_host_id, project_hash,
		start_ms, end_ms, ingested_ms, source_path, source_format, schema_version
	) VALUES (
		'ffffffff-ffff-ffff-ffff-ffffffffffff', 'claude-code', 'claude-opus-4-6',
		'`+noMetrics2OpaqueID+`',
		'5555555555555555555555555555555555555555555555555555555555555555',
		1700000000000, 1700060000000, 1700120000000,
		'/test/path/session2.jsonl', 'jsonl', 1
	)`)
	s.PoolForTest().Put(conn)
	if err != nil {
		t.Fatalf("insert session without metrics: %v", err)
	}

	ids, err := s.SessionsWithoutMetrics(ctx)
	if err != nil {
		t.Fatalf("SessionsWithoutMetrics: %v", err)
	}
	if len(ids) != 1 {
		t.Errorf("SessionsWithoutMetrics: expected 1, got %d", len(ids))
	}
	if len(ids) > 0 && ids[0].SessionID != "ffffffff-ffff-ffff-ffff-ffffffffffff" {
		t.Errorf("SessionsWithoutMetrics: expected %q, got %q", "ffffffff-ffff-ffff-ffff-ffffffffffff", ids[0].SessionID)
	}
	// The project identity travels with the held session so callers can report
	// only the ones belonging to the repository they are pushing.
	if len(ids) > 0 && ids[0].ProjectHash == "" {
		t.Error("SessionsWithoutMetrics: held session must carry its project identity")
	}
}

// TestStore_SessionsWithoutMetrics_EmptyWhenAllHaveMetrics verifies the
// happy-path returns an empty slice when all sessions have metrics.
func TestStore_SessionsWithoutMetrics_EmptyWhenAllHaveMetrics(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "6666666666666666666666666666666666666666666666666666666666666666"
	entry := makeStoreEntry(t, "66666666-6666-6666-6666-666666666666", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	ids, err := s.SessionsWithoutMetrics(ctx)
	if err != nil {
		t.Fatalf("SessionsWithoutMetrics: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("SessionsWithoutMetrics: expected empty slice, got %v", ids)
	}
}

// ---------------------------------------------------------------------------
// DurationMs conversion test
// ---------------------------------------------------------------------------

// TestStore_UnpushedSessions_DurationMsConversion verifies that duration_minutes
// (REAL, in minutes) is correctly converted to integer milliseconds via * 60000.
func TestStore_UnpushedSessions_DurationMsConversion(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// makeStoreEntry sets Stats.DurationMs = 60000 ms (1 minute)
	// → stored as 60000/60000.0 = 1.0 minutes in session_metrics
	// → retrieved as CAST(1.0 * 60000 AS INTEGER) = 60000 ms
	hash := "7777777777777777777777777777777777777777777777777777777777777777"
	entry := makeStoreEntry(t, "77777777-7777-7777-7777-777777777777", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	rows, err := s.UnpushedSessions(ctx)
	if err != nil {
		t.Fatalf("UnpushedSessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].DurationMs != 60000 {
		t.Errorf("DurationMs: expected 60000, got %d", rows[0].DurationMs)
	}
}
