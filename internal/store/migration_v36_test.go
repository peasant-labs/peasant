package store_test

import (
	"testing"
)

// V36 seeds ONE general-purpose free-text entry annotation type
// (user.custom_label) so the web transcript viewer's per-turn "Label" picker
// can save arbitrary strings. These tests verify the seeded type's
// shape: described/text value domain with an empty constraint (free text), user
// origin, active status, turn_metadata family, and entry-level targeting.

// TestMigrationV36Applies verifies user_version reached at least 36 and the
// new type row exists.
func TestMigrationV36Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 36 {
		t.Errorf("user_version: expected >= 36, got %d", uv)
	}

	c := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_types WHERE type_id = 'user.custom_label'`)
	if c != 1 {
		t.Fatalf("user.custom_label: expected exactly 1 row, got %d", c)
	}
}

// TestMigrationV36CustomLabelShape verifies the seeded type's display name,
// value domain (described/text with empty constraint), origin, status, and
// family — the exact shape the free-text label picker and the batch validator
// depend on.
func TestMigrationV36CustomLabelShape(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	const typeID = "user.custom_label"

	got := queryText(t, conn, `SELECT display_name FROM annotation_types WHERE type_id = ?`, typeID)
	if got != "Custom label" {
		t.Errorf("display_name = %q, want %q", got, "Custom label")
	}

	// Value domain kind must be "described" (free text), NOT "enumerated":
	// the picker renders a text input only when there are no permissible values.
	domain := queryText(t, conn,
		`SELECT vdk.name FROM annotation_types t
		 JOIN value_domain_kinds vdk ON vdk.id = t.value_domain_kind_id
		 WHERE t.type_id = ?`, typeID)
	if domain != "described" {
		t.Errorf("value_domain_kind = %q, want %q", domain, "described")
	}

	datatype := queryText(t, conn, `SELECT datatype FROM annotation_types WHERE type_id = ?`, typeID)
	if datatype != "text" {
		t.Errorf("datatype = %q, want %q", datatype, "text")
	}

	// Empty constraint spec → the validator (ValidateDescribedValue) accepts any
	// non-empty string. A non-empty constraint here would re-impose validation.
	vc := queryText(t, conn, `SELECT value_constraint FROM annotation_types WHERE type_id = ?`, typeID)
	if vc != "" {
		t.Errorf("value_constraint = %q, want empty string (free text)", vc)
	}

	origin := queryText(t, conn,
		`SELECT o.name FROM annotation_types t
		 JOIN type_origins o ON o.id = t.origin_id
		 WHERE t.type_id = ?`, typeID)
	if origin != "user" {
		t.Errorf("origin = %q, want %q", origin, "user")
	}

	// Status must be active so it appears as a usable choice in the picker.
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
	if family != "turn_metadata" {
		t.Errorf("family = %q, want %q", family, "turn_metadata")
	}
}

// TestMigrationV36EntryTargeting verifies the type is registered for
// entry-level targeting (target_kind "entry"), so entryApplicableTypes offers
// it on a turn.
func TestMigrationV36EntryTargeting(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	c := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_type_target_kinds atk
		 JOIN annotation_types t ON t.id = atk.annotation_type_id
		 JOIN target_kinds tk ON tk.id = atk.target_kind_id
		 WHERE t.type_id = 'user.custom_label' AND tk.name = 'entry'`)
	if c != 1 {
		t.Errorf("user.custom_label: expected entry-level target kind, got count %d", c)
	}
}
