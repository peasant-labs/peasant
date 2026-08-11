package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/schema"
	"go.uber.org/goleak"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestMain(m *testing.M) {
	// Open a 2-connection pool instead of the default 10: every store.Open
	// otherwise opens 10 connections, each re-parsing the schema, and these
	// tests do hundreds of Opens. 2 (not 1) because some store operations take a
	// second connection while holding the first — a 1-connection pool deadlocks
	// them. Measured: internal/store -race 29s -> 9s. (Production keeps the
	// default; this env override is test-only.)
	os.Setenv(store.EnvPoolSize, "2")
	goleak.VerifyTestMain(m)
}

// takeConn is a test helper that checks out a pool connection or fails the test.
func takeConn(t *testing.T, pool *sqlitex.Pool) *sqlite.Conn {
	t.Helper()
	conn, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	return conn
}

// openTestStore opens a Store backed by a copy of the golden (pre-migrated) DB.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t)
}

func TestStore_Open_CreatesDB(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify all 6 tables exist by querying sqlite_master.
	tables := make(map[string]bool)
	err = sqlitex.ExecuteTransient(conn, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name;`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			tables[stmt.ColumnText(0)] = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}

	expected := []string{
		"projects",
		"host_slugs",
		"sessions",
		"session_metrics",
		"daily_summary",
		"daily_summary_harness",
	}
	for _, name := range expected {
		if !tables[name] {
			t.Errorf("table %q not found; got tables: %v", name, tables)
		}
	}
}

func TestStore_Open_TwiceIdempotent(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// First open: creates DB and tables.
	s1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}

	// Insert a sentinel row into host_slugs to verify persistence.
	// V23+: host_slugs PK is opaque_id (64-char hex); host_slug is a regular column.
	const sentinelOpaqueID = "aabbccddeeff00112233445566778899aabbccddeeff001122334455667788aa"
	conn := takeConn(t, s1.PoolForTest())
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+sentinelOpaqueID+`', 'test-slug', 'git@example.com');`, nil)
	if err != nil {
		s1.PoolForTest().Put(conn)
		t.Fatalf("insert sentinel: %v", err)
	}
	s1.PoolForTest().Put(conn)
	if err := s1.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	// Second open: should succeed without error and preserve data.
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() {
		if err := s2.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
	}()

	conn2 := takeConn(t, s2.PoolForTest())
	defer s2.PoolForTest().Put(conn2)

	var found bool
	err = sqlitex.ExecuteTransient(conn2, `SELECT 1 FROM host_slugs WHERE host_slug = 'test-slug';`, &sqlitex.ExecOptions{
		ResultFunc: func(_ *sqlite.Stmt) error {
			found = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query sentinel: %v", err)
	}
	if !found {
		t.Error("sentinel row not preserved after re-open")
	}
}

func TestStore_Migrations_ApplyV1(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify all expected tables exist:
	// 34 from v1-v22 + _install_salt from V23 + session_commands from V24
	// + lessons from V28 + memory_injection_log from V30 + lesson_sources from V32
	// + pulled_transcripts + pulled_annotations from V34
	// + session_entries_fts external-content FTS5 virtual table + its shadow tables
	//   (_data/_idx/_docsize/_config — _content is NOT created in external-content
	//   mode) from V35. V36 (user.custom_label seed) and V39 (turn_outcome/turn_flag
	//   seed) are data-only. V40 adds the durable association ledger and V41 adds
	//   its normalized annotation target table.
	var tableCount int
	err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%';`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			tableCount = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tableCount != 50 {
		t.Errorf("expected 50 tables including publication receipts and attempt diagnostics, got %d", tableCount)
	}

	// Verify all 44 indexes exist (v1-v24 base + idx_lessons_session/annotation from V28
	// + idx_injection_log_project from V30 + idx_lessons_dedup from V31
	// + idx_lesson_sources_lesson/session from V32
	// + idx_pulled_annotations_transcript/local_session from V34 + association
	// ledger and association-target indexes from V40/V41).
	var indexCount int
	err = sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name LIKE 'idx_%';`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			indexCount = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	if indexCount != 46 {
		t.Errorf("expected 46 indexes including publication receipt and retry-target lookups, got %d", indexCount)
	}

	// Verify STRICT mode by inserting TEXT into an INTEGER column on a table
	// without FK constraints. Using daily_summary avoids FK violations that
	// would mask the STRICT type check.
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO daily_summary (date_utc, session_count) VALUES ('2026-01-01', 'not-a-number');`, nil)
	if err == nil {
		t.Error("expected STRICT mode to reject TEXT in INTEGER column, but insert succeeded")
	}

	// Verify daily_summary_harness CHECK constraint on model_harness.
	err = sqlitex.ExecuteTransient(conn, `INSERT INTO daily_summary_harness (date_utc, model_harness) VALUES ('2026-01-01', 'invalid');`, nil)
	if err == nil {
		t.Error("expected CHECK constraint to reject invalid model_harness, but insert succeeded")
	}

	// Verify user_version was set by the migration framework.
	// All 43 migrations run, so user_version = 43. (V35 adds the FTS5 virtual
	// table + shadow tables; V36 seeds the user.custom_label annotation type;
	// V37/V38 add the sessions/pulled_transcripts license_id columns; V39 seeds
	// the quality.turn_outcome/quality.turn_flag annotation types. V40/V41 add
	// the durable association ledger and its annotation target table; V42 admits
	// Strike in the closed harness mirrors.)
	var userVersion int
	err = sqlitex.ExecuteTransient(conn, `PRAGMA user_version;`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			userVersion = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("query user_version: %v", err)
	}
	if userVersion != 43 {
		t.Errorf("expected user_version=43 after all migrations, got %d", userVersion)
	}
}

func TestStore_Close(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Double close should be safe (nil pool guard).
	if err := s.Close(); err != nil {
		t.Errorf("second Close: unexpected error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Writer test helpers
// ---------------------------------------------------------------------------

// makeStoreEntry builds a StoreEntry with sensible defaults for testing.
// All newtype fields are validated via constructors (NewSessionID, NewProjectHash,
// NewHostSlug, NewModelID, NewResolvedPath) to catch invalid test data early.
func makeStoreEntry(t *testing.T, sessionID, projectHash, hostSlug string, harness defaults.Harness, startMs int64, tokensIn, tokensOut int) ingest.StoreEntry {
	t.Helper()

	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", sessionID, err)
	}
	ph, err := ingest.NewProjectHash(projectHash)
	if err != nil {
		t.Fatalf("NewProjectHash(%q): %v", projectHash, err)
	}
	hs, err := ingest.NewHostSlug(hostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug(%q): %v", hostSlug, err)
	}
	model, err := ingest.NewModelID("claude-opus-4-6")
	if err != nil {
		t.Fatalf("NewModelID: %v", err)
	}
	srcPath, err := ingest.NewResolvedPath("/test/path/session.jsonl")
	if err != nil {
		t.Fatalf("NewResolvedPath: %v", err)
	}

	ingested := startMs + 120000
	return ingest.StoreEntry{
		Metadata: &ingest.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  harness,
			Model:         model,
			HostSlug:      hs,
			Timestamp: ingest.TimestampInfo{
				Start:    startMs,
				End:      startMs + 60000,
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
				SubagentCount: 0,
				DurationMs:    60000,
				TokensIn:      tokensIn,
				TokensOut:     tokensOut,
			},
			Version:     "2.1.14",
			Subagents:   []ingest.SubagentRef{},
			Diagnostics: ingest.DiagnosticsInfo{Warnings: []ingest.DiagnosticEntry{}},
		},
		Session: ingest.DiscoveredSession{
			SessionID:    sid,
			Harness:      harness,
			SourcePath:   srcPath,
			SourceFormat: ingest.SourceFormatJSONL,
		},
	}
}

// queryInt executes a SQL query returning a single integer value.
func queryInt(t *testing.T, conn *sqlite.Conn, sql string, args ...any) int {
	t.Helper()
	var result int
	err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			result = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("queryInt(%q): %v", sql, err)
	}
	return result
}

// queryFloat executes a SQL query returning a single float64 value.
func queryFloat(t *testing.T, conn *sqlite.Conn, sql string, args ...any) float64 {
	t.Helper()
	var result float64
	err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			result = stmt.ColumnFloat(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("queryFloat(%q): %v", sql, err)
	}
	return result
}

// queryText executes a SQL query returning a single text value.
func queryText(t *testing.T, conn *sqlite.Conn, sql string, args ...any) string {
	t.Helper()
	var result string
	err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			result = stmt.ColumnText(0)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("queryText(%q): %v", sql, err)
	}
	return result
}

// ---------------------------------------------------------------------------
// Writer tests
// ---------------------------------------------------------------------------

