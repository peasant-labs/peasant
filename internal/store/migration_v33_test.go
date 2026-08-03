package store

// V33 migration tests.
//
// This file is package `store` (internal), not `store_test`, because verifying
// the destructive harness rename requires executing the unexported `migrationV33`
// SQL directly against a hand-built pre-v33 table, and the consent-gate tests
// call the unexported `maybeRunV33Consent` / `newConsentPrompter`. Partial
// migration replay via sqlitemigration is unreliable here because V23's rename
// runs in Go (applyV23DataMigration in Open), not in the migration SQL — so we
// reconstruct the pre-v33 table shape (post-V23 columns, OLD harness CHECK) and
// run the V33 SQL on it. See debug_migration_test.go for the in-package pattern.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// preV33SessionsDDL is the sessions table as it existed after V23 and before V33:
// 20 columns (matching v23CreateSessionsV2) with the OLD harness CHECK set
// ('claude','opencode','codex','gemini'). FK REFERENCES are dropped — V33 runs
// with foreign keys disabled and the rename does not depend on FK targets.
const preV33SessionsDDL = `CREATE TABLE sessions (
    session_id     TEXT PRIMARY KEY,
    parent_id      TEXT,
    model_harness  TEXT NOT NULL CHECK (model_harness IN ('claude','opencode','codex','gemini')),
    model_id       TEXT NOT NULL,
    opaque_host_id TEXT NOT NULL,
    project_hash   TEXT NOT NULL,
    start_ms       INTEGER NOT NULL,
    end_ms         INTEGER NOT NULL,
    ingested_ms    INTEGER NOT NULL,
    source_path    TEXT NOT NULL,
    source_format  TEXT NOT NULL CHECK (source_format IN ('jsonl','json')),
    schema_version INTEGER NOT NULL DEFAULT 1,
    git_branch     TEXT,
    git_worktree   TEXT,
    git_tracking   TEXT,
    tool_version   TEXT,
    pushed_at      INTEGER,
    tags           TEXT,
    index_version  INTEGER NOT NULL DEFAULT 0,
    indexed_at     INTEGER
) STRICT`

// preV33DailySummaryDDL is daily_summary_harness before V33: same 9 columns as
// the post-V33 table but with the OLD harness CHECK set.
const preV33DailySummaryDDL = `CREATE TABLE daily_summary_harness (
    date_utc        TEXT    NOT NULL,
    model_harness   TEXT    NOT NULL CHECK (model_harness IN ('claude','opencode','codex','gemini')),
    session_count   INTEGER NOT NULL DEFAULT 0,
    tokens_in       INTEGER NOT NULL DEFAULT 0,
    tokens_out      INTEGER NOT NULL DEFAULT 0,
    tokens_total    INTEGER NOT NULL DEFAULT 0,
    avg_duration_ms REAL    NOT NULL DEFAULT 0,
    avg_turns       REAL    NOT NULL DEFAULT 0,
    tool_call_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (date_utc, model_harness)
) STRICT, WITHOUT ROWID`

func execT(t *testing.T, conn *sqlite.Conn, sql string) {
	t.Helper()
	if err := sqlitex.ExecuteTransient(conn, sql, nil); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func scalarText(t *testing.T, conn *sqlite.Conn, sql string) string {
	t.Helper()
	var got string
	if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { got = stmt.ColumnText(0); return nil },
	}); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return got
}

func scalarInt(t *testing.T, conn *sqlite.Conn, sql string) int {
	t.Helper()
	var got int
	if err := sqlitex.ExecuteTransient(conn, sql, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error { got = int(stmt.ColumnInt64(0)); return nil },
	}); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return got
}

