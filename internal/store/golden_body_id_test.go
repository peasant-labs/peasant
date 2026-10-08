package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

// TestGoldenTemplateBodyInsert proves the storetest golden template — the
// migrated database every storetest.Open copy derives from — allocates its
// first entry body at BodyRowIDBase under the writer contract, exactly like a
// fresh install and a chain-migrated store. The copy's user_version pins the
// template to the build schema, so a template built from the wrong baseline
// fails here instead of passing on a stale schema. This is the golden leg of
// the fresh/migrated/golden trio; the fresh and migrated legs live in
// TestSchemaFreshVsMigrated.
func TestGoldenTemplateBodyInsert(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t)
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	var userVersion int64
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA user_version`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			userVersion = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("PRAGMA user_version on golden copy: %v", err)
	}
	if int(userVersion) != store.CurrentSchemaVersion() {
		t.Fatalf("golden copy user_version = %d, want the build schema %d", userVersion, store.CurrentSchemaVersion())
	}

	const sessionID = "s-golden-first-body"
	storetest.SeedSession(t, s, sessionID)
	digest := strings.Repeat("ab", 32)
	if err := sqlitex.ExecuteTransient(conn, fmt.Sprintf(`
INSERT INTO session_entry_bodies(body_id, session_id, body_digest, entry_index, harness, entry_type, role, has_tool_use, has_thinking, is_error, depth)
VALUES ((SELECT coalesce(max(body_id), %d) + 1 FROM session_entry_bodies), ?, ?, 0, 'opencode', 'text', 'user', 0, 0, 0, 0)`, store.BodyRowIDBase-1), &sqlitex.ExecOptions{
		Args: []any{sessionID, digest},
	}); err != nil {
		t.Fatalf("allocate first body on golden copy: %v", err)
	}
	var bodyID int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT body_id FROM session_entry_bodies WHERE session_id=?`, &sqlitex.ExecOptions{
		Args: []any{sessionID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			bodyID = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read back first body on golden copy: %v", err)
	}
	if bodyID != store.BodyRowIDBase {
		t.Errorf("golden-template-body-insert: first body_id = %d, want BodyRowIDBase %d", bodyID, store.BodyRowIDBase)
	}
}
