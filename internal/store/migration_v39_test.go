package store_test

import (
	"testing"
)

// V39 seeds TWO entry-level annotation types (quality.turn_outcome,
// quality.turn_flag) backing the restored per-turn labeling modal in the web
// transcript viewer (usability feedback: "txn-label-popover was changed for no
// reason. What we had before worked properly."). These tests verify the
// seeded types' shape: enumerated/text value domains with the exact
// good/neutral/bad and none/error/retry_loop/revert/highlight permissible
// values, user origin, active status, turn_quality family, and entry-level
// targeting.

// TestMigrationV39Applies verifies user_version reached at least 39 and both
// new type rows exist.
func TestMigrationV39Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 39 {
		t.Errorf("user_version: expected >= 39, got %d", uv)
	}

	for _, typeID := range []string{"quality.turn_outcome", "quality.turn_flag"} {
		c := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_types WHERE type_id = ?`, typeID)
		if c != 1 {
			t.Fatalf("%s: expected exactly 1 row, got %d", typeID, c)
		}
	}
}

// TestMigrationV39TurnOutcomeShape verifies quality.turn_outcome's display
// name, enumerated value domain with the exact good/neutral/bad values,
// ordinal scale, user origin, active status, and turn_quality family.
func TestMigrationV39TurnOutcomeShape(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	const typeID = "quality.turn_outcome"

	got := queryText(t, conn, `SELECT display_name FROM annotation_types WHERE type_id = ?`, typeID)
	if got != "Turn outcome" {
		t.Errorf("display_name = %q, want %q", got, "Turn outcome")
	}

	domain := queryText(t, conn,
		`SELECT vdk.name FROM annotation_types t
		 JOIN value_domain_kinds vdk ON vdk.id = t.value_domain_kind_id
		 WHERE t.type_id = ?`, typeID)
	if domain != "enumerated" {
		t.Errorf("value_domain_kind = %q, want %q", domain, "enumerated")
	}

	// Exact permissible-value set the outcome segmented control renders.
	vc := queryText(t, conn, `SELECT value_constraint FROM annotation_types WHERE type_id = ?`, typeID)
	if vc != `["good","neutral","bad"]` {
		t.Errorf("value_constraint = %q, want %q", vc, `["good","neutral","bad"]`)
	}

	scale := queryText(t, conn,
		`SELECT sk.name FROM annotation_types t
		 JOIN scale_kinds sk ON sk.id = t.scale_kind_id
		 WHERE t.type_id = ?`, typeID)
	if scale != "ordinal" {
		t.Errorf("scale_kind = %q, want %q", scale, "ordinal")
	}

	origin := queryText(t, conn,
		`SELECT o.name FROM annotation_types t
		 JOIN type_origins o ON o.id = t.origin_id
		 WHERE t.type_id = ?`, typeID)
	if origin != "user" {
		t.Errorf("origin = %q, want %q", origin, "user")
	}

	status := queryText(t, conn,
		`SELECT s.name FROM annotation_types t
		 JOIN annotation_statuses s ON s.id = t.status_id
		 WHERE t.type_id = ?`, typeID)
	if status != "active" {
		t.Errorf("status = %q, want %q", status, "active")
	}

	family := queryText(t, conn,
		`SELECT f.family FROM annotation_types t
		 JOIN annotation_families f ON f.id = t.family_id
		 WHERE t.type_id = ?`, typeID)
	if family != "turn_quality" {
		t.Errorf("family = %q, want %q", family, "turn_quality")
	}
}

// TestMigrationV39TurnFlagShape verifies quality.turn_flag's display name,
// enumerated value domain with the exact 5-value flag set, nominal scale,
// user origin, active status, and turn_quality family.
func TestMigrationV39TurnFlagShape(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	const typeID = "quality.turn_flag"

	got := queryText(t, conn, `SELECT display_name FROM annotation_types WHERE type_id = ?`, typeID)
	if got != "Turn flag" {
		t.Errorf("display_name = %q, want %q", got, "Turn flag")
	}

	domain := queryText(t, conn,
		`SELECT vdk.name FROM annotation_types t
		 JOIN value_domain_kinds vdk ON vdk.id = t.value_domain_kind_id
		 WHERE t.type_id = ?`, typeID)
	if domain != "enumerated" {
		t.Errorf("value_domain_kind = %q, want %q", domain, "enumerated")
	}

	// Exact permissible-value set the flag chip row renders.
	vc := queryText(t, conn, `SELECT value_constraint FROM annotation_types WHERE type_id = ?`, typeID)
	want := `["none","error","retry_loop","revert","highlight"]`
	if vc != want {
		t.Errorf("value_constraint = %q, want %q", vc, want)
	}

	scale := queryText(t, conn,
		`SELECT sk.name FROM annotation_types t
		 JOIN scale_kinds sk ON sk.id = t.scale_kind_id
		 WHERE t.type_id = ?`, typeID)
	if scale != "nominal" {
		t.Errorf("scale_kind = %q, want %q", scale, "nominal")
	}

	origin := queryText(t, conn,
		`SELECT o.name FROM annotation_types t
		 JOIN type_origins o ON o.id = t.origin_id
		 WHERE t.type_id = ?`, typeID)
	if origin != "user" {
		t.Errorf("origin = %q, want %q", origin, "user")
	}

	status := queryText(t, conn,
		`SELECT s.name FROM annotation_types t
		 JOIN annotation_statuses s ON s.id = t.status_id
		 WHERE t.type_id = ?`, typeID)
	if status != "active" {
		t.Errorf("status = %q, want %q", status, "active")
	}

	family := queryText(t, conn,
		`SELECT f.family FROM annotation_types t
		 JOIN annotation_families f ON f.id = t.family_id
		 WHERE t.type_id = ?`, typeID)
	if family != "turn_quality" {
		t.Errorf("family = %q, want %q", family, "turn_quality")
	}
}

// TestMigrationV39EntryTargeting verifies both new types are registered for
// entry-level targeting (target_kind "entry"), so entryApplicableTypes offers
// them on a turn.
func TestMigrationV39EntryTargeting(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	for _, typeID := range []string{"quality.turn_outcome", "quality.turn_flag"} {
		c := queryInt(t, conn,
			`SELECT COUNT(*) FROM annotation_type_target_kinds atk
			 JOIN annotation_types t ON t.id = atk.annotation_type_id
			 JOIN target_kinds tk ON tk.id = atk.target_kind_id
			 WHERE t.type_id = ? AND tk.name = 'entry'`, typeID)
		if c != 1 {
			t.Errorf("%s: expected entry-level target kind, got count %d", typeID, c)
		}
	}
}