// TestMigrationV33Applies verifies the final schema state on a freshly opened DB:
// user_version, recreated indexes, and the updated CHECK constraint.
func TestMigrationV33Applies(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	conn, err := s.Pool().Take(context.Background())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	defer s.Pool().Put(conn)

	if uv := scalarInt(t, conn, "PRAGMA user_version"); uv < migrationV33TargetVersion {
		t.Errorf("user_version: expected >= %d, got %d", migrationV33TargetVersion, uv)
	}

	// The sessions indexes must be recreated after the table rebuild.
	for _, idx := range []string{
		"idx_sessions_start", "idx_sessions_harness", "idx_sessions_project",
		"idx_sessions_host", "idx_sessions_parent",
	} {
		got := scalarInt(t, conn,
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='"+idx+"'")
		if got != 1 {
			t.Errorf("index %s: expected 1, got %d", idx, got)
		}
	}

	// daily_summary_harness CHECK uses the same harness set as sessions; assert
	// the updated constraint there (it has no FK/NOT-NULL columns to satisfy).
	rejectOld := sqlitex.ExecuteTransient(conn,
		`INSERT INTO daily_summary_harness (date_utc, model_harness) VALUES ('2026-01-01','claude')`, nil)
	if rejectOld == nil {
		t.Error("expected post-V33 CHECK to reject legacy 'claude', but insert succeeded")
	}
	rejectOldGemini := sqlitex.ExecuteTransient(conn,
		`INSERT INTO daily_summary_harness (date_utc, model_harness) VALUES ('2026-01-01','gemini')`, nil)
	if rejectOldGemini == nil {
		t.Error("expected post-V33 CHECK to reject legacy 'gemini', but insert succeeded")
	}
	for _, h := range []defaults.Harness{defaults.HarnessClaudeCode, defaults.HarnessGeminiCLI, defaults.HarnessCursor, defaults.HarnessAntigravity} {
		if err := sqlitex.ExecuteTransient(conn,
			`INSERT INTO daily_summary_harness (date_utc, model_harness) VALUES ('2026-01-02',`+"'"+string(h)+"')", nil); err != nil {
			t.Errorf("expected post-V33 CHECK to accept %q, got: %v", h, err)
		}
	}
}

// TestMigrationV33DataRename verifies the destructive in-place rename:
// claude->claude-code, gemini->gemini-cli; opencode/codex untouched; and that
// the rebuilt sessions table's CHECK now rejects the legacy values.
func TestMigrationV33DataRename(t *testing.T) {
	t.Parallel()
	conn, err := sqlite.OpenConn(":memory:", sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("open conn: %v", err)
	}
	defer conn.Close()

	// V33 runs with foreign keys disabled; the rebuilt sessions table declares
	// FKs to host_slugs/projects which we don't create here.
	execT(t, conn, "PRAGMA foreign_keys = OFF")
	execT(t, conn, preV33SessionsDDL)
	execT(t, conn, preV33DailySummaryDDL)

	insertSession := func(id, harness string) {
		execT(t, conn, `INSERT INTO sessions
			(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
			VALUES ('`+id+`','`+harness+`','m1','host1','proj1',1,2,3,'/x.jsonl','jsonl')`)
	}

	// Typed harness values (no bare provider string literals — ast-grep no-bare-provider-literals).
	oldClaude := string(defaults.LegacyHarnessClaude) // pre-rename "claude"
	oldGemini := string(defaults.LegacyHarnessGemini) // pre-rename "gemini"
	newClaude := string(defaults.HarnessClaudeCode)
	newGemini := string(defaults.HarnessGeminiCLI)
	opencode := string(defaults.HarnessOpenCode)
	codex := string(defaults.HarnessCodex)

	insertSession("s-claude", oldClaude)
	insertSession("s-gemini", oldGemini)
	insertSession("s-opencode", opencode)
	insertSession("s-codex", codex)

	execT(t, conn, `INSERT INTO daily_summary_harness (date_utc, model_harness, session_count) VALUES ('2026-01-01','`+oldClaude+`',5)`)
	execT(t, conn, `INSERT INTO daily_summary_harness (date_utc, model_harness, session_count) VALUES ('2026-01-01','`+oldGemini+`',3)`)

	// Run the actual migration SQL (multi-statement).
	if err := sqlitex.ExecuteScript(conn, migrationV33, nil); err != nil {
		t.Fatalf("execute migrationV33: %v", err)
	}

	// sessions: legacy values renamed, others preserved.
	if got := scalarText(t, conn, `SELECT model_harness FROM sessions WHERE session_id='s-claude'`); got != newClaude {
		t.Errorf("s-claude harness: got %q, want %q", got, newClaude)
	}
	if got := scalarText(t, conn, `SELECT model_harness FROM sessions WHERE session_id='s-gemini'`); got != newGemini {
		t.Errorf("s-gemini harness: got %q, want %q", got, newGemini)
	}
	if got := scalarText(t, conn, `SELECT model_harness FROM sessions WHERE session_id='s-opencode'`); got != opencode {
		t.Errorf("s-opencode harness: got %q, want %q (should be untouched)", got, opencode)
	}
	if got := scalarText(t, conn, `SELECT model_harness FROM sessions WHERE session_id='s-codex'`); got != codex {
		t.Errorf("s-codex harness: got %q, want %q (should be untouched)", got, codex)
	}
	if n := scalarInt(t, conn, `SELECT COUNT(*) FROM sessions`); n != 4 {
		t.Errorf("sessions row count after migration: got %d, want 4 (no rows lost)", n)
	}

	// daily_summary_harness: legacy values renamed, counts preserved.
	if got := scalarText(t, conn, `SELECT model_harness FROM daily_summary_harness WHERE date_utc='2026-01-01' AND session_count=5`); got != newClaude {
		t.Errorf("daily claude harness: got %q, want %q", got, newClaude)
	}
	if got := scalarText(t, conn, `SELECT model_harness FROM daily_summary_harness WHERE date_utc='2026-01-01' AND session_count=3`); got != newGemini {
		t.Errorf("daily gemini harness: got %q, want %q", got, newGemini)
	}

	// The rebuilt sessions CHECK must now reject the legacy identifiers.
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions
		(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
		VALUES ('s-legacy','`+oldClaude+`','m','h','p',1,2,3,'/x.jsonl','jsonl')`, nil); err == nil {
		t.Error("expected rebuilt sessions CHECK to reject legacy claude, but insert succeeded")
	}
}

// --- I2: consent gate (maybeRunV33Consent) ---

// writeConsentDB creates a minimal on-disk DB with a sessions table containing
// sessionRows rows and PRAGMA user_version set to userVersion. This is the
// minimum maybeRunV33Consent inspects (user_version + COUNT(*) FROM sessions).
func writeConsentDB(t *testing.T, dbPath string, userVersion, sessionRows int) {
	t.Helper()
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite|sqlite.OpenCreate)
	if err != nil {
		t.Fatalf("create consent DB: %v", err)
	}
	execT(t, conn, "CREATE TABLE sessions (session_id TEXT PRIMARY KEY, model_harness TEXT)")
	for i := range sessionRows {
		execT(t, conn, "INSERT INTO sessions (session_id, model_harness) VALUES ('s"+itoa(i)+"','claude')")
	}
	// PRAGMA user_version does not accept bound parameters; the int is safe to format.
	execT(t, conn, "PRAGMA user_version = "+itoa(userVersion))
	if err := conn.Close(); err != nil {
		t.Fatalf("close consent DB: %v", err)
	}
}

func itoa(i int) string {
	// small helper to avoid importing strconv just for test SQL building
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}

func TestMaybeRunV33Consent_FreshDB_NotCalled(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "absent.db") // never created
	called := false
	err := maybeRunV33Consent(dbPath, func(MigrationConsentContext) (bool, error) {
		called = true
		return true, nil
	})
	if err != nil {
		t.Fatalf("expected nil for fresh DB, got %v", err)
	}
	if called {
		t.Error("consent should NOT be called for a non-existent (fresh) DB")
	}
}

func TestMaybeRunV33Consent_EmptySessions_NotCalled(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	writeConsentDB(t, dbPath, 20, 0) // below v33, zero sessions
	called := false
	err := maybeRunV33Consent(dbPath, func(MigrationConsentContext) (bool, error) {
		called = true
		return true, nil
	})
	if err != nil {
		t.Fatalf("expected nil for empty DB, got %v", err)
	}
	if called {
		t.Error("consent should NOT be called when there are no sessions to migrate")
	}
}

func TestMaybeRunV33Consent_AlreadyMigrated_NotCalled(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "migrated.db")
	writeConsentDB(t, dbPath, migrationV33TargetVersion, 3) // already at/above v33
	called := false
	err := maybeRunV33Consent(dbPath, func(MigrationConsentContext) (bool, error) {
		called = true
		return true, nil
	})
	if err != nil {
		t.Fatalf("expected nil when already migrated, got %v", err)
	}
	if called {
		t.Error("consent should NOT be called when DB is already at/above v33")
	}
}

func TestMaybeRunV33Consent_NilCallback_Proceeds(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "nilcb.db")
	writeConsentDB(t, dbPath, 32, 2)
	if err := maybeRunV33Consent(dbPath, nil); err != nil {
		t.Fatalf("expected nil with nil consent callback, got %v", err)
	}
	if _, err := os.Stat(dbPath + ".pre-v33.bak"); !os.IsNotExist(err) {
		t.Error("no backup should be created when consent callback is nil")
	}
}

func TestMaybeRunV33Consent_Refused(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "refused.db")
	writeConsentDB(t, dbPath, 32, 2)
	err := maybeRunV33Consent(dbPath, func(MigrationConsentContext) (bool, error) {
		return false, nil
	})
	if !errors.Is(err, ErrMigrationRefused) {
		t.Fatalf("expected ErrMigrationRefused, got %v", err)
	}
	if _, err := os.Stat(dbPath + ".pre-v33.bak"); !os.IsNotExist(err) {
		t.Error("no backup should be created when consent is refused")
	}
}

func TestMaybeRunV33Consent_Proceeds_CreatesBackup(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "proceed.db")
	writeConsentDB(t, dbPath, 32, 2)
	if err := maybeRunV33Consent(dbPath, func(MigrationConsentContext) (bool, error) {
		return true, nil
	}); err != nil {
		t.Fatalf("expected nil when consent proceeds, got %v", err)
	}
	backupPath := dbPath + ".pre-v33.bak"
	orig, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("backup file should exist after consent: %v", err)
	}
	if !bytes.Equal(orig, backup) {
		t.Error("backup content should byte-for-byte match the original DB file")
	}
}

func TestMaybeRunV33Consent_ContextFields(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "ctx.db")
	writeConsentDB(t, dbPath, 30, 4)
	var got MigrationConsentContext
	if err := maybeRunV33Consent(dbPath, func(ctx MigrationConsentContext) (bool, error) {
		got = ctx
		return true, nil
	}); err != nil {
		t.Fatalf("consent: %v", err)
	}
	if got.DBPath != dbPath {
		t.Errorf("DBPath: got %q, want %q", got.DBPath, dbPath)
	}
	if got.CurrentVersion != 30 {
		t.Errorf("CurrentVersion: got %d, want 30", got.CurrentVersion)
	}
	if got.TargetVersion != migrationV33TargetVersion {
		t.Errorf("TargetVersion: got %d, want %d", got.TargetVersion, migrationV33TargetVersion)
	}
	if got.SessionsCount != 4 {
		t.Errorf("SessionsCount: got %d, want 4", got.SessionsCount)
	}
	if got.BackupPath != dbPath+".pre-v33.bak" {
		t.Errorf("BackupPath: got %q, want %q", got.BackupPath, dbPath+".pre-v33.bak")
	}
}

func TestMaybeRunV33Consent_CallbackError(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "err.db")
	writeConsentDB(t, dbPath, 32, 1)
	sentinel := errors.New("boom")
	err := maybeRunV33Consent(dbPath, func(MigrationConsentContext) (bool, error) {
		return false, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected wrapped sentinel error, got %v", err)
	}
}

// --- I2: prompt/parse seam (newConsentPrompter) ---

func TestNewConsentPrompter_Parsing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		in          string
		interactive bool
		want        bool
	}{
		{"y", "y\n", true, true},
		{"yes", "yes\n", true, true},
		{"uppercase Y", "Y\n", true, true},
		{"YES with spaces", "  YES  \n", true, true},
		{"n", "n\n", true, false},
		{"empty enter", "\n", true, false},
		{"garbage", "maybe\n", true, false},
		{"eof", "", true, false},
		{"non-interactive proceeds without reading", "", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			prompter := newConsentPrompter(strings.NewReader(c.in), &out, c.interactive)
			got, err := prompter(MigrationConsentContext{CurrentVersion: 32, TargetVersion: 33})
			if err != nil {
				t.Fatalf("prompter returned error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v (input %q, interactive=%v)", got, c.want, c.in, c.interactive)
			}
			if c.interactive && !strings.Contains(out.String(), "Proceed?") {
				t.Errorf("interactive prompt should write the question to out, got %q", out.String())
			}
			if !c.interactive && out.Len() != 0 {
				t.Errorf("non-interactive should write nothing, got %q", out.String())
			}
		})
	}
}

// Compile-time assurance that the seam returns the public callback type.
var _ MigrationConsent = newConsentPrompter(strings.NewReader(""), io.Discard, false)