func TestStore_InsertSessions_Batch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// 64-char lowercase hex hashes (validated by NewProjectHash).
	hash1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hash2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000000000, 1000, 500),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000060000, 2000, 800),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash2, "github.com-user-repo2", defaults.HarnessOpenCode, 1700000120000, 3000, 1200),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify projects table: 2 distinct project hashes.
	projCount := queryInt(t, conn, `SELECT COUNT(*) FROM projects`)
	if projCount != 2 {
		t.Errorf("projects: expected 2 rows, got %d", projCount)
	}

	// Verify host_slugs table: 2 distinct host slugs.
	hostCount := queryInt(t, conn, `SELECT COUNT(*) FROM host_slugs`)
	if hostCount != 2 {
		t.Errorf("host_slugs: expected 2 rows, got %d", hostCount)
	}

	// Verify sessions table: 3 rows.
	sessCount := queryInt(t, conn, `SELECT COUNT(*) FROM sessions`)
	if sessCount != 3 {
		t.Errorf("sessions: expected 3 rows, got %d", sessCount)
	}

	// Verify session_metrics table: 3 rows.
	metricCount := queryInt(t, conn, `SELECT COUNT(*) FROM session_metrics`)
	if metricCount != 3 {
		t.Errorf("session_metrics: expected 3 rows, got %d", metricCount)
	}

	// Spot-check a specific session's fields.
	harness := queryText(t, conn, `SELECT model_harness FROM sessions WHERE session_id = ?`, "33333333-3333-3333-3333-333333333333")
	if harness != string(defaults.HarnessOpenCode) {
		t.Errorf("session model_harness: expected %q, got %q", string(defaults.HarnessOpenCode), harness)
	}

	tokIn := queryInt(t, conn, `SELECT input_tokens FROM session_metrics WHERE session_id = ?`, "11111111-1111-1111-1111-111111111111")
	if tokIn != 1000 {
		t.Errorf("input_tokens: expected 1000, got %d", tokIn)
	}

	tokOut := queryInt(t, conn, `SELECT output_tokens FROM session_metrics WHERE session_id = ?`, "11111111-1111-1111-1111-111111111111")
	if tokOut != 500 {
		t.Errorf("output_tokens: expected 500, got %d", tokOut)
	}

	// tokens_total no longer exists as a GENERATED column in session_metrics after v2 migration.
	// It is now computed at query time via COALESCE(input_tokens,0)+COALESCE(output_tokens,0).
	// Verify via the daily_summary which stores the aggregated total.

	// InsertSessions no longer computes daily_summary (decoupled).
	// Call UpdateDailySummary explicitly.
	if err := s.UpdateDailySummary(ctx, []string{"2023-11-14"}); err != nil {
		t.Fatalf("UpdateDailySummary: %v", err)
	}
	dsCount := queryInt(t, conn, `SELECT COUNT(*) FROM daily_summary`)
	if dsCount != 1 {
		t.Errorf("daily_summary: expected 1 row (all sessions on same day), got %d", dsCount)
	}
	dsSessions := queryInt(t, conn, `SELECT session_count FROM daily_summary WHERE date_utc = '2023-11-14'`)
	if dsSessions != 3 {
		t.Errorf("daily_summary session_count: expected 3, got %d", dsSessions)
	}
}

func TestStore_InsertSessions_AllowsCursorProvider(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, "c0000000-0000-0000-0000-000000000001", hash, "github.com-cursor-repo", defaults.HarnessCursor, 1700000000000, 100, 50)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions cursor provider: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	harness := queryText(t, conn, `SELECT model_harness FROM sessions WHERE session_id = ?`, "c0000000-0000-0000-0000-000000000001")
	if harness != string(defaults.HarnessCursor) {
		t.Errorf("model_harness: expected %q, got %q", string(defaults.HarnessCursor), harness)
	}
}

func TestStore_InsertSessions_EmptyBatch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Empty batch should be a no-op, not an error.
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{}); err != nil {
		t.Fatalf("InsertSessions(empty): %v", err)
	}
	if err := s.InsertSessions(ctx, nil); err != nil {
		t.Fatalf("InsertSessions(nil): %v", err)
	}
}

func TestStore_InsertSessions_NilMetadata(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Mix of entries: one valid, one with nil Metadata.
	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash, "github.com-test", defaults.HarnessClaudeCode, 1700000000000, 1000, 500),
		{Metadata: nil}, // nil Metadata — should be silently skipped
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Only the valid entry should be inserted.
	sessCount := queryInt(t, conn, `SELECT COUNT(*) FROM sessions`)
	if sessCount != 1 {
		t.Errorf("sessions: expected 1 row (nil metadata skipped), got %d", sessCount)
	}
}

func TestStore_InsertSessions_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	entry := makeStoreEntry(t, "44444444-4444-4444-4444-444444444444", hash, "github.com-user-repo", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)

	// Insert once.
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("first InsertSessions: %v", err)
	}

	// Modify tokens and re-insert (INSERT OR REPLACE should upsert, not duplicate).
	entry.Metadata.Stats.TokensIn = 2000
	entry.Metadata.Stats.TokensOut = 900
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("second InsertSessions: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	sessCount := queryInt(t, conn, `SELECT COUNT(*) FROM sessions`)
	if sessCount != 1 {
		t.Errorf("sessions: expected 1 row after double insert, got %d", sessCount)
	}

	metricCount := queryInt(t, conn, `SELECT COUNT(*) FROM session_metrics`)
	if metricCount != 1 {
		t.Errorf("session_metrics: expected 1 row after double insert, got %d", metricCount)
	}

	projCount := queryInt(t, conn, `SELECT COUNT(*) FROM projects`)
	if projCount != 1 {
		t.Errorf("projects: expected 1 row after double insert, got %d", projCount)
	}

	// Verify host_slugs also deduplicated.
	hostCount := queryInt(t, conn, `SELECT COUNT(*) FROM host_slugs`)
	if hostCount != 1 {
		t.Errorf("host_slugs: expected 1 row after double insert, got %d", hostCount)
	}

	// Verify upsert updated field values to the second insert's values.
	tokIn := queryInt(t, conn, `SELECT input_tokens FROM session_metrics WHERE session_id = ?`, "44444444-4444-4444-4444-444444444444")
	if tokIn != 2000 {
		t.Errorf("input_tokens after upsert: expected 2000, got %d", tokIn)
	}
	tokOut := queryInt(t, conn, `SELECT output_tokens FROM session_metrics WHERE session_id = ?`, "44444444-4444-4444-4444-444444444444")
	if tokOut != 900 {
		t.Errorf("output_tokens after upsert: expected 900, got %d", tokOut)
	}
}

func TestStore_InsertSessions_DimensionDedup(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	// Two sessions sharing the same project_hash and host_slug.
	hash := "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "55555555-5555-5555-5555-555555555555", hash, "github.com-shared-slug", defaults.HarnessClaudeCode, 1700000000000, 100, 50),
		makeStoreEntry(t, "66666666-6666-6666-6666-666666666666", hash, "github.com-shared-slug", defaults.HarnessClaudeCode, 1700000060000, 200, 100),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// projects should have exactly 1 row (INSERT OR IGNORE deduplicates).
	projCount := queryInt(t, conn, `SELECT COUNT(*) FROM projects`)
	if projCount != 1 {
		t.Errorf("projects: expected 1 row (dedup), got %d", projCount)
	}

	// host_slugs should have exactly 1 row.
	hostCount := queryInt(t, conn, `SELECT COUNT(*) FROM host_slugs`)
	if hostCount != 1 {
		t.Errorf("host_slugs: expected 1 row (dedup), got %d", hostCount)
	}

	// But sessions should have 2 distinct rows.
	sessCount := queryInt(t, conn, `SELECT COUNT(*) FROM sessions`)
	if sessCount != 2 {
		t.Errorf("sessions: expected 2 rows, got %d", sessCount)
	}
}

