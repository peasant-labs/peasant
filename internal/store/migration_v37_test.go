package store_test

import (
	"testing"
)

// V37 adds the per-transcript license_id column to the sessions table, mirroring
// the village's authoritative licenses.id (CC0-1.0 / CC-BY-4.0 / CC-BY-SA-4.0,
// seeded in village migration 026). Current value only; license-change history
// is held server-side on the village. These tests verify the column exists and is
// nullable TEXT; the closed-set CHECK is exercised in migration_v37_check_test.go.

// TestMigrationV37Applies verifies user_version reached at least 37 and the
// new sessions.license_id column exists with the expected nullable TEXT shape.
func TestMigrationV37Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 37 {
		t.Errorf("user_version: expected >= 37, got %d", uv)
	}

	// The license_id column must exist exactly once on sessions.
	c := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'license_id'`)
	if c != 1 {
		t.Fatalf("sessions.license_id: expected exactly 1 column, got %d", c)
	}

	// It must be TEXT and nullable (no NOT NULL) — NULL = unset/legacy.
	typ := queryText(t, conn, `SELECT type FROM pragma_table_info('sessions') WHERE name = 'license_id'`)
	if typ != "TEXT" {
		t.Errorf("sessions.license_id type = %q, want %q", typ, "TEXT")
	}
	notNull := queryInt(t, conn, `SELECT "notnull" FROM pragma_table_info('sessions') WHERE name = 'license_id'`)
	if notNull != 0 {
		t.Errorf("sessions.license_id notnull = %d, want 0 (nullable)", notNull)
	}
}
