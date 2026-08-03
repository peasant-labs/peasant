package store_test

import (
	"testing"
)

// V26 migration adds part_type TEXT column to session_entries.

// ---------------------------------------------------------------------------
// TestMigrationV26Applies — schema structure
// ---------------------------------------------------------------------------

// TestMigrationV26Applies verifies that migration V26 adds the part_type column
// to session_entries and sets user_version = 26.
func TestMigrationV26Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 26 {
		t.Errorf("user_version: expected >= 26, got %d", uv)
	}

	// part_type column must exist on session_entries.
	colCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name = 'part_type'`)
	if colCount != 1 {
		t.Errorf("part_type column: expected 1, got %d", colCount)
	}
}

// TestMigrationV26PartTypeNullable verifies that part_type accepts NULL (depth=0 rows
// will have NULL part_type by convention).
func TestMigrationV26PartTypeNullable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// part_type must not have a NOT NULL constraint.
	notnull := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('session_entries') WHERE name = 'part_type' AND "notnull" = 0`)
	if notnull != 1 {
		t.Errorf("part_type notnull: expected column to be nullable (notnull=0), got count=%d", notnull)
	}
}
