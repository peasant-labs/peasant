package store_test

import (
	"testing"
)

// v19 migration seeds annotation_type_deps linking entry-level evidence classifiers
// to their session-level counterparts:
//   - quality.session_outcome depends on quality.resolution_evidence (required)
//   - quality.user_frustration depends on quality.frustration_signal (required)

// ---------------------------------------------------------------------------
// TestMigrationV19Applies — schema structure
// ---------------------------------------------------------------------------

// TestMigrationV19Applies verifies that migration V19 inserts the dependency
// rows and sets user_version = 19.
func TestMigrationV19Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V1: user_version >= 19 (V19 migration and all subsequent applied).
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 19 {
		t.Errorf("user_version: expected >= 19, got %d", uv)
	}

	// V2: annotation_type_deps has 3 rows (1 from V13 + 2 from V19).
	depCount := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_type_deps`)
	if depCount != 3 {
		t.Errorf("annotation_type_deps: expected 3 rows, got %d", depCount)
	}
}

// TestMigrationV19SeedDeps verifies the 2 new entry-level dependency links.
func TestMigrationV19SeedDeps(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	cases := []struct {
		parentTypeID string
		depTypeID    string
		wantRequired int
	}{
		{
			parentTypeID: "quality.session_outcome",
			depTypeID:    "quality.resolution_evidence",
			wantRequired: 1,
		},
		{
			parentTypeID: "quality.user_frustration",
			depTypeID:    "quality.frustration_signal",
			wantRequired: 1,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.parentTypeID+"->"+tc.depTypeID, func(t *testing.T) {
			// Dependency row exists.
			c := queryInt(t, conn,
				`SELECT COUNT(*) FROM annotation_type_deps d
				 JOIN annotation_types p ON p.id = d.annotation_type_id
				 JOIN annotation_types dep ON dep.id = d.depends_on_id
				 WHERE p.type_id = ? AND dep.type_id = ?`,
				tc.parentTypeID, tc.depTypeID)
			if c != 1 {
				t.Fatalf("dependency %s -> %s not found", tc.parentTypeID, tc.depTypeID)
			}

			// Required flag matches.
			got := queryInt(t, conn,
				`SELECT d.required FROM annotation_type_deps d
				 JOIN annotation_types p ON p.id = d.annotation_type_id
				 JOIN annotation_types dep ON dep.id = d.depends_on_id
				 WHERE p.type_id = ? AND dep.type_id = ?`,
				tc.parentTypeID, tc.depTypeID)
			if got != tc.wantRequired {
				t.Errorf("required = %d, want %d", got, tc.wantRequired)
			}

			// Rationale is non-empty.
			rationale := queryText(t, conn,
				`SELECT d.rationale FROM annotation_type_deps d
				 JOIN annotation_types p ON p.id = d.annotation_type_id
				 JOIN annotation_types dep ON dep.id = d.depends_on_id
				 WHERE p.type_id = ? AND dep.type_id = ?`,
				tc.parentTypeID, tc.depTypeID)
			if rationale == "" {
				t.Errorf("rationale is empty for dependency %s -> %s", tc.parentTypeID, tc.depTypeID)
			}
		})
	}
}

// TestMigrationV19FKValidity verifies that all V19 seed rows have valid FKs.
func TestMigrationV19FKValidity(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// annotation_type_deps → annotation_types FK valid (both sides).
	orphanParent := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_type_deps d
		 LEFT JOIN annotation_types p ON p.id = d.annotation_type_id
		 WHERE p.id IS NULL`)
	if orphanParent != 0 {
		t.Errorf("annotation_type_deps: %d rows have invalid annotation_type_id FK", orphanParent)
	}

	orphanDep := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_type_deps d
		 LEFT JOIN annotation_types dep ON dep.id = d.depends_on_id
		 WHERE dep.id IS NULL`)
	if orphanDep != 0 {
		t.Errorf("annotation_type_deps: %d rows have invalid depends_on_id FK", orphanDep)
	}
}
