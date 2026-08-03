package store_test

import (
	"testing"
)

// v18 migration seeds 2 entry-level annotation types (quality.frustration_signal,
// quality.resolution_evidence), 2 rule-based annotators (frustration-signal-classifier,
// resolution-evidence-classifier), and registers allowed target kinds (entry-level).

// ---------------------------------------------------------------------------
// TestMigrationV18Applies — schema structure
// ---------------------------------------------------------------------------

// TestMigrationV18Applies verifies that migration V18 seeds the new annotation
// types and annotators, and sets user_version = 18.
func TestMigrationV18Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V1: user_version >= 18 (V18 migration and all subsequent applied).
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 18 {
		t.Errorf("user_version: expected >= 18, got %d", uv)
	}

	// V2: at least the V13+V18+V20 entry/session types are present. openTestStore
	// applies ALL migrations, so later seeds (research.friction_episode in V25,
	// user.custom_label in V35) push the total up — use >= per the openTestStore
	// convention. V18's own types are verified by type_id below.
	typeCount := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_types`)
	if typeCount < 8 {
		t.Errorf("annotation_types: expected >= 8 rows, got %d", typeCount)
	}

	// V3: 6 annotators (3 rule from V13, 1 human from V15, 2 rule from V18, 1 human from V24).
	annotatorCount := queryInt(t, conn, `SELECT COUNT(*) FROM annotators`)
	if annotatorCount != 6 {
		t.Errorf("annotators: expected 6 rows, got %d", annotatorCount)
	}
}

// TestMigrationV18SeedTypes verifies the 2 new entry-level annotation types.
func TestMigrationV18SeedTypes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	cases := []struct {
		typeID          string
		displayName     string
		valueConstraint string
		wantScaleKind   string
	}{
		{
			typeID:          "quality.frustration_signal",
			displayName:     "Frustration Signal",
			valueConstraint: `["detected","not_detected"]`,
			wantScaleKind:   "nominal",
		},
		{
			typeID:          "quality.resolution_evidence",
			displayName:     "Resolution Evidence",
			valueConstraint: `["present","absent"]`,
			wantScaleKind:   "nominal",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.typeID, func(t *testing.T) {
			// Type exists.
			c := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_types WHERE type_id = ?`, tc.typeID)
			if c != 1 {
				t.Fatalf("annotation type %q not found", tc.typeID)
			}

			// Display name matches.
			got := queryText(t, conn, `SELECT display_name FROM annotation_types WHERE type_id = ?`, tc.typeID)
			if got != tc.displayName {
				t.Errorf("display_name = %q, want %q", got, tc.displayName)
			}

			// Value constraint matches.
			gotVC := queryText(t, conn, `SELECT value_constraint FROM annotation_types WHERE type_id = ?`, tc.typeID)
			if gotVC != tc.valueConstraint {
				t.Errorf("value_constraint = %q, want %q", gotVC, tc.valueConstraint)
			}

			// Scale kind assigned correctly.
			gotScale := queryText(t, conn,
				`SELECT sk.name FROM annotation_types t
				 JOIN scale_kinds sk ON sk.id = t.scale_kind_id
				 WHERE t.type_id = ?`, tc.typeID)
			if gotScale != tc.wantScaleKind {
				t.Errorf("scale_kind = %q, want %q", gotScale, tc.wantScaleKind)
			}

			// Origin is system.
			gotOrigin := queryText(t, conn,
				`SELECT o.name FROM annotation_types t
				 JOIN type_origins o ON o.id = t.origin_id
				 WHERE t.type_id = ?`, tc.typeID)
			if gotOrigin != "system" {
				t.Errorf("origin = %q, want %q", gotOrigin, "system")
			}

			// Status is deprecated (V24 deprecates all pre-research types).
			gotStatus := queryText(t, conn,
				`SELECT s.name FROM annotation_types t
				 JOIN annotation_statuses s ON s.id = t.status_id
				 WHERE t.type_id = ?`, tc.typeID)
			if gotStatus != "deprecated" {
				t.Errorf("status = %q, want %q", gotStatus, "deprecated")
			}

			// Family is turn_quality.
			gotFamily := queryText(t, conn,
				`SELECT f.family FROM annotation_types t
				 JOIN annotation_families f ON f.id = t.family_id
				 WHERE t.type_id = ?`, tc.typeID)
			if gotFamily != "turn_quality" {
				t.Errorf("family = %q, want %q", gotFamily, "turn_quality")
			}
		})
	}
}

// TestMigrationV18SeedAnnotators verifies the 2 new entry-level annotators.
func TestMigrationV18SeedAnnotators(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	cases := []struct {
		name        string
		displayName string
	}{
		{"frustration-signal-classifier", "Frustration Signal Classifier"},
		{"resolution-evidence-classifier", "Resolution Evidence Classifier"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := queryInt(t, conn, `SELECT COUNT(*) FROM annotators WHERE name = ?`, tc.name)
			if c != 1 {
				t.Fatalf("annotator %q not found", tc.name)
			}

			got := queryText(t, conn, `SELECT display_name FROM annotators WHERE name = ?`, tc.name)
			if got != tc.displayName {
				t.Errorf("display_name = %q, want %q", got, tc.displayName)
			}

			// Kind is rule.
			gotKind := queryText(t, conn,
				`SELECT ak.name FROM annotators a
				 JOIN annotator_kinds ak ON ak.id = a.kind_id
				 WHERE a.name = ?`, tc.name)
			if gotKind != "rule" {
				t.Errorf("kind = %q, want %q", gotKind, "rule")
			}
		})
	}
}

// TestMigrationV18AllowedTargetKinds verifies that the new types are registered
// for entry-level targeting (target_kind_id = 2).
func TestMigrationV18AllowedTargetKinds(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	for _, typeID := range []string{"quality.frustration_signal", "quality.resolution_evidence"} {
		c := queryInt(t, conn,
			`SELECT COUNT(*) FROM annotation_type_target_kinds atk
			 JOIN annotation_types t ON t.id = atk.annotation_type_id
			 JOIN target_kinds tk ON tk.id = atk.target_kind_id
			 WHERE t.type_id = ? AND tk.name = 'entry'`, typeID)
		if c != 1 {
			t.Errorf("annotation type %q: expected entry-level target kind, got count %d", typeID, c)
		}
	}
}

// TestMigrationV18FKValidity verifies that all V18 seed rows have valid FKs.
func TestMigrationV18FKValidity(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// annotation_types → annotation_families FK valid.
	orphanTypes := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_types t
		 LEFT JOIN annotation_families f ON f.id = t.family_id
		 WHERE f.id IS NULL`)
	if orphanTypes != 0 {
		t.Errorf("annotation_types: %d rows have invalid family_id FK", orphanTypes)
	}

	// annotation_type_target_kinds → annotation_types FK valid.
	orphanATK := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_type_target_kinds atk
		 LEFT JOIN annotation_types t ON t.id = atk.annotation_type_id
		 WHERE t.id IS NULL`)
	if orphanATK != 0 {
		t.Errorf("annotation_type_target_kinds: %d rows have invalid annotation_type_id FK", orphanATK)
	}
}
