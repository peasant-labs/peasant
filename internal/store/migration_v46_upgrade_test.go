package store

import (
	"context"
	"path/filepath"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitemigration"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestMigrationV46UpgradesAStoreFrozenAtV45 covers the upgrade direction, which
// is the one a person with an existing database actually takes: a store that
// already carries every migration through the OpenCode change cursor, and none
// of the session-origin work, is opened by this build and must arrive at V46
// with both features intact.
//
// A fresh-from-empty store is covered by TestStore_Migrations_ApplyV1, which
// asserts the final user_version. This case is the complement, and it is the
// one a renumbering can break: if the origin migration were left in the slot
// the change cursor now owns, a frozen store would either skip the origin
// columns or re-run a migration it already holds.
func TestMigrationV46UpgradesAStoreFrozenAtV45(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "frozen-at-v45.db")

	// Freeze the database at the shape the default branch ships.
	pool, err := sqlitex.NewPool(dbPath, sqlitex.PoolOptions{PrepareConn: preparePragmas})
	if err != nil {
		t.Fatalf("open pool for the frozen store: %v", err)
	}
	conn, err := pool.Take(ctx)
	if err != nil {
		pool.Close()
		t.Fatalf("take connection for the frozen store: %v", err)
	}
	frozen := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:45],
		MigrationOptions: dbSchema.MigrationOptions[:45],
	}
	if err := sqlitemigration.Migrate(ctx, conn, frozen); err != nil {
		pool.Put(conn)
		pool.Close()
		t.Fatalf("migrate through V45: %v", err)
	}
	if got := upgradeUserVersion(t, conn); got != 45 {
		pool.Put(conn)
		pool.Close()
		t.Fatalf("frozen store user_version = %d, want 45", got)
	}
	if !upgradeTableExists(t, conn, "opencode_session_seq_cursor") {
		pool.Put(conn)
		pool.Close()
		t.Fatal("frozen store is missing opencode_session_seq_cursor, so it is not shaped like the default branch")
	}
	if _, present := upgradeColumn(t, conn, "sessions", "session_origin"); present {
		pool.Put(conn)
		pool.Close()
		t.Fatal("frozen store already carries sessions.session_origin, so it is not frozen before the origin migration")
	}
	// One evidence record mined before the origin field existed. After the
	// upgrade it must read as the empty marker, not as a legal origin.
	err = sqlitex.ExecuteTransient(conn, `
		INSERT INTO claude_transcript_evidence
		  (source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, spawns_json, title, branch, cwd)
		VALUES ('/tmp/frozen-at-v45.jsonl', 'root', 1, 1, 1, '[]', '', '', '')`, nil)
	if err != nil {
		pool.Put(conn)
		pool.Close()
		t.Fatalf("seed a pre-origin evidence record: %v", err)
	}
	pool.Put(conn)
	pool.Close()

	// Open the same file with this build, which is the real upgrade path.
	upgraded, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open the frozen store with this build: %v", err)
	}
	defer upgraded.Close()

	after, err := upgraded.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("take connection after the upgrade: %v", err)
	}
	defer upgraded.PoolForTest().Put(after)

	if got := upgradeUserVersion(t, after); got != 46 {
		t.Errorf("upgraded store user_version = %d, want 46", got)
	}
	// Their migration survives the upgrade.
	if !upgradeTableExists(t, after, "opencode_session_seq_cursor") {
		t.Error("the upgrade dropped opencode_session_seq_cursor")
	}
	// Ours arrives.
	origin, present := upgradeColumn(t, after, "sessions", "session_origin")
	if !present {
		t.Fatal("the upgrade did not add sessions.session_origin")
	}
	if origin.notNull != 1 {
		t.Errorf("sessions.session_origin notnull = %d, want 1", origin.notNull)
	}
	if origin.defaultValue != "'unknown'" {
		t.Errorf("sessions.session_origin default = %q, want %q", origin.defaultValue, "'unknown'")
	}
	if _, present := upgradeColumn(t, after, "sessions", "origin_version"); !present {
		t.Error("the upgrade did not add sessions.origin_version")
	}
	// The pre-origin evidence record keeps the empty marker rather than being
	// relabelled as a decided origin.
	var marker string
	var seen int
	err = sqlitex.ExecuteTransient(after, `SELECT origin FROM claude_transcript_evidence WHERE source_path = '/tmp/frozen-at-v45.jsonl'`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			marker = stmt.ColumnText(0)
			seen++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("read the migrated evidence record: %v", err)
	}
	if seen != 1 {
		t.Fatalf("read %d migrated evidence records, want 1", seen)
	}
	if marker != "" {
		t.Errorf("evidence record mined before the origin field reads origin=%q, want the empty marker", marker)
	}
	// The menu is enforced on the upgraded table, not only on a fresh one.
	err = sqlitex.ExecuteTransient(after, `
		INSERT INTO claude_transcript_evidence
		  (source_path, scope, mod_time_unix_nano, size_bytes, has_conversation, spawns_json, title, branch, cwd, origin)
		VALUES ('/tmp/frozen-at-v45-bad.jsonl', 'root', 1, 1, 1, '[]', '', '', '', 'teammate')`, nil)
	if err == nil {
		t.Error("the upgraded evidence table accepted an origin outside the closed menu")
	}
}

type upgradeColumnInfo struct {
	notNull      int
	defaultValue string
}

// upgradeColumn reports a column's NOT NULL flag and declared default, and
// whether the column is present at all.
func upgradeColumn(t *testing.T, conn *sqlite.Conn, table, column string) (upgradeColumnInfo, bool) {
	t.Helper()
	var info upgradeColumnInfo
	var found bool
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA table_info("+table+")", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnText(1) != column {
				return nil
			}
			found = true
			info.notNull = stmt.ColumnInt(3)
			info.defaultValue = stmt.ColumnText(4)
			return nil
		},
	}); err != nil {
		t.Fatalf("read table_info(%s): %v", table, err)
	}
	return info, found
}

// upgradeTableExists reports whether a table of that name is in the schema.
func upgradeTableExists(t *testing.T, conn *sqlite.Conn, table string) bool {
	t.Helper()
	var count int
	if err := sqlitex.ExecuteTransient(conn, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, &sqlitex.ExecOptions{
		Args: []any{table},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("look for table %s: %v", table, err)
	}
	return count == 1
}

// upgradeUserVersion reads PRAGMA user_version from an open connection.
func upgradeUserVersion(t *testing.T, conn *sqlite.Conn) int {
	t.Helper()
	var version int
	if err := sqlitex.ExecuteTransient(conn, "PRAGMA user_version", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			version = stmt.ColumnInt(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return version
}