func TestStore_UpdateDailySummary_MultiDay(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	// Day 1: 2024-01-15 (1705276800000 = 2024-01-15T00:00:00Z in ms)
	day1Ms := int64(1705276800000)
	// Day 2: 2024-01-16 (1705363200000 = 2024-01-16T00:00:00Z in ms)
	day2Ms := int64(1705363200000)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "77777777-7777-7777-7777-777777777777", hash, "github.com-test", defaults.HarnessClaudeCode, day1Ms, 1000, 500),
		makeStoreEntry(t, "88888888-8888-8888-8888-888888888888", hash, "github.com-test", defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800),
		makeStoreEntry(t, "99999999-9999-9999-9999-999999999999", hash, "github.com-test", defaults.HarnessOpenCode, day2Ms, 3000, 1200),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// UpdateDailySummary as standalone re-computation (idempotent with InsertSessions).
	if err := s.UpdateDailySummary(ctx, []string{"2024-01-15", "2024-01-16"}); err != nil {
		t.Fatalf("UpdateDailySummary: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// daily_summary should have 2 rows (one per day).
	dsCount := queryInt(t, conn, `SELECT COUNT(*) FROM daily_summary`)
	if dsCount != 2 {
		t.Errorf("daily_summary: expected 2 rows, got %d", dsCount)
	}

	// Day 1: 2 sessions, tokens_in = 1000+2000 = 3000, tokens_out = 500+800 = 1300.
	d1Sessions := queryInt(t, conn, `SELECT session_count FROM daily_summary WHERE date_utc = ?`, "2024-01-15")
	if d1Sessions != 2 {
		t.Errorf("day1 session_count: expected 2, got %d", d1Sessions)
	}
	d1TokIn := queryInt(t, conn, `SELECT tokens_in FROM daily_summary WHERE date_utc = ?`, "2024-01-15")
	if d1TokIn != 3000 {
		t.Errorf("day1 tokens_in: expected 3000, got %d", d1TokIn)
	}
	d1TokOut := queryInt(t, conn, `SELECT tokens_out FROM daily_summary WHERE date_utc = ?`, "2024-01-15")
	if d1TokOut != 1300 {
		t.Errorf("day1 tokens_out: expected 1300, got %d", d1TokOut)
	}
	d1TokTotal := queryInt(t, conn, `SELECT tokens_total FROM daily_summary WHERE date_utc = ?`, "2024-01-15")
	if d1TokTotal != 4300 {
		t.Errorf("day1 tokens_total: expected 4300, got %d", d1TokTotal)
	}
	d1ToolCalls := queryInt(t, conn, `SELECT tool_call_count FROM daily_summary WHERE date_utc = ?`, "2024-01-15")
	if d1ToolCalls != 10 { // 5 + 5
		t.Errorf("day1 tool_call_count: expected 10, got %d", d1ToolCalls)
	}

	// Day 1: avg_turns = AVG(10, 10) = 10.0, avg_duration_ms = AVG(60000, 60000) = 60000.0
	d1AvgTurns := queryFloat(t, conn, `SELECT avg_turns FROM daily_summary WHERE date_utc = ?`, "2024-01-15")
	if d1AvgTurns != 10.0 {
		t.Errorf("day1 avg_turns: expected 10.0, got %f", d1AvgTurns)
	}
	d1AvgDur := queryFloat(t, conn, `SELECT avg_duration_ms FROM daily_summary WHERE date_utc = ?`, "2024-01-15")
	if d1AvgDur != 60000.0 {
		t.Errorf("day1 avg_duration_ms: expected 60000.0, got %f", d1AvgDur)
	}

	// Day 2: 1 session, tokens_in = 3000, tokens_out = 1200.
	d2Sessions := queryInt(t, conn, `SELECT session_count FROM daily_summary WHERE date_utc = ?`, "2024-01-16")
	if d2Sessions != 1 {
		t.Errorf("day2 session_count: expected 1, got %d", d2Sessions)
	}
	d2TokIn := queryInt(t, conn, `SELECT tokens_in FROM daily_summary WHERE date_utc = ?`, "2024-01-16")
	if d2TokIn != 3000 {
		t.Errorf("day2 tokens_in: expected 3000, got %d", d2TokIn)
	}

	// daily_summary_harness: day1 has 1 harness (claude), day2 has 1 harness (opencode).
	dshCount := queryInt(t, conn, `SELECT COUNT(*) FROM daily_summary_harness`)
	if dshCount != 2 {
		t.Errorf("daily_summary_harness: expected 2 rows, got %d", dshCount)
	}

	d1ClaudeCount := queryInt(t, conn, `SELECT session_count FROM daily_summary_harness WHERE date_utc = ? AND model_harness = ?`, "2024-01-15", string(defaults.HarnessClaudeCode))
	if d1ClaudeCount != 2 {
		t.Errorf("day1 claude session_count: expected 2, got %d", d1ClaudeCount)
	}

	d2OCHarness := queryText(t, conn, `SELECT model_harness FROM daily_summary_harness WHERE date_utc = ?`, "2024-01-16")
	if d2OCHarness != string(defaults.HarnessOpenCode) {
		t.Errorf("day2 harness: expected %q, got %q", string(defaults.HarnessOpenCode), d2OCHarness)
	}
}

func TestStore_UpdateDailySummary_RecomputeOnReinsert(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	dayMs := int64(1705276800000) // 2024-01-15T00:00:00Z

	// First insert: 1000 tokens_in, 500 tokens_out.
	entry := makeStoreEntry(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", hash, "github.com-recompute", defaults.HarnessClaudeCode, dayMs, 1000, 500)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("first InsertSessions: %v", err)
	}
	if err := s.UpdateDailySummary(ctx, []string{"2024-01-15"}); err != nil {
		t.Fatalf("first UpdateDailySummary: %v", err)
	}

	// Verify first summary values using a scoped connection.
	func() {
		conn := takeConn(t, s.PoolForTest())
		defer s.PoolForTest().Put(conn)

		tokIn1 := queryInt(t, conn, `SELECT tokens_in FROM daily_summary WHERE date_utc = '2024-01-15'`)
		if tokIn1 != 1000 {
			t.Errorf("first tokens_in: expected 1000, got %d", tokIn1)
		}
		sessCount1 := queryInt(t, conn, `SELECT session_count FROM daily_summary WHERE date_utc = '2024-01-15'`)
		if sessCount1 != 1 {
			t.Errorf("first session_count: expected 1, got %d", sessCount1)
		}
	}()

	// Re-insert with different metrics (INSERT OR REPLACE updates).
	entry.Metadata.Stats.TokensIn = 9999
	entry.Metadata.Stats.TokensOut = 8888
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("second InsertSessions: %v", err)
	}
	if err := s.UpdateDailySummary(ctx, []string{"2024-01-15"}); err != nil {
		t.Fatalf("second UpdateDailySummary: %v", err)
	}

	// Verify recomputed summary values.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Session count should still be 1 (upsert, not duplicate).
	sessCount2 := queryInt(t, conn, `SELECT session_count FROM daily_summary WHERE date_utc = '2024-01-15'`)
	if sessCount2 != 1 {
		t.Errorf("recomputed session_count: expected 1, got %d", sessCount2)
	}

	// Should reflect the NEW metrics, not accumulate with old ones.
	tokIn2 := queryInt(t, conn, `SELECT tokens_in FROM daily_summary WHERE date_utc = '2024-01-15'`)
	if tokIn2 != 9999 {
		t.Errorf("recomputed tokens_in: expected 9999, got %d", tokIn2)
	}

	tokOut2 := queryInt(t, conn, `SELECT tokens_out FROM daily_summary WHERE date_utc = '2024-01-15'`)
	if tokOut2 != 8888 {
		t.Errorf("recomputed tokens_out: expected 8888, got %d", tokOut2)
	}

	tokTotal2 := queryInt(t, conn, `SELECT tokens_total FROM daily_summary WHERE date_utc = '2024-01-15'`)
	if tokTotal2 != 9999+8888 {
		t.Errorf("recomputed tokens_total: expected %d, got %d", 9999+8888, tokTotal2)
	}

	// Verify harness row also recomputed.
	harnessIn := queryInt(t, conn, `SELECT tokens_in FROM daily_summary_harness WHERE date_utc = '2024-01-15' AND model_harness = 'claude-code'`)
	if harnessIn != 9999 {
		t.Errorf("harness recomputed tokens_in: expected 9999, got %d", harnessIn)
	}
}

// ---------------------------------------------------------------------------
// Reader tests
// ---------------------------------------------------------------------------

func TestStore_AllSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hash2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000000000, 1000, 500),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000060000, 2000, 800),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash2, "github.com-user-repo2", defaults.HarnessOpenCode, 1700000120000, 3000, 1200),
	}
	worktree := "/home/test/project-worktree"
	entries[2].Metadata.Git.Worktree = &worktree

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	rows, err := s.AllSessions(ctx)
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("AllSessions: expected 3 rows, got %d", len(rows))
	}

	// Ordered by start_ms DESC: session 33 (120000), 22 (60000), 11 (0)
	if rows[0].SessionID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("row[0] session_id: expected 33..., got %s", rows[0].SessionID)
	}
	if rows[1].SessionID != "22222222-2222-2222-2222-222222222222" {
		t.Errorf("row[1] session_id: expected 22..., got %s", rows[1].SessionID)
	}
	if rows[2].SessionID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("row[2] session_id: expected 11..., got %s", rows[2].SessionID)
	}

	// Spot-check fields on first row (latest session).
	r := rows[0]
	if r.ModelHarness != string(defaults.HarnessOpenCode) {
		t.Errorf("row[0] model_harness: expected %q, got %q", string(defaults.HarnessOpenCode), r.ModelHarness)
	}
	if r.ModelID != "claude-opus-4-6" {
		t.Errorf("row[0] model_id: expected %q, got %q", "claude-opus-4-6", r.ModelID)
	}
	if r.HostSlug != "github.com-user-repo2" {
		t.Errorf("row[0] host_slug: expected %q, got %q", "github.com-user-repo2", r.HostSlug)
	}
	if r.ProjectHash != hash2 {
		t.Errorf("row[0] project_hash: expected %q, got %q", hash2, r.ProjectHash)
	}
	// V23+: ProjectName = canonical_cwd = Project.FilePath from StoreEntry.
	if r.ProjectName != "/home/test/project" {
		t.Errorf("row[0] project_name: expected %q, got %q", "/home/test/project", r.ProjectName)
	}
	if r.GitWorktree != worktree {
		t.Errorf("row[0] git_worktree: expected %q, got %q", worktree, r.GitWorktree)
	}
	if r.InputTokens != 3000 {
		t.Errorf("row[0] input_tokens: expected 3000, got %d", r.InputTokens)
	}
	if r.OutputTokens != 1200 {
		t.Errorf("row[0] output_tokens: expected 1200, got %d", r.OutputTokens)
	}
	if r.TokensTotal != 4200 {
		t.Errorf("row[0] tokens_total: expected 4200, got %d", r.TokensTotal)
	}
	if r.TurnCount != 10 {
		t.Errorf("row[0] turn_count: expected 10, got %d", r.TurnCount)
	}
	if r.ToolCalls != 5 {
		t.Errorf("row[0] tool_calls: expected 5, got %d", r.ToolCalls)
	}
	if r.DurationMinutes != 1.0 {
		t.Errorf("row[0] duration_minutes: expected 1.0, got %f", r.DurationMinutes)
	}
	if r.StartMs != 1700000120000 {
		t.Errorf("row[0] start_ms: expected 1700000120000, got %d", r.StartMs)
	}
	if r.EndMs != 1700000180000 {
		t.Errorf("row[0] end_ms: expected 1700000180000, got %d", r.EndMs)
	}
}

