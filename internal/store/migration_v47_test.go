package store_test

import (
	"strings"
	"testing"

	"zombiezen.com/go/sqlite/sqlitex"
)

func TestMigrationV47SessionEntriesHashColumn(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 47 {
		t.Errorf("user_version: expected >= 47, got %d", uv)
	}

	c := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'session_entries_hash'`)
	if c != 1 {
		t.Fatalf("sessions.session_entries_hash: expected exactly 1 column, got %d", c)
	}
	typ := queryText(t, conn, `SELECT type FROM pragma_table_info('sessions') WHERE name = 'session_entries_hash'`)
	if typ != "TEXT" {
		t.Errorf("sessions.session_entries_hash type = %q, want %q", typ, "TEXT")
	}
	notNull := queryInt(t, conn, `SELECT "notnull" FROM pragma_table_info('sessions') WHERE name = 'session_entries_hash'`)
	if notNull != 0 {
		t.Errorf("sessions.session_entries_hash notnull = %d, want 0", notNull)
	}

	seedSession(t, s, "99999999-3333-4333-8333-999999999999")
	valid := strings.Repeat("a", 64)
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_entries_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{valid, "99999999-3333-4333-8333-999999999999"}}); err != nil {
		t.Fatalf("valid session_entries_hash rejected: %v", err)
	}
	for _, invalid := range []string{strings.Repeat("b", 63), strings.Repeat("A", 64)} {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET session_entries_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{invalid, "99999999-3333-4333-8333-999999999999"}}); err == nil {
			t.Fatalf("invalid session_entries_hash %q was accepted", invalid)
		}
	}
}
