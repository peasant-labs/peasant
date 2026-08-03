package store_test

import (
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// v17 migration adds the scale_kinds lookup table with 3 rows (nominal, ordinal, continuous)
// and the scale_kind_id FK column on annotation_types. The 4 V13 seed annotation types
// (preserved through V16 UUID refactor) are updated:
// outcome=ordinal, frustration=nominal, scope=nominal, approval=nominal.

// ---------------------------------------------------------------------------
// TestMigrationV17Applies — schema structure
// ---------------------------------------------------------------------------

// TestMigrationV17Applies verifies that migration V17 creates the scale_kinds
// table, adds scale_kind_id column to annotation_types, and seeds 3 scale kinds.
func TestMigrationV17Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V1: scale_kinds table exists.
	tableCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='scale_kinds'`)
	if tableCount != 1 {
		t.Fatal("scale_kinds table not found after migration V17")
	}

	// V2: scale_kinds has 3 seed rows.
	count := queryInt(t, conn, `SELECT COUNT(*) FROM scale_kinds`)
	if count != 3 {
		t.Errorf("scale_kinds: expected 3 seed rows, got %d", count)
	}

	// V3: scale_kind_id column exists on annotation_types.
	colCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('annotation_types') WHERE name='scale_kind_id'`)
	if colCount != 1 {
		t.Error("annotation_types.scale_kind_id column not found after migration V17")
	}

	// V4: user_version >= 17 (V17 migration and all subsequent applied).
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 17 {
		t.Errorf("user_version: expected >= 17, got %d", uv)
	}
}

// TestMigrationV17ScaleKindSeeds verifies all 3 scale kind names exist.
func TestMigrationV17ScaleKindSeeds(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	for _, name := range []string{"nominal", "ordinal", "continuous"} {
		c := queryInt(t, conn, `SELECT COUNT(*) FROM scale_kinds WHERE name=?`, name)
		if c != 1 {
			t.Errorf("scale_kinds: missing name %q", name)
		}
	}
}

// TestMigrationV17SeedTypeScaleKinds verifies the BDD criterion:
// Given V17 migration, then:
//   - quality.session_outcome   → ordinal
//   - quality.user_frustration  → nominal
//   - metadata.session_scope    → nominal
//   - quality.session_approval  → nominal
func TestMigrationV17SeedTypeScaleKinds(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	cases := []struct {
		typeID    string
		wantScale string
	}{
		{"quality.session_outcome", "ordinal"},
		{"quality.user_frustration", "nominal"},
		{"metadata.session_scope", "nominal"},
		{"quality.session_approval", "nominal"},
	}

	for _, tc := range cases {
		// Join annotation_types with scale_kinds to get the name.
		var gotScale string
		err := sqlitex.ExecuteTransient(conn,
			`SELECT sk.name
             FROM annotation_types t
             JOIN scale_kinds sk ON sk.id = t.scale_kind_id
             WHERE t.type_id = ?`,
			&sqlitex.ExecOptions{
				Args: []any{tc.typeID},
				ResultFunc: func(stmt *sqlite.Stmt) error {
					gotScale = stmt.ColumnText(0)
					return nil
				},
			})
		if err != nil {
			t.Errorf("query scale_kind for %q: %v", tc.typeID, err)
			continue
		}
		if gotScale == "" {
			t.Errorf("annotation_type %q: scale_kind_id is NULL (not set)", tc.typeID)
			continue
		}
		if gotScale != tc.wantScale {
			t.Errorf("annotation_type %q: scale_kind = %q, want %q", tc.typeID, gotScale, tc.wantScale)
		}
	}
}

// TestMigrationV17ScaleKindNullable verifies scale_kind_id is nullable (allows NULL for user-defined types).
func TestMigrationV17ScaleKindNullable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Column should be nullable (notnull=0).
	nullableCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('annotation_types') WHERE name='scale_kind_id' AND "notnull"=0`)
	if nullableCount != 1 {
		t.Error("annotation_types.scale_kind_id should be nullable (notnull=0)")
	}
}

// TestMigrationV17InsertAnnotationTypeWithScaleKind verifies that a new annotation type
// can be inserted with a scale_kind_id.
func TestMigrationV17InsertAnnotationTypeWithScaleKind(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Get a valid family_id UUID from the DB.
	familyID := queryText(t, conn, `SELECT id FROM annotation_families WHERE family='session_quality'`)
	if familyID == "" {
		t.Fatal("session_quality family not found")
	}

	// Insert a new annotation type with a scale_kind_id (continuous).
	// V16: annotation_types uses UUID PK and INT FK enums for value_domain_kind, status, origin.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, version, display_name, family_id,
          value_domain_kind_id, datatype, value_constraint,
          scale_kind_id, status_id, origin_id, created_at)
         VALUES (
             'test-v17-uuid-0000-0000-000000000001',
             'quality.test_continuous', 1, 'Test Continuous', ?,
             (SELECT id FROM value_domain_kinds WHERE name='described'),
             'real', '{"minimum":0,"maximum":1}',
             (SELECT id FROM scale_kinds WHERE name='continuous'),
             (SELECT id FROM annotation_statuses WHERE name='proposed'),
             (SELECT id FROM type_origins WHERE name='user'),
             1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{familyID}})
	if err != nil {
		t.Errorf("insert annotation_type with scale_kind_id=continuous: %v", err)
	}

	// Verify the scale kind is set correctly.
	gotScale := queryText(t, conn,
		`SELECT sk.name FROM annotation_types t
         JOIN scale_kinds sk ON sk.id = t.scale_kind_id
         WHERE t.type_id = 'quality.test_continuous'`)
	if gotScale != "continuous" {
		t.Errorf("scale_kind: expected %q, got %q", "continuous", gotScale)
	}
}

// TestMigrationV17ScaleKindFKEnforced verifies that scale_kind_id with an invalid FK
// is rejected (FK enforcement).
func TestMigrationV17ScaleKindFKEnforced(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Enable FK enforcement for this test.
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA foreign_keys = ON`, nil); err != nil {
		t.Fatalf("enable foreign_keys: %v", err)
	}

	// Get a valid family_id UUID from the DB.
	familyID := queryText(t, conn, `SELECT id FROM annotation_families WHERE family='session_quality'`)
	if familyID == "" {
		t.Fatal("session_quality family not found")
	}

	// Insert with invalid scale_kind_id (99 does not exist in scale_kinds).
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, version, display_name, family_id,
          value_domain_kind_id, datatype, value_constraint,
          scale_kind_id, status_id, origin_id, created_at)
         VALUES (
             'test-v17-uuid-0000-0000-000000000002',
             'quality.bad_scale', 1, 'Bad Scale', ?,
             (SELECT id FROM value_domain_kinds WHERE name='enumerated'),
             'text', '["a","b"]',
             99,
             (SELECT id FROM annotation_statuses WHERE name='proposed'),
             (SELECT id FROM type_origins WHERE name='user'),
             1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{familyID}})
	if err == nil {
		t.Error("insert with invalid scale_kind_id=99 should fail FK constraint")
	}
}