func TestStore_SessionByID_Found(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000000000, 1000, 500)

	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	row, err := s.SessionByID(ctx, "11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("SessionByID: %v", err)
	}
	if row == nil {
		t.Fatal("SessionByID: expected non-nil row, got nil")
	}

	if row.SessionID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("session_id: expected %q, got %q", "11111111-1111-1111-1111-111111111111", row.SessionID)
	}
	if row.ModelHarness != string(defaults.HarnessClaudeCode) {
		t.Errorf("model_harness: expected %q, got %q", string(defaults.HarnessClaudeCode), row.ModelHarness)
	}
	if row.ModelID != "claude-opus-4-6" {
		t.Errorf("model_id: expected %q, got %q", "claude-opus-4-6", row.ModelID)
	}
	if row.HostSlug != "github.com-user-repo1" {
		t.Errorf("host_slug: expected %q, got %q", "github.com-user-repo1", row.HostSlug)
	}
	if row.ProjectHash != hash {
		t.Errorf("project_hash: expected %q, got %q", hash, row.ProjectHash)
	}
	// V23+: ProjectName = canonical_cwd = Project.FilePath from StoreEntry.
	if row.ProjectName != "/home/test/project" {
		t.Errorf("project_name: expected %q, got %q", "/home/test/project", row.ProjectName)
	}
	if row.StartMs != 1700000000000 {
		t.Errorf("start_ms: expected 1700000000000, got %d", row.StartMs)
	}
	if row.EndMs != 1700000060000 {
		t.Errorf("end_ms: expected 1700000060000, got %d", row.EndMs)
	}
	if row.InputTokens != 1000 {
		t.Errorf("input_tokens: expected 1000, got %d", row.InputTokens)
	}
	if row.OutputTokens != 500 {
		t.Errorf("output_tokens: expected 500, got %d", row.OutputTokens)
	}
	if row.TokensTotal != 1500 {
		t.Errorf("tokens_total: expected 1500, got %d", row.TokensTotal)
	}
	if row.TurnCount != 10 {
		t.Errorf("turn_count: expected 10, got %d", row.TurnCount)
	}
	if row.ToolCalls != 5 {
		t.Errorf("tool_calls: expected 5, got %d", row.ToolCalls)
	}
	if row.DurationMinutes != 1.0 {
		t.Errorf("duration_minutes: expected 1.0, got %f", row.DurationMinutes)
	}

	// makeStoreEntry sets Version = "2.1.14", which maps to sessions.tool_version.
	if row.ToolVersion == nil {
		t.Error("tool_version: expected non-nil, got nil")
	} else if *row.ToolVersion != "2.1.14" {
		t.Errorf("tool_version: expected %q, got %q", "2.1.14", *row.ToolVersion)
	}

	// makeStoreEntry does not set Git.Branch, so it should be nil.
	if row.GitBranch != nil {
		t.Errorf("git_branch: expected nil, got %q", *row.GitBranch)
	}
}

func TestStore_SessionByID_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	row, err := s.SessionByID(ctx, "nonexistent-id")
	if err != nil {
		t.Fatalf("SessionByID: unexpected error: %v", err)
	}
	if row != nil {
		t.Errorf("SessionByID: expected nil for nonexistent ID, got %+v", row)
	}
}

func TestStore_DashboardAggregates(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Day 1: 2024-01-15 (1705276800000 ms)
	day1Ms := int64(1705276800000)
	// Day 2: 2024-01-16 (1705363200000 ms)
	day2Ms := int64(1705363200000)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash, "github.com-test", defaults.HarnessClaudeCode, day1Ms, 1000, 500),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash, "github.com-test", defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash, "github.com-test", defaults.HarnessOpenCode, day2Ms, 3000, 1200),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	if err := s.UpdateDailySummary(ctx, []string{"2024-01-15", "2024-01-16"}); err != nil {
		t.Fatalf("UpdateDailySummary: %v", err)
	}

	dash, err := s.DashboardAggregates(ctx)
	if err != nil {
		t.Fatalf("DashboardAggregates: %v", err)
	}
	if dash == nil {
		t.Fatal("DashboardAggregates: expected non-nil, got nil")
	}

	// TotalSessions = 2 (day1) + 1 (day2) = 3
	if dash.TotalSessions != 3 {
		t.Errorf("TotalSessions: expected 3, got %d", dash.TotalSessions)
	}

	// TotalTokens = (1000+500) + (2000+800) + (3000+1200) = 8500
	if dash.TotalTokens != 8500 {
		t.Errorf("TotalTokens: expected 8500, got %d", dash.TotalTokens)
	}

	// ToolCallCount = 5 + 5 + 5 = 15
	if dash.ToolCallCount != 15 {
		t.Errorf("ToolCallCount: expected 15, got %d", dash.ToolCallCount)
	}

	// AvgDurationMs: weighted average of daily averages.
	// Day 1 avg_duration_ms = 60000 (2 sessions), Day 2 avg_duration_ms = 60000 (1 session)
	// Weighted = (60000*2 + 60000*1) / 3 = 60000
	if dash.AvgDurationMs != 60000.0 {
		t.Errorf("AvgDurationMs: expected 60000.0, got %f", dash.AvgDurationMs)
	}

	// AvgTurns: each session has 10 turns, so weighted avg = 10.0
	if dash.AvgTurns != 10.0 {
		t.Errorf("AvgTurns: expected 10.0, got %f", dash.AvgTurns)
	}
}

func TestStore_DashboardAggregates_Empty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	dash, err := s.DashboardAggregates(ctx)
	if err != nil {
		t.Fatalf("DashboardAggregates: unexpected error: %v", err)
	}
	if dash == nil {
		t.Fatal("DashboardAggregates: expected non-nil zeroed row, got nil")
	}

	if dash.TotalSessions != 0 {
		t.Errorf("TotalSessions: expected 0, got %d", dash.TotalSessions)
	}
	if dash.TotalTokens != 0 {
		t.Errorf("TotalTokens: expected 0, got %d", dash.TotalTokens)
	}
	if dash.AvgDurationMs != 0.0 {
		t.Errorf("AvgDurationMs: expected 0.0, got %f", dash.AvgDurationMs)
	}
	if dash.AvgTurns != 0.0 {
		t.Errorf("AvgTurns: expected 0.0, got %f", dash.AvgTurns)
	}
	if dash.ToolCallCount != 0 {
		t.Errorf("ToolCallCount: expected 0, got %d", dash.ToolCallCount)
	}
}

func TestStore_HarnessCounts(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dayMs := int64(1705276800000) // 2024-01-15

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash, "github.com-test", defaults.HarnessClaudeCode, dayMs, 1000, 500),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash, "github.com-test", defaults.HarnessClaudeCode, dayMs+1000, 2000, 800),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash, "github.com-test", defaults.HarnessOpenCode, dayMs+2000, 3000, 1200),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	if err := s.UpdateDailySummary(ctx, []string{"2024-01-15"}); err != nil {
		t.Fatalf("UpdateDailySummary: %v", err)
	}

	counts, err := s.HarnessCounts(ctx)
	if err != nil {
		t.Fatalf("HarnessCounts: %v", err)
	}

	if len(counts) != 2 {
		t.Fatalf("HarnessCounts: expected 2 entries, got %d: %v", len(counts), counts)
	}
	if counts[string(defaults.HarnessClaudeCode)] != 2 {
		t.Errorf("claude count: expected 2, got %d", counts[string(defaults.HarnessClaudeCode)])
	}
	if counts[string(defaults.HarnessOpenCode)] != 1 {
		t.Errorf("opencode count: expected 1, got %d", counts[string(defaults.HarnessOpenCode)])
	}
}

func TestStore_DailySummaries(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	// Day 1: 2024-01-15
	day1Ms := int64(1705276800000)
	// Day 2: 2024-01-16
	day2Ms := int64(1705363200000)

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash, "github.com-test", defaults.HarnessClaudeCode, day1Ms, 1000, 500),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash, "github.com-test", defaults.HarnessClaudeCode, day1Ms+1000, 2000, 800),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash, "github.com-test", defaults.HarnessOpenCode, day2Ms, 3000, 1200),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}
	if err := s.UpdateDailySummary(ctx, []string{"2024-01-15", "2024-01-16"}); err != nil {
		t.Fatalf("UpdateDailySummary: %v", err)
	}

	summaries, err := s.DailySummaries(ctx)
	if err != nil {
		t.Fatalf("DailySummaries: %v", err)
	}

	if len(summaries) != 2 {
		t.Fatalf("DailySummaries: expected 2 rows, got %d", len(summaries))
	}

	// Ordered by date_utc DESC: 2024-01-16 first.
	if summaries[0].DateUTC != "2024-01-16" {
		t.Errorf("row[0] date_utc: expected %q, got %q", "2024-01-16", summaries[0].DateUTC)
	}
	if summaries[1].DateUTC != "2024-01-15" {
		t.Errorf("row[1] date_utc: expected %q, got %q", "2024-01-15", summaries[1].DateUTC)
	}

	// Day 2 (row 0): 1 session, tokens 3000+1200 = 4200.
	if summaries[0].SessionCount != 1 {
		t.Errorf("row[0] session_count: expected 1, got %d", summaries[0].SessionCount)
	}
	if summaries[0].TokensIn != 3000 {
		t.Errorf("row[0] tokens_in: expected 3000, got %d", summaries[0].TokensIn)
	}
	if summaries[0].TokensOut != 1200 {
		t.Errorf("row[0] tokens_out: expected 1200, got %d", summaries[0].TokensOut)
	}
	if summaries[0].TokensTotal != 4200 {
		t.Errorf("row[0] tokens_total: expected 4200, got %d", summaries[0].TokensTotal)
	}

	// Day 1 (row 1): 2 sessions, tokens (1000+500) + (2000+800) = 4300.
	if summaries[1].SessionCount != 2 {
		t.Errorf("row[1] session_count: expected 2, got %d", summaries[1].SessionCount)
	}
	if summaries[1].TokensTotal != 4300 {
		t.Errorf("row[1] tokens_total: expected 4300, got %d", summaries[1].TokensTotal)
	}
	if summaries[1].AvgTurns != 10.0 {
		t.Errorf("row[1] avg_turns: expected 10.0, got %f", summaries[1].AvgTurns)
	}
	if summaries[1].AvgDurationMs != 60000.0 {
		t.Errorf("row[1] avg_duration_ms: expected 60000.0, got %f", summaries[1].AvgDurationMs)
	}
	if summaries[1].ToolCallCount != 10 {
		t.Errorf("row[1] tool_call_count: expected 10 (5+5), got %d", summaries[1].ToolCallCount)
	}
}

