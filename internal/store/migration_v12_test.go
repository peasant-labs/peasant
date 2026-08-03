package store_test

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// v12 test session IDs (valid UUIDs).
const (
	v12TestSID1 = "c1200000-0000-0000-0000-000000000001"
	v12TestSID2 = "c1200000-0000-0000-0000-000000000002"
	v12TestSID3 = "c1200000-0000-0000-0000-000000000003"
)

// ---------------------------------------------------------------------------
// TestMigrationV12Applies — migration applies, table exists with correct columns
// ---------------------------------------------------------------------------

// TestMigrationV12Applies verifies that migration v12 creates the session_commits
// table with all expected columns and sets user_version = 12.
func TestMigrationV12Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify session_commits table was created.
	tableCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_commits'`)
	if tableCount != 1 {
		t.Fatal("session_commits table not found after migration v12")
	}

	// Verify all expected columns exist.
	expectedCols := []string{
		"session_id", "commit_hash", "author_name", "author_email",
		"message", "commit_time", "author_time", "created_at",
	}
	for _, col := range expectedCols {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_commits') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("session_commits missing column %q after migration v12", col)
		}
	}

	// Verify session_id is NOT NULL (notnull=1).
	sessionIDNotNull := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_commits') WHERE name='session_id' AND "notnull"=1`)
	if sessionIDNotNull != 1 {
		t.Error("session_commits.session_id should be NOT NULL")
	}

	// Verify commit_hash is NOT NULL (notnull=1).
	hashNotNull := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_commits') WHERE name='commit_hash' AND "notnull"=1`)
	if hashNotNull != 1 {
		t.Error("session_commits.commit_hash should be NOT NULL")
	}

	// Verify attribution columns are nullable (notnull=0).
	for _, col := range []string{"author_name", "author_email", "message", "commit_time", "author_time"} {
		nullable := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_commits') WHERE name=? AND "notnull"=0`, col)
		if nullable != 1 {
			t.Errorf("session_commits.%s should be nullable (notnull=0)", col)
		}
	}

	// Verify created_at has a DEFAULT expression (dflt_value IS NOT NULL).
	createdAtDefault := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('session_commits') WHERE name='created_at' AND dflt_value IS NOT NULL`)
	if createdAtDefault != 1 {
		t.Error("session_commits.created_at should have a DEFAULT expression")
	}

	// Verify user_version >= 12 (V12 migration and all subsequent applied).
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 12 {
		t.Errorf("user_version: expected >= 12, got %d", uv)
	}
}

// ---------------------------------------------------------------------------
// TestSessionCommitsTableConstraints — FK and composite PK enforcement
// ---------------------------------------------------------------------------

// TestSessionCommitsTableConstraints verifies that:
//   - The FK constraint to sessions(session_id) is enforced
//   - The composite PK (session_id, commit_hash) rejects duplicates
func TestSessionCommitsTableConstraints(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Seed a valid session so FK inserts can succeed.
	sid := ingest.SessionID(v12TestSID1)
	seedTestSessionV12(t, ctx, s, string(sid))

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// FK enforcement: inserting a session_commits row for a non-existent session must fail.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_commits (session_id, commit_hash) VALUES ('non-existent-session', 'abc123')`, nil)
	if err == nil {
		t.Error("expected FK violation when inserting session_commits row for non-existent session, but insert succeeded")
	}

	// Valid insert: FK satisfied by the seeded session.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_commits (session_id, commit_hash, author_email) VALUES (?, ?, ?)`,
		&sqlitex.ExecOptions{Args: []any{string(sid), "abc123def456", "dev@example.com"}})
	if err != nil {
		t.Fatalf("valid session_commits insert failed: %v", err)
	}

	// Composite PK enforcement: re-inserting the same (session_id, commit_hash) must fail.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_commits (session_id, commit_hash) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{string(sid), "abc123def456"}})
	if err == nil {
		t.Error("expected PRIMARY KEY violation when inserting duplicate (session_id, commit_hash), but insert succeeded")
	}

	// Composite PK allows same commit_hash for a different session_id.
	sid2 := ingest.SessionID(v12TestSID2)
	seedTestSessionV12(t, ctx, s, string(sid2))

	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_commits (session_id, commit_hash) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{string(sid2), "abc123def456"}})
	if err != nil {
		t.Errorf("same commit_hash for different session should be allowed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestSessionCommitsIndexes — indexes are created
// ---------------------------------------------------------------------------

// TestSessionCommitsIndexes verifies that migration v12 creates both indexes:
// idx_session_commits_author and idx_session_commits_time.
func TestSessionCommitsIndexes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	authorIndexCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_session_commits_author'`)
	if authorIndexCount != 1 {
		t.Error("idx_session_commits_author index not found after migration v12")
	}

	timeIndexCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_session_commits_time'`)
	if timeIndexCount != 1 {
		t.Error("idx_session_commits_time index not found after migration v12")
	}
}

// ---------------------------------------------------------------------------
// TestUpsertSessionCommits — writer round-trip
// ---------------------------------------------------------------------------

// TestUpsertSessionCommits verifies that UpsertSessionCommits writes commit rows
// and that re-calling it replaces existing rows (DELETE + INSERT semantics).
func TestUpsertSessionCommits(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sid := ingest.SessionID(v12TestSID3)
	seedTestSessionV12(t, ctx, s, string(sid))

	commits := []ingest.CommitInfo{
		{
			Hash:        "aaaa1111bbbb2222cccc3333dddd4444eeee5555",
			AuthorName:  "Alice Dev",
			AuthorEmail: "alice@example.com",
			Message:     "feat: add session-to-commit linking",
			CommitTime:  1700000001000,
			AuthorTime:  1700000000000,
		},
		{
			Hash:        "ffff6666aaaa7777bbbb8888cccc9999dddd0000",
			AuthorName:  "Alice Dev",
			AuthorEmail: "alice@example.com",
			Message:     "fix: correct author email normalization",
			CommitTime:  1700000002000,
			AuthorTime:  1700000001500,
		},
	}

	if err := s.UpsertSessionCommits(ctx, sid, commits); err != nil {
		t.Fatalf("UpsertSessionCommits: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify 2 rows were inserted.
	rowCount := queryInt(t, conn, `SELECT COUNT(*) FROM session_commits WHERE session_id = ?`, string(sid))
	if rowCount != 2 {
		t.Errorf("session_commits: expected 2 rows, got %d", rowCount)
	}

	// Verify commit metadata was stored correctly.
	gotEmail := queryText(t, conn, `SELECT author_email FROM session_commits WHERE session_id = ? AND commit_hash = ?`,
		string(sid), "aaaa1111bbbb2222cccc3333dddd4444eeee5555")
	if gotEmail != "alice@example.com" {
		t.Errorf("author_email: expected %q, got %q", "alice@example.com", gotEmail)
	}

	gotMsg := queryText(t, conn, `SELECT message FROM session_commits WHERE session_id = ? AND commit_hash = ?`,
		string(sid), "ffff6666aaaa7777bbbb8888cccc9999dddd0000")
	if gotMsg != "fix: correct author email normalization" {
		t.Errorf("message: expected %q, got %q", "fix: correct author email normalization", gotMsg)
	}

	// Re-call with a different commit set — should replace (DELETE + INSERT).
	updatedCommits := []ingest.CommitInfo{
		{
			Hash:        "1234567890abcdef1234567890abcdef12345678",
			AuthorName:  "Alice Dev",
			AuthorEmail: "alice@example.com",
			Message:     "chore: update deps",
			CommitTime:  1700000003000,
			AuthorTime:  1700000003000,
		},
	}

	if err := s.UpsertSessionCommits(ctx, sid, updatedCommits); err != nil {
		t.Fatalf("UpsertSessionCommits (update): %v", err)
	}

	// Verify old rows are gone and new row is present.
	rowCount = queryInt(t, conn, `SELECT COUNT(*) FROM session_commits WHERE session_id = ?`, string(sid))
	if rowCount != 1 {
		t.Errorf("after update: session_commits expected 1 row, got %d", rowCount)
	}

	newHash := queryText(t, conn, `SELECT commit_hash FROM session_commits WHERE session_id = ?`, string(sid))
	if newHash != "1234567890abcdef1234567890abcdef12345678" {
		t.Errorf("after update: expected new hash, got %q", newHash)
	}
}

// TestUpsertSessionCommits_EmptySlice verifies that calling UpsertSessionCommits
// with an empty slice deletes existing rows and inserts nothing.
func TestUpsertSessionCommits_EmptySlice(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sid := ingest.SessionID(v12TestSID1)
	seedTestSessionV12(t, ctx, s, string(sid))

	// Insert an initial commit.
	initial := []ingest.CommitInfo{
		{Hash: "abc123def456abc123def456abc123def456abc1", AuthorEmail: "dev@example.com"},
	}
	if err := s.UpsertSessionCommits(ctx, sid, initial); err != nil {
		t.Fatalf("initial UpsertSessionCommits: %v", err)
	}

	// Call with empty slice — must delete all rows.
	if err := s.UpsertSessionCommits(ctx, sid, []ingest.CommitInfo{}); err != nil {
		t.Fatalf("empty UpsertSessionCommits: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	rowCount := queryInt(t, conn, `SELECT COUNT(*) FROM session_commits WHERE session_id = ?`, string(sid))
	if rowCount != 0 {
		t.Errorf("after empty upsert: expected 0 rows, got %d", rowCount)
	}
}

// TestUpsertSessionCommits_NullableFields verifies that empty attribution fields
// are stored as NULL (not empty string) in STRICT mode.
func TestUpsertSessionCommits_NullableFields(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Use v12TestSID2 for this test; avoid collision with empty-slice test.
	sid := ingest.SessionID(v12TestSID2)
	seedTestSessionV12(t, ctx, s, string(sid))

	// CommitInfo with only required fields populated; attribution fields are zero-value.
	commits := []ingest.CommitInfo{
		{Hash: "deadbeefdeadbeefdeadbeefdeadbeefdeadbee1"},
	}
	if err := s.UpsertSessionCommits(ctx, sid, commits); err != nil {
		t.Fatalf("UpsertSessionCommits: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify nullable attribution columns are NULL (not empty string).
	var authorNameNull, authorEmailNull, messageNull, commitTimeNull, authorTimeNull bool
	err := sqlitex.ExecuteTransient(conn,
		`SELECT
            author_name IS NULL,
            author_email IS NULL,
            message IS NULL,
            commit_time IS NULL,
            author_time IS NULL
        FROM session_commits WHERE session_id = ? AND commit_hash = ?`,
		&sqlitex.ExecOptions{
			Args: []any{string(sid), "deadbeefdeadbeefdeadbeefdeadbeefdeadbee1"},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				authorNameNull = stmt.ColumnInt(0) == 1
				authorEmailNull = stmt.ColumnInt(1) == 1
				messageNull = stmt.ColumnInt(2) == 1
				commitTimeNull = stmt.ColumnInt(3) == 1
				authorTimeNull = stmt.ColumnInt(4) == 1
				return nil
			},
		})
	if err != nil {
		t.Fatalf("query nullable fields: %v", err)
	}

	if !authorNameNull {
		t.Error("author_name: expected NULL for empty string, got non-NULL")
	}
	if !authorEmailNull {
		t.Error("author_email: expected NULL for empty string, got non-NULL")
	}
	if !messageNull {
		t.Error("message: expected NULL for empty string, got non-NULL")
	}
	if !commitTimeNull {
		t.Error("commit_time: expected NULL for zero value, got non-NULL")
	}
	if !authorTimeNull {
		t.Error("author_time: expected NULL for zero value, got non-NULL")
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// seedTestSessionV12 creates the FK prerequisite row in sessions for v12 tests.
func seedTestSessionV12(t *testing.T, ctx context.Context, s *store.Store, sessionID string) {
	t.Helper()

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Insert required dimension rows (use OR IGNORE to allow multiple sessions
	// in the same test to share the same project/host).
	// V23+: host_slugs(opaque_id, host_slug, ...); sessions uses opaque_host_id.
	if err := sqlitex.ExecuteTransient(conn, `INSERT OR IGNORE INTO projects (project_hash, canonical_cwd) VALUES ('v12projhashaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/proj')`, nil); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const v12SlugOpaqueID = "12aa12bb12cc12dd12ee12ff1234123412aa12bb12cc12dd12ee12ff12341234"
	if err := sqlitex.ExecuteTransient(conn, `INSERT OR IGNORE INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+v12SlugOpaqueID+`','v12slug','git@test')`, nil); err != nil {
		t.Fatalf("insert host_slug: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO sessions (session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format)
         VALUES (?, 'claude-code','claude-opus-4-6','`+v12SlugOpaqueID+`','v12projhashaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1,2,3,'/f','jsonl')`,
		&sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		t.Fatalf("insert session %s: %v", sessionID, err)
	}
}
