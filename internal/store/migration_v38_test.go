package store_test

import (
	"testing"
)

// V38 adds the per-transcript license_id column to the pulled_transcripts
// table — the PULL-side mirror of V37's sessions.license_id (push side),
// recording the license the village served with the transcript (village
// licenses.id menu, seeded in village migration 026). Current value only,
// refreshed on every re-pull; license-change history is held server-side on
// the village. These tests verify the column exists and is nullable TEXT; the
// closed-set CHECK is exercised in migration_v38_check_test.go.

// TestMigrationV38Applies verifies user_version reached at least 38 and the
// new pulled_transcripts.license_id column exists with the expected nullable
// TEXT shape.
func TestMigrationV38Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 38 {
		t.Errorf("user_version: expected >= 38, got %d", uv)
	}

	// The license_id column must exist exactly once on pulled_transcripts.
	c := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('pulled_transcripts') WHERE name = 'license_id'`)
	if c != 1 {
		t.Fatalf("pulled_transcripts.license_id: expected exactly 1 column, got %d", c)
	}

	// It must be TEXT and nullable (no NOT NULL) — NULL = the village sent no
	// license (unset/legacy ⇒ default copyright).
	typ := queryText(t, conn, `SELECT type FROM pragma_table_info('pulled_transcripts') WHERE name = 'license_id'`)
	if typ != "TEXT" {
		t.Errorf("pulled_transcripts.license_id type = %q, want %q", typ, "TEXT")
	}
	notNull := queryInt(t, conn, `SELECT "notnull" FROM pragma_table_info('pulled_transcripts') WHERE name = 'license_id'`)
	if notNull != 0 {
		t.Errorf("pulled_transcripts.license_id notnull = %d, want 0 (nullable)", notNull)
	}
}