func TestStore_InsertSessions_ParentBeforeChild(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

	// Build parent entry first using the standard helper (ParentUUID == nil).
	parentEntry := makeStoreEntry(t, "aaaabbbb-0000-0000-0000-000000000001", hash, "github.com-fk-test", defaults.HarnessClaudeCode, 1700000000000, 500, 250)

	// Build child entry: same helper, then set ParentUUID to point at parent.
	childEntry := makeStoreEntry(t, "aaaabbbb-0000-0000-0000-000000000002", hash, "github.com-fk-test", defaults.HarnessClaudeCode, 1700000060000, 100, 50)
	parentSID := parentEntry.Metadata.SessionID
	childEntry.Metadata.ParentUUID = &parentSID

	// Deliberately place child BEFORE parent in the slice to reproduce the bug.
	// Without the topological sort in InsertSessions this would produce:
	//   sqlite: step: constraint failed: FOREIGN KEY constraint failed
	entries := []ingest.StoreEntry{childEntry, parentEntry}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions with child-before-parent: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Both sessions must be present.
	sessCount := queryInt(t, conn, `SELECT COUNT(*) FROM sessions`)
	if sessCount != 2 {
		t.Errorf("sessions: expected 2 rows, got %d", sessCount)
	}

	// The child must have its parent_id set correctly.
	parentIDVal := queryText(t, conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, "aaaabbbb-0000-0000-0000-000000000002")
	if parentIDVal != "aaaabbbb-0000-0000-0000-000000000001" {
		t.Errorf("child parent_id: expected %q, got %q", "aaaabbbb-0000-0000-0000-000000000001", parentIDVal)
	}

	// The parent must have a NULL parent_id.
	parentParentID := queryText(t, conn, `SELECT COALESCE(parent_id, 'NULL') FROM sessions WHERE session_id = ?`, "aaaabbbb-0000-0000-0000-000000000001")
	if parentParentID != "NULL" {
		t.Errorf("parent parent_id: expected NULL, got %q", parentParentID)
	}
}

func TestStore_FilteredSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	hash2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash1, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000000000, 1000, 500),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash1, "github.com-user-repo1", defaults.HarnessClaudeCode, 1700000060000, 2000, 800),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash2, "github.com-user-repo2", defaults.HarnessOpenCode, 1700000120000, 3000, 1200),
	}

	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// Filter by model_harness = "claude-code".
	claudeHarness := string(defaults.HarnessClaudeCode)
	rows, err := s.FilteredSessions(ctx, store.SessionFilter{
		ModelHarness: &claudeHarness,
	})
	if err != nil {
		t.Fatalf("FilteredSessions(claude): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("FilteredSessions(claude): expected 2, got %d", len(rows))
	}
	for _, r := range rows {
		if r.ModelHarness != string(defaults.HarnessClaudeCode) {
			t.Errorf("expected model_harness=claude-code, got %q", r.ModelHarness)
		}
	}

	// Filter by model_harness = "opencode".
	ocHarness := string(defaults.HarnessOpenCode)
	rows, err = s.FilteredSessions(ctx, store.SessionFilter{
		ModelHarness: &ocHarness,
	})
	if err != nil {
		t.Fatalf("FilteredSessions(opencode): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("FilteredSessions(opencode): expected 1, got %d", len(rows))
	}
	if rows[0].SessionID != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("expected session 33..., got %s", rows[0].SessionID)
	}

	// Filter by project_hash.
	rows, err = s.FilteredSessions(ctx, store.SessionFilter{
		ProjectHash: &hash2,
	})
	if err != nil {
		t.Fatalf("FilteredSessions(project): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("FilteredSessions(project): expected 1, got %d", len(rows))
	}
	if rows[0].ProjectHash != hash2 {
		t.Errorf("expected project_hash=%q, got %q", hash2, rows[0].ProjectHash)
	}

	// Filter by host_slug.
	slug := "github.com-user-repo1"
	rows, err = s.FilteredSessions(ctx, store.SessionFilter{
		HostSlug: &slug,
	})
	if err != nil {
		t.Fatalf("FilteredSessions(host): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("FilteredSessions(host): expected 2, got %d", len(rows))
	}

	// Filter by StartFrom (inclusive lower bound).
	fromMs := int64(1700000060000)
	rows, err = s.FilteredSessions(ctx, store.SessionFilter{
		StartFrom: &fromMs,
	})
	if err != nil {
		t.Fatalf("FilteredSessions(start_from): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("FilteredSessions(start_from): expected 2, got %d", len(rows))
	}

	// Filter by StartBefore.
	beforeMs := int64(1700000070000)
	rows, err = s.FilteredSessions(ctx, store.SessionFilter{
		StartBefore: &beforeMs,
	})
	if err != nil {
		t.Fatalf("FilteredSessions(start_before): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("FilteredSessions(start_before): expected 2, got %d", len(rows))
	}

	// Combined filter: claude + repo1 host.
	rows, err = s.FilteredSessions(ctx, store.SessionFilter{
		ModelHarness: &claudeHarness,
		HostSlug:     &slug,
	})
	if err != nil {
		t.Fatalf("FilteredSessions(combined): %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("FilteredSessions(combined): expected 2, got %d", len(rows))
	}

	// Empty filter returns all sessions.
	rows, err = s.FilteredSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatalf("FilteredSessions(empty): %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("FilteredSessions(empty): expected 3, got %d", len(rows))
	}
}

func TestStore_MigrationV2V3_RoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify session_entries table exists.
	seExists := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='session_entries'`)
	if seExists != 1 {
		t.Error("session_entries table not found after migration v2")
	}

	// Verify session_entries indexes exist (5 composite indexes: role, type, error, depth, parent).
	seIdxCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name LIKE 'idx_session_entries_%'`)
	if seIdxCount != 5 {
		t.Errorf("expected 5 session_entries indexes, got %d", seIdxCount)
	}

	// Verify session_metrics has v2 columns (compute_version, outcome, signal_density, etc.).
	// Insert a row with v2 fields to confirm they exist.
	// V23+: projects uses (project_hash, canonical_cwd, canonical_remote); host_slugs uses
	// (opaque_id, host_slug, git_remote, canonical_remote); sessions uses opaque_host_id FK.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO projects (project_hash, canonical_cwd) VALUES ('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '/test')`, nil)
	if err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const testV2OpaqueID = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+testV2OpaqueID+`', 'test-slug', 'git@test')`, nil)
	if err != nil {
		t.Fatalf("insert host_slug: %v", err)
	}
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO sessions (session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format) VALUES ('test-sess-v2', 'claude-code', 'claude-opus-4-6', '`+testV2OpaqueID+`', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 1000, 2000, 3000, '/test.jsonl', 'jsonl')`, nil)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	// Use v2 columns: outcome CHECK only allows 'resolved','partial','failed'.
	// signal_density and compute_version are v2 columns.
	// input_tokens, output_tokens, tool_calls, duration_minutes are the renamed v1→v2 columns.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_metrics (session_id, turn_count, tool_calls, duration_minutes, input_tokens, output_tokens, outcome, signal_density, compute_version) VALUES ('test-sess-v2', 10, 5, 1.0, 1000, 500, 'resolved', 0.85, 1)`, nil)
	if err != nil {
		t.Fatalf("insert session_metrics with v2 columns: %v", err)
	}

	// Read back v2 columns.
	cv := queryInt(t, conn, `SELECT compute_version FROM session_metrics WHERE session_id = 'test-sess-v2'`)
	if cv != 1 {
		t.Errorf("compute_version: expected 1, got %d", cv)
	}
	outcome := queryText(t, conn, `SELECT outcome FROM session_metrics WHERE session_id = 'test-sess-v2'`)
	if outcome != string(ingest.OutcomeResolved) {
		t.Errorf("outcome: expected %q, got %q", string(ingest.OutcomeResolved), outcome)
	}
	// signal_density is a v2 REAL column.
	density := queryFloat(t, conn, `SELECT signal_density FROM session_metrics WHERE session_id = 'test-sess-v2'`)
	if density != 0.85 {
		t.Errorf("signal_density: expected 0.85, got %f", density)
	}

	// Verify the outcome CHECK constraint rejects invalid values.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_metrics (session_id, outcome) VALUES ('fake-session', 'invalid')`, nil)
	if err == nil {
		t.Error("expected CHECK constraint to reject invalid outcome, but insert succeeded")
	}

	// Verify ingest_log table exists (migration v3).
	ilExists := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='ingest_log'`)
	if ilExists != 1 {
		t.Error("ingest_log table not found after migration v3")
	}

	// Verify ingest_log can accept inserts with the v3 schema columns.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO ingest_log (started_at, sessions_new, sessions_updated, sessions_unchanged, sessions_error) VALUES (1000, 5, 2, 3, 0)`, nil)
	if err != nil {
		t.Fatalf("insert ingest_log: %v", err)
	}
	ilCount := queryInt(t, conn, `SELECT COUNT(*) FROM ingest_log`)
	if ilCount != 1 {
		t.Errorf("ingest_log: expected 1 row, got %d", ilCount)
	}

	// Verify user_version is at least V3 (this test specifically validates the
	// V2→V3 roundtrip; openTestStore runs all migrations, so the schema is actually
	// at a higher version). Per-migration tests use a >= floor and are NOT bumped per
	// new migration — only TestStore_Migrations_ApplyV1 asserts the exact user_version.
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 3 {
		t.Errorf("user_version: expected >= 3 (at least v2→v3 applied), got %d", uv)
	}
}

// ---------------------------------------------------------------------------
// S3: Session Entries + Metrics round-trip tests
// ---------------------------------------------------------------------------

// seedSession inserts a minimal session row so that FK constraints on
// session_entries and session_metrics are satisfied.
func seedSession(t *testing.T, s *store.Store, sessionID string) {
	t.Helper()
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, sessionID, hash, "github.com-test", defaults.HarnessClaudeCode, 1700000000000, 100, 50)
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seedSession: %v", err)
	}
}

func intPtr(v int) *int             { return &v }
func int64Ptr(v int64) *int64       { return &v }
func strPtr(v string) *string       { return &v }
func float64Ptr(v float64) *float64 { return &v }

func TestStore_IndexSessionEntries_RoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "11111111-1111-1111-1111-111111111111"
	seedSession(t, s, sid)

	sessionID := ingest.SessionID(sid)
	entries := []schema.SessionEntry{
		{
			SessionID:      sessionID,
			EntryIndex:     0,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeText,
			Role:           ingest.RoleUser,
			TimestampMs:    int64Ptr(1700000000000),
			ContentPreview: strPtr("Hello, can you help?"),
			TokensIn:       intPtr(100),
			HasToolUse:     false,
			HasThinking:    false,
			IsError:        false,
			RawByteLength:  intPtr(512),
		},
		{
			SessionID:      sessionID,
			EntryIndex:     1,
			Harness:        defaults.HarnessClaudeCode,
			EntryType:      ingest.EntryTypeToolUse,
			Role:           ingest.RoleAssistant,
			TimestampMs:    int64Ptr(1700000001000),
			ContentPreview: strPtr("Running Read tool..."),
			TokensOut:      intPtr(200),
			HasToolUse:     true,
			ToolNamesCSV:   strPtr("Read,Grep"),
			HasThinking:    true,
			IsError:        false,
			RawByteLength:  intPtr(1024),
			ToolCallID:     strPtr("tool-123"),
			EntryID:        strPtr("entry-abc"),
		},
		{
			SessionID:     sessionID,
			EntryIndex:    2,
			Harness:       defaults.HarnessClaudeCode,
			EntryType:     ingest.EntryTypeToolResult,
			Role:          ingest.RoleTool,
			IsError:       true,
			RawByteLength: intPtr(256),
		},
	}

	if err := s.IndexSessionEntries(ctx, sessionID, entries); err != nil {
		t.Fatalf("IndexSessionEntries: %v", err)
	}

	// Verify existence.
	exists, err := s.SessionEntriesExist(ctx, sessionID)
	if err != nil {
		t.Fatalf("SessionEntriesExist: %v", err)
	}
	if !exists {
		t.Error("SessionEntriesExist: expected true, got false")
	}

	// Read back via ListEntries.
	got, err := s.ListEntries(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("ListEntries: expected 3 entries, got %d", len(got))
	}

	// Verify entry 0.
	e0 := got[0]
	if e0.EntryIndex != 0 {
		t.Errorf("entry[0] index: expected 0, got %d", e0.EntryIndex)
	}
	if e0.Harness != defaults.HarnessClaudeCode {
		t.Errorf("entry[0] provider: expected %s, got %s", defaults.HarnessClaudeCode, e0.Harness)
	}
	if e0.EntryType != ingest.EntryTypeText {
		t.Errorf("entry[0] entry_type: expected %s, got %s", ingest.EntryTypeText, e0.EntryType)
	}
	if e0.Role != ingest.RoleUser {
		t.Errorf("entry[0] role: expected %s, got %s", ingest.RoleUser, e0.Role)
	}
	if e0.TimestampMs == nil || *e0.TimestampMs != 1700000000000 {
		t.Errorf("entry[0] timestamp_ms: expected 1700000000000, got %v", e0.TimestampMs)
	}
	if e0.ContentPreview == nil || *e0.ContentPreview != "Hello, can you help?" {
		t.Errorf("entry[0] content_preview: expected %q, got %v", "Hello, can you help?", e0.ContentPreview)
	}
	if e0.TokensIn == nil || *e0.TokensIn != 100 {
		t.Errorf("entry[0] tokens_in: expected 100, got %v", e0.TokensIn)
	}
	// tokens_out should be nil for entry[0].
	if e0.TokensOut != nil {
		t.Errorf("entry[0] tokens_out: expected nil, got %v", e0.TokensOut)
	}

	// Verify entry 1 (tool use).
	e1 := got[1]
	if !e1.HasToolUse {
		t.Error("entry[1] has_tool_use: expected true")
	}
	if e1.ToolNamesCSV == nil || *e1.ToolNamesCSV != "Read,Grep" {
		t.Errorf("entry[1] tool_names_csv: expected %q, got %v", "Read,Grep", e1.ToolNamesCSV)
	}
	if !e1.HasThinking {
		t.Error("entry[1] has_thinking: expected true")
	}
	if e1.ToolCallID == nil || *e1.ToolCallID != "tool-123" {
		t.Errorf("entry[1] tool_call_id: expected %q, got %v", "tool-123", e1.ToolCallID)
	}
	if e1.EntryID == nil || *e1.EntryID != "entry-abc" {
		t.Errorf("entry[1] entry_id: expected %q, got %v", "entry-abc", e1.EntryID)
	}

	// Verify entry 2 (error).
	e2 := got[2]
	if !e2.IsError {
		t.Error("entry[2] is_error: expected true")
	}
	if e2.Role != ingest.RoleTool {
		t.Errorf("entry[2] role: expected %s, got %s", ingest.RoleTool, e2.Role)
	}
}

func TestStore_IndexSessionEntries_Idempotent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "22222222-2222-2222-2222-222222222222"
	seedSession(t, s, sid)

	sessionID := ingest.SessionID(sid)

	// First index: 2 entries.
	first := []schema.SessionEntry{
		{SessionID: sessionID, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleUser},
		{SessionID: sessionID, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText, Role: ingest.RoleAssistant},
	}
	if err := s.IndexSessionEntries(ctx, sessionID, first); err != nil {
		t.Fatalf("first IndexSessionEntries: %v", err)
	}

	// Second index: 1 entry (replaces the 2).
	second := []schema.SessionEntry{
		{SessionID: sessionID, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse, Role: ingest.RoleAssistant, HasToolUse: true},
	}
	if err := s.IndexSessionEntries(ctx, sessionID, second); err != nil {
		t.Fatalf("second IndexSessionEntries: %v", err)
	}

	got, err := s.ListEntries(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListEntries: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListEntries: expected 1 entry after re-index, got %d", len(got))
	}
	if got[0].EntryType != ingest.EntryTypeToolUse {
		t.Errorf("entry[0] type: expected %s, got %s", ingest.EntryTypeToolUse, got[0].EntryType)
	}
}

func TestStore_SessionEntriesExist_NotIndexed(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	exists, err := s.SessionEntriesExist(ctx, ingest.SessionID("nonexistent-id"))
	if err != nil {
		t.Fatalf("SessionEntriesExist: %v", err)
	}
	if exists {
		t.Error("SessionEntriesExist: expected false for nonexistent session")
	}
}

func TestStore_SaveMetrics_RoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "33333333-3333-3333-3333-333333333333"
	seedSession(t, s, sid)

	sessionID := ingest.SessionID(sid)
	now := int64(1700000099000)
	outcomeVal := ingest.OutcomeResolved

	metrics := &ingest.SessionMetrics{
		SessionID: sessionID,
		QualityMetrics: schema.QualityMetrics{
			TitleGenerated:   strPtr("Fix authentication bug"),
			Outcome:          &outcomeVal,
			TotalTokens:      intPtr(5000),
			InputTokens:      intPtr(2000),
			OutputTokens:     intPtr(3000),
			ToolCalls:        intPtr(8),
			TurnCount:        intPtr(15),
			SubagentCount:    intPtr(2),
			FilesTouched:     intPtr(3),
			LinesChanged:     intPtr(42),
			DurationMinutes:  float64Ptr(5.5),
			RetryLoops:       intPtr(1),
			SignalDensity:    float64Ptr(0.85),
			SpecQualityScore: float64Ptr(0.72),
			ComputedAt:       &now,
			ComputeVersion:   intPtr(1),
		},
	}

	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	// Read back.
	got, err := s.GetMetrics(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if got == nil {
		t.Fatal("GetMetrics: expected non-nil, got nil")
	}

	if got.SessionID != sessionID {
		t.Errorf("session_id: expected %s, got %s", sessionID, got.SessionID)
	}
	if got.TurnCount == nil || *got.TurnCount != 15 {
		t.Errorf("turn_count: expected 15, got %v", got.TurnCount)
	}
	if got.SubagentCount == nil || *got.SubagentCount != 2 {
		t.Errorf("subagent_count: expected 2, got %v", got.SubagentCount)
	}
	if got.TitleGenerated == nil || *got.TitleGenerated != "Fix authentication bug" {
		t.Errorf("title: expected %q, got %v", "Fix authentication bug", got.TitleGenerated)
	}
	quality, err := s.GetQualityMetrics(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetQualityMetrics: %v", err)
	}
	if quality == nil || quality.TitleGenerated == nil || *quality.TitleGenerated != "Fix authentication bug" {
		t.Fatalf("published quality title: expected persisted title, got %#v", quality)
	}
	if got.Outcome == nil || *got.Outcome != ingest.OutcomeResolved {
		t.Errorf("outcome: expected %q, got %v", ingest.OutcomeResolved, got.Outcome)
	}
	if got.TotalTokens == nil || *got.TotalTokens != 5000 {
		t.Errorf("total_tokens: expected 5000, got %v", got.TotalTokens)
	}
	if got.InputTokens == nil || *got.InputTokens != 2000 {
		t.Errorf("input_tokens: expected 2000, got %v", got.InputTokens)
	}
	if got.OutputTokens == nil || *got.OutputTokens != 3000 {
		t.Errorf("output_tokens: expected 3000, got %v", got.OutputTokens)
	}
	if got.ToolCalls == nil || *got.ToolCalls != 8 {
		t.Errorf("tool_calls: expected 8, got %v", got.ToolCalls)
	}
	if got.DurationMinutes == nil || *got.DurationMinutes != 5.5 {
		t.Errorf("duration_minutes: expected 5.5, got %v", got.DurationMinutes)
	}
	if got.SignalDensity == nil || *got.SignalDensity != 0.85 {
		t.Errorf("signal_density: expected 0.85, got %v", got.SignalDensity)
	}
	if got.SpecQualityScore == nil || *got.SpecQualityScore != 0.72 {
		t.Errorf("spec_quality_score: expected 0.72, got %v", got.SpecQualityScore)
	}
	if got.ComputeVersion == nil || *got.ComputeVersion != 1 {
		t.Errorf("compute_version: expected 1, got %v", got.ComputeVersion)
	}
	if got.ComputedAt == nil || *got.ComputedAt != now {
		t.Errorf("computed_at: expected %d, got %v", now, got.ComputedAt)
	}

	// NULL fields should be nil.
	if got.RetryTokensWasted != nil {
		t.Errorf("retry_tokens_wasted: expected nil, got %v", got.RetryTokensWasted)
	}
	if got.WithinSessionReverts != nil {
		t.Errorf("within_session_reverts: expected nil, got %v", got.WithinSessionReverts)
	}
	if got.ExplorationRatio != nil {
		t.Errorf("exploration_ratio: expected nil, got %v", got.ExplorationRatio)
	}
}

func TestStore_SaveMetrics_NullPreservation(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "44444444-4444-4444-4444-444444444444"
	seedSession(t, s, sid)

	sessionID := ingest.SessionID(sid)

	// Save metrics with most fields nil (only required fields set).
	metrics := &ingest.SessionMetrics{
		SessionID:      sessionID,
		QualityMetrics: schema.QualityMetrics{ComputeVersion: intPtr(1)},
	}

	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	got, err := s.GetMetrics(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if got == nil {
		t.Fatal("GetMetrics: expected non-nil")
	}

	// All optional fields should be nil.
	if got.TurnCount != nil {
		t.Errorf("turn_count: expected nil, got %v", got.TurnCount)
	}
	if got.TitleGenerated != nil {
		t.Errorf("title: expected nil, got %v", got.TitleGenerated)
	}
	if got.Outcome != nil {
		t.Errorf("outcome: expected nil, got %v", got.Outcome)
	}
	if got.TotalTokens != nil {
		t.Errorf("total_tokens: expected nil, got %v", got.TotalTokens)
	}
	if got.DurationMinutes != nil {
		t.Errorf("duration_minutes: expected nil, got %v", got.DurationMinutes)
	}
}

func TestStore_GetMetrics_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	got, err := s.GetMetrics(ctx, ingest.SessionID("nonexistent-id"))
	if err != nil {
		t.Fatalf("GetMetrics: unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("GetMetrics: expected nil for nonexistent session, got %+v", got)
	}
}

// TestStore_SaveMetrics_NullM7BoolColumns verifies that M7 bool columns written
// as nil survive the SaveMetrics → GetMetrics round-trip with consistent values.
//
// Note: m7_spec_has_examples and m7_spec_has_constraints are defined as
// "INTEGER NOT NULL DEFAULT 0" in the schema (migrationV2). SQLite silently
// coerces nil → 0 on INSERT for NOT NULL columns with a DEFAULT, so these
// columns cannot store SQL NULL. The scanner therefore always returns &false
// (not nil) when the stored value is 0 (the DEFAULT).
//
// The test asserts that:
//   - SaveMetrics with M7SpecHasExamples=nil succeeds (no error)
//   - GetMetrics round-trips the value consistently (false, not panicking)
//   - A non-nil M7SpecHasExamples=true value round-trips correctly
//
// If the schema is later changed to allow NULL (e.g., by dropping NOT NULL DEFAULT 0),
// update the assertions below to expect nil instead of &false.
func TestStore_SaveMetrics_NullM7BoolColumns(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "66666666-6666-6666-6666-666666666666"
	seedSession(t, s, sid)

	sessionID := ingest.SessionID(sid)

	// Save metrics with M7SpecHasExamples nil (not explicitly set).
	metrics := &ingest.SessionMetrics{
		SessionID: sessionID,
		QualityMetrics: schema.QualityMetrics{
			ComputeVersion:    intPtr(1),
			TurnCount:         intPtr(5),
			M7SpecHasExamples: nil, // not set — written as DEFAULT 0 due to NOT NULL constraint
		},
	}

	if err := s.SaveMetrics(ctx, metrics); err != nil {
		t.Fatalf("SaveMetrics with nil M7SpecHasExamples: %v", err)
	}

	got, err := s.GetMetrics(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetMetrics: %v", err)
	}
	if got == nil {
		t.Fatal("GetMetrics: expected non-nil, got nil")
	}

	// Due to NOT NULL DEFAULT 0 schema constraint, nil is coerced to 0 on write.
	// The scanner reads 0 as &false (not nil). This is the current schema-enforced behavior.
	// Desired future behavior (after schema fix): got.M7SpecHasExamples == nil.
	if got.M7SpecHasExamples == nil {
		t.Logf("M7SpecHasExamples: schema now supports NULL (schema was fixed) — update test expectations")
	} else if *got.M7SpecHasExamples != false {
		t.Errorf("M7SpecHasExamples: expected false (DEFAULT 0), got %v", *got.M7SpecHasExamples)
	}

	// Sanity: non-nil fields should still be correct.
	if got.TurnCount == nil || *got.TurnCount != 5 {
		t.Errorf("TurnCount: expected 5, got %v", got.TurnCount)
	}

	// Verify explicit true value round-trips correctly.
	trueVal := true
	metrics2 := &ingest.SessionMetrics{
		SessionID: sessionID,
		QualityMetrics: schema.QualityMetrics{
			ComputeVersion:    intPtr(1),
			M7SpecHasExamples: &trueVal,
		},
	}
	if err := s.SaveMetrics(ctx, metrics2); err != nil {
		t.Fatalf("SaveMetrics with M7SpecHasExamples=true: %v", err)
	}
	got2, err := s.GetMetrics(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetMetrics (round 2): %v", err)
	}
	if got2 == nil {
		t.Fatal("GetMetrics (round 2): expected non-nil, got nil")
	}
	if got2.M7SpecHasExamples == nil || !*got2.M7SpecHasExamples {
		t.Errorf("M7SpecHasExamples=true: expected non-nil &true after round-trip, got %v", got2.M7SpecHasExamples)
	}
}

func TestStore_MetricsExist_VersionCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()
	sid := "55555555-5555-5555-5555-555555555555"
	seedSession(t, s, sid)

	sessionID := ingest.SessionID(sid)

	// Save with compute_version=1.
	if err := s.SaveMetrics(ctx, &ingest.SessionMetrics{
		SessionID:      sessionID,
		QualityMetrics: schema.QualityMetrics{ComputeVersion: intPtr(1)},
	}); err != nil {
		t.Fatalf("SaveMetrics: %v", err)
	}

	// Version 1 should exist.
	exists, err := s.MetricsExist(ctx, sessionID, 1)
	if err != nil {
		t.Fatalf("MetricsExist(1): %v", err)
	}
	if !exists {
		t.Error("MetricsExist(1): expected true")
	}

	// Version 2 should NOT exist.
	exists, err = s.MetricsExist(ctx, sessionID, 2)
	if err != nil {
		t.Fatalf("MetricsExist(2): %v", err)
	}
	if exists {
		t.Error("MetricsExist(2): expected false")
	}

	// Nonexistent session.
	exists, err = s.MetricsExist(ctx, ingest.SessionID("nonexistent-id"), 1)
	if err != nil {
		t.Fatalf("MetricsExist(nonexistent): %v", err)
	}
	if exists {
		t.Error("MetricsExist(nonexistent): expected false")
	}
}

func TestStore_ListEntries_Empty(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entries, err := s.ListEntries(ctx, ingest.SessionID("nonexistent-id"))
	if err != nil {
		t.Fatalf("ListEntries: unexpected error: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ListEntries: expected 0 entries, got %d", len(entries))
	}
}

// ---------------------------------------------------------------------------
// IngestLog tests
// ---------------------------------------------------------------------------

func TestStore_LogIngestRun(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	startedAt := int64(1705276800000) // 2024-01-15T00:00:00Z
	finishedAt := int64(1705276860000)
	entry := ingest.IngestLogEntry{
		StartedAt:         startedAt,
		FinishedAt:        &finishedAt,
		SessionsNew:       5,
		SessionsUpdated:   2,
		SessionsUnchanged: 10,
		SessionsError:     1,
		IndexedCount:      7,
		ComputedCount:     7,
	}

	if err := s.LogIngestRun(ctx, entry); err != nil {
		t.Fatalf("LogIngestRun: %v", err)
	}

	// Verify the row was inserted.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	count := queryInt(t, conn, `SELECT COUNT(*) FROM ingest_log`)
	if count != 1 {
		t.Errorf("ingest_log count: expected 1, got %d", count)
	}

	newCount := queryInt(t, conn, `SELECT sessions_new FROM ingest_log WHERE id = 1`)
	if newCount != 5 {
		t.Errorf("sessions_new: expected 5, got %d", newCount)
	}

	indexedCount := queryInt(t, conn, `SELECT indexed_count FROM ingest_log WHERE id = 1`)
	if indexedCount != 7 {
		t.Errorf("indexed_count: expected 9, got %d", indexedCount)
	}
}

func TestStore_LogIngestRun_NilFinishedAt(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	entry := ingest.IngestLogEntry{
		StartedAt:   1705276800000,
		FinishedAt:  nil, // simulates pipeline crash
		SessionsNew: 0,
	}

	if err := s.LogIngestRun(ctx, entry); err != nil {
		t.Fatalf("LogIngestRun: %v", err)
	}

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	count := queryInt(t, conn, `SELECT COUNT(*) FROM ingest_log WHERE finished_at IS NULL`)
	if count != 1 {
		t.Errorf("ingest_log with NULL finished_at: expected 1, got %d", count)
	}
}

// ---------------------------------------------------------------------------
// IndexLogWriter tests
// ---------------------------------------------------------------------------

func TestStore_LogIndexEntry_RoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sid := "11111111-1111-1111-1111-111111111111"
	seedSession(t, s, sid)

	sessionID, err := ingest.NewSessionID(sid)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	srcPath := "/home/test/.claude/projects/abc/session.jsonl"
	origRoot := "/home/test/.claude/projects"
	finishedAt := int64(1705276860000)

	// Entry 1: fully populated.
	entry1 := ingest.IndexLogEntry{
		SessionID:    sessionID,
		Harness:      defaults.HarnessClaudeCode,
		Outcome:      ingest.IndexOutcomeIndexed,
		IndexVersion: 2,
		EntriesCount: 15,
		SourcePath:   &srcPath,
		OriginalRoot: &origRoot,
		Reason:       nil,
		StartedAt:    1705276800000,
		FinishedAt:   &finishedAt,
		ErrorMessage: nil,
	}

	if err := s.LogIndexEntry(ctx, entry1); err != nil {
		t.Fatalf("LogIndexEntry(entry1): %v", err)
	}

	// Verify row 1 was inserted with correct values.
	func() {
		conn := takeConn(t, s.PoolForTest())
		defer s.PoolForTest().Put(conn)

		rowCount := queryInt(t, conn, `SELECT COUNT(*) FROM index_log`)
		if rowCount != 1 {
			t.Fatalf("index_log count: expected 1, got %d", rowCount)
		}

		gotSID := queryText(t, conn, `SELECT session_id FROM index_log WHERE id = 1`)
		if gotSID != sid {
			t.Errorf("session_id: expected %q, got %q", sid, gotSID)
		}
		gotProvider := queryText(t, conn, `SELECT provider FROM index_log WHERE id = 1`)
		if gotProvider != string(defaults.HarnessClaudeCode) {
			t.Errorf("provider: expected %q, got %q", string(defaults.HarnessClaudeCode), gotProvider)
		}
		gotOutcome := queryText(t, conn, `SELECT outcome FROM index_log WHERE id = 1`)
		if gotOutcome != string(ingest.IndexOutcomeIndexed) {
			t.Errorf("outcome: expected %q, got %q", string(ingest.IndexOutcomeIndexed), gotOutcome)
		}
		gotVersion := queryInt(t, conn, `SELECT index_version FROM index_log WHERE id = 1`)
		if gotVersion != 2 {
			t.Errorf("index_version: expected 2, got %d", gotVersion)
		}
		gotEntries := queryInt(t, conn, `SELECT entries_count FROM index_log WHERE id = 1`)
		if gotEntries != 15 {
			t.Errorf("entries_count: expected 15, got %d", gotEntries)
		}
		gotSrcPath := queryText(t, conn, `SELECT source_path FROM index_log WHERE id = 1`)
		if gotSrcPath != srcPath {
			t.Errorf("source_path: expected %q, got %q", srcPath, gotSrcPath)
		}
		gotOrigRoot := queryText(t, conn, `SELECT original_root FROM index_log WHERE id = 1`)
		if gotOrigRoot != origRoot {
			t.Errorf("original_root: expected %q, got %q", origRoot, gotOrigRoot)
		}
		gotStartedAt := queryInt(t, conn, `SELECT started_at FROM index_log WHERE id = 1`)
		if int64(gotStartedAt) != 1705276800000 {
			t.Errorf("started_at: expected 1705276800000, got %d", gotStartedAt)
		}
		gotFinishedAt := queryInt(t, conn, `SELECT finished_at FROM index_log WHERE id = 1`)
		if int64(gotFinishedAt) != finishedAt {
			t.Errorf("finished_at: expected %d, got %d", finishedAt, gotFinishedAt)
		}

		// Verify NULL fields (reason, error_message).
		reasonNull := queryInt(t, conn, `SELECT COUNT(*) FROM index_log WHERE id = 1 AND reason IS NULL`)
		if reasonNull != 1 {
			t.Error("reason should be NULL for entry1")
		}
		errMsgNull := queryInt(t, conn, `SELECT COUNT(*) FROM index_log WHERE id = 1 AND error_message IS NULL`)
		if errMsgNull != 1 {
			t.Error("error_message should be NULL for entry1")
		}
	}()

	// Entry 2: nil optional fields (SourcePath, OriginalRoot) but populated Reason/ErrorMessage.
	sid2 := "22222222-2222-2222-2222-222222222222"
	seedSession(t, s, sid2)
	sessionID2, err := ingest.NewSessionID(sid2)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	reason := "no transcript file"
	errMsg := "file not found"

	entry2 := ingest.IndexLogEntry{
		SessionID:    sessionID2,
		Harness:      defaults.HarnessOpenCode,
		Outcome:      ingest.IndexOutcomeError,
		IndexVersion: 1,
		EntriesCount: 0,
		SourcePath:   nil,
		OriginalRoot: nil,
		Reason:       &reason,
		StartedAt:    1705276900000,
		FinishedAt:   nil,
		ErrorMessage: &errMsg,
	}

	if err := s.LogIndexEntry(ctx, entry2); err != nil {
		t.Fatalf("LogIndexEntry(entry2): %v", err)
	}

	// Verify entry 2 with NULL handling.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	rowCount := queryInt(t, conn, `SELECT COUNT(*) FROM index_log`)
	if rowCount != 2 {
		t.Fatalf("index_log count after 2 inserts: expected 2, got %d", rowCount)
	}

	// Verify NULL handling on entry2.
	srcPathNull := queryInt(t, conn, `SELECT COUNT(*) FROM index_log WHERE id = 2 AND source_path IS NULL`)
	if srcPathNull != 1 {
		t.Error("source_path should be NULL for entry2")
	}
	origRootNull := queryInt(t, conn, `SELECT COUNT(*) FROM index_log WHERE id = 2 AND original_root IS NULL`)
	if origRootNull != 1 {
		t.Error("original_root should be NULL for entry2")
	}
	finishedAtNull := queryInt(t, conn, `SELECT COUNT(*) FROM index_log WHERE id = 2 AND finished_at IS NULL`)
	if finishedAtNull != 1 {
		t.Error("finished_at should be NULL for entry2")
	}
	gotReason := queryText(t, conn, `SELECT reason FROM index_log WHERE id = 2`)
	if gotReason != reason {
		t.Errorf("reason: expected %q, got %q", reason, gotReason)
	}
	gotErrMsg := queryText(t, conn, `SELECT error_message FROM index_log WHERE id = 2`)
	if gotErrMsg != errMsg {
		t.Errorf("error_message: expected %q, got %q", errMsg, gotErrMsg)
	}
}

// ---------------------------------------------------------------------------
// IndexStateWriter tests
// ---------------------------------------------------------------------------

func TestStore_UpdateIndexState_RoundTrip(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sid := "11111111-1111-1111-1111-111111111111"
	seedSession(t, s, sid)

	sessionID, err := ingest.NewSessionID(sid)
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	indexedAtMs := int64(1705276860000)
	if err := s.UpdateIndexState(ctx, sessionID, 2, indexedAtMs); err != nil {
		t.Fatalf("UpdateIndexState: %v", err)
	}

	// Verify the session row was updated.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	gotVersion := queryInt(t, conn, `SELECT index_version FROM sessions WHERE session_id = ?`, sid)
	if gotVersion != 2 {
		t.Errorf("index_version: expected 2, got %d", gotVersion)
	}

	gotIndexedAt := queryInt(t, conn, `SELECT indexed_at FROM sessions WHERE session_id = ?`, sid)
	if int64(gotIndexedAt) != indexedAtMs {
		t.Errorf("indexed_at: expected %d, got %d", indexedAtMs, gotIndexedAt)
	}
}

func TestStore_ListStaleIndexSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entries := []ingest.StoreEntry{
		makeStoreEntry(t, "11111111-1111-1111-1111-111111111111", hash, "github.com-test", defaults.HarnessClaudeCode, 1700000000000, 100, 50),
		makeStoreEntry(t, "22222222-2222-2222-2222-222222222222", hash, "github.com-test", defaults.HarnessClaudeCode, 1700000060000, 200, 100),
		makeStoreEntry(t, "33333333-3333-3333-3333-333333333333", hash, "github.com-test", defaults.HarnessClaudeCode, 1700000120000, 300, 150),
	}
	if err := s.InsertSessions(ctx, entries); err != nil {
		t.Fatalf("InsertSessions: %v", err)
	}

	// Session 1 → version=2, Session 2 → version=1, Session 3 → default (0).
	sid1, _ := ingest.NewSessionID("11111111-1111-1111-1111-111111111111")
	sid2, _ := ingest.NewSessionID("22222222-2222-2222-2222-222222222222")

	if err := s.UpdateIndexState(ctx, sid1, 2, 1705276800000); err != nil {
		t.Fatalf("UpdateIndexState(session1, v2): %v", err)
	}
	if err := s.UpdateIndexState(ctx, sid2, 1, 1705276800000); err != nil {
		t.Fatalf("UpdateIndexState(session2, v1): %v", err)
	}

	// ListStaleIndexSessions(currentVersion=2): sessions with version < 2.
	// Session 2 (v=1) and Session 3 (v=0) should be returned.
	stale2, err := s.ListStaleIndexSessions(ctx, 2)
	if err != nil {
		t.Fatalf("ListStaleIndexSessions(2): %v", err)
	}
	if len(stale2) != 2 {
		t.Fatalf("ListStaleIndexSessions(2): expected 2 stale sessions, got %d", len(stale2))
	}

	// Collect returned session IDs.
	staleMap2 := make(map[ingest.SessionID]bool, len(stale2))
	for _, sid := range stale2 {
		staleMap2[sid] = true
	}
	if !staleMap2[sid2] {
		t.Error("ListStaleIndexSessions(2): expected session 22... (v=1) to be stale")
	}
	sid3, _ := ingest.NewSessionID("33333333-3333-3333-3333-333333333333")
	if !staleMap2[sid3] {
		t.Error("ListStaleIndexSessions(2): expected session 33... (v=0) to be stale")
	}

	// ListStaleIndexSessions(currentVersion=1): sessions with version < 1.
	// Only Session 3 (v=0) should be returned.
	stale1, err := s.ListStaleIndexSessions(ctx, 1)
	if err != nil {
		t.Fatalf("ListStaleIndexSessions(1): %v", err)
	}
	if len(stale1) != 1 {
		t.Fatalf("ListStaleIndexSessions(1): expected 1 stale session, got %d", len(stale1))
	}
	if stale1[0] != sid3 {
		t.Errorf("ListStaleIndexSessions(1): expected session 33..., got %s", stale1[0])
	}
}
