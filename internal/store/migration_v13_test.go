package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/store"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// v13 test session IDs (valid UUIDs).
const (
	v13TestSID1 = "c1300000-0000-0000-0000-000000000001"
	v13TestSID2 = "c1300000-0000-0000-0000-000000000002"
)

// ---------------------------------------------------------------------------
// TestMigrationV13Applies — all 6 tables + 1 view created, user_version = 13
// ---------------------------------------------------------------------------

// TestMigrationV13Applies verifies that migration V13 creates all 6 annotation
// tables and the annotations_with_target view, and sets user_version = 13.
func TestMigrationV13Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V1: Verify all 6 tables exist.
	expectedTables := []string{
		"annotation_classes",
		"annotation_families",
		"annotation_types",
		"annotation_type_deps",
		"annotators",
		"annotations",
	}
	for _, tbl := range expectedTables {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl)
		if count != 1 {
			t.Errorf("V1: table %q not found after migration V13", tbl)
		}
	}

	// V1: Verify the view exists.
	viewCount := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='view' AND name='annotations_with_target'`)
	if viewCount != 1 {
		t.Fatal("V1: annotations_with_target view not found after migration V13")
	}

	// Verify user_version >= 13 (V13 and all prior migrations applied).
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 13 {
		t.Errorf("user_version: expected >= 13, got %d", uv)
	}
}

// TestMigrationV13AllTableNames verifies AllTableNames includes all 6 new tables (V20).
func TestMigrationV13AllTableNames(t *testing.T) {
	t.Parallel()

	required := []string{
		"annotation_classes",
		"annotation_families",
		"annotation_types",
		"annotation_type_deps",
		"annotators",
		"annotations",
	}

	// Build lookup from AllTableNames.
	nameSet := make(map[string]bool, len(store.AllTableNames))
	for _, n := range store.AllTableNames {
		nameSet[n] = true
	}

	for _, name := range required {
		if !nameSet[name] {
			t.Errorf("V20: AllTableNames is missing %q", name)
		}
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV13SeedData — seed rows count + FK validity
// ---------------------------------------------------------------------------

// TestMigrationV13SeedClasses verifies 3 annotation_classes seed rows (V3).
func TestMigrationV13SeedClasses(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V3: 4 annotation_classes seed rows (quality, metadata, behavior, research).
	count := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_classes`)
	if count != 4 {
		t.Errorf("V3: annotation_classes: expected 4 seed rows, got %d", count)
	}

	// V3: Expected class names.
	for _, class := range []string{"quality", "metadata", "behavior", "research"} {
		c := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_classes WHERE class=?`, class)
		if c != 1 {
			t.Errorf("V3: annotation_classes: missing class %q", class)
		}
	}

	// V16: id is TEXT (UUID PK — V16 rebuilt annotation_classes with TEXT PK).
	idType := queryText(t, conn, `SELECT type FROM pragma_table_info('annotation_classes') WHERE name='id'`)
	if idType != "TEXT" {
		t.Errorf("V16: annotation_classes.id: expected type TEXT (UUID), got %q", idType)
	}

	// V8a: class is a UNIQUE text column (not PK); must be NOT NULL.
	classNotNull := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('annotation_classes') WHERE name='class' AND "notnull"=1`)
	if classNotNull != 1 {
		t.Error("V8a: annotation_classes.class should be NOT NULL")
	}
}

// TestMigrationV13SeedFamilies verifies 5 annotation_families seed rows (V4).
func TestMigrationV13SeedFamilies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V4: 6 annotation_families seed rows (5 original + 1 from V25).
	// V4: 6 annotation_families seed rows (5 original + 1 from V24).
	count := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_families`)
	if count != 6 {
		t.Errorf("V4: annotation_families: expected 6 seed rows, got %d", count)
	}

	// V4: All family rows have valid class_id FKs.
	orphanCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_families f
         LEFT JOIN annotation_classes c ON c.id = f.class_id
         WHERE c.id IS NULL`)
	if orphanCount != 0 {
		t.Errorf("V4: annotation_families: %d rows have invalid class_id FK", orphanCount)
	}

	// V26: Families have class_id (types FK to families, not directly to classes).
	classIDCount := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('annotation_families') WHERE name='class_id'`)
	if classIDCount != 1 {
		t.Error("V26: annotation_families: missing class_id column")
	}
}

// TestMigrationV13SeedAnnotationTypes verifies 4 annotation types seeded (V5).
func TestMigrationV13SeedAnnotationTypes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// openTestStore applies ALL migrations, so the total grows as later
	// migrations seed types (research.friction_episode in V25, user.custom_label
	// in V35). Use >= per the openTestStore convention; V13's own seed types are
	// verified by type_id below.
	count := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_types`)
	if count < 8 {
		t.Errorf("V5+V18+V20: annotation_types: expected >= 8 seed rows, got %d", count)
	}

	// V5: Expected type_ids present.
	for _, typeID := range []string{
		"quality.session_approval",
		"quality.session_outcome",
		"quality.user_frustration",
		"metadata.session_scope",
	} {
		c := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_types WHERE type_id=?`, typeID)
		if c != 1 {
			t.Errorf("V5: annotation_types: missing type_id %q", typeID)
		}
	}

	// V5: All FK-valid (family_id references annotation_families).
	orphanCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_types t
         LEFT JOIN annotation_families f ON f.id = t.family_id
         WHERE f.id IS NULL`)
	if orphanCount != 0 {
		t.Errorf("V5: annotation_types: %d rows have invalid family_id FK", orphanCount)
	}

	// V12: lower_is_better is NULL for all seed rows (non-ordinal types).
	nonNullLIB := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_types WHERE lower_is_better IS NOT NULL`)
	if nonNullLIB != 0 {
		t.Errorf("V12: expected all seed types to have NULL lower_is_better, got %d non-null", nonNullLIB)
	}
}

// TestMigrationV13SeedAnnotators verifies 3 annotators seeded (V6).
func TestMigrationV13SeedAnnotators(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V6+V15+V18: 6 annotators seeded (3 rule from V13, 1 human from V15, 2 rule from V18).
	count := queryInt(t, conn, `SELECT COUNT(*) FROM annotators`)
	if count != 6 {
		t.Errorf("V6+V15+V18: annotators: expected 6 seed rows, got %d", count)
	}

	// V6+V18: 5 rule-based annotators (3 from V13 + 2 from V18).
	// V16: kind column renamed to kind_id (INT FK to annotator_kinds).
	// Join with annotator_kinds to filter by kind name.
	ruleCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotators a
         JOIN annotator_kinds ak ON ak.id = a.kind_id
         WHERE ak.name = 'rule'`)
	if ruleCount != 5 {
		t.Errorf("V6+V18: expected 5 rule-based seed annotators, got %d", ruleCount)
	}

	// V6: Expected rule annotator names.
	for _, name := range []string{"outcome-classifier", "frustration-classifier", "scope-classifier"} {
		c := queryInt(t, conn, `SELECT COUNT(*) FROM annotators WHERE name=?`, name)
		if c != 1 {
			t.Errorf("V6: annotators: missing name %q", name)
		}
	}

	// V15: human-web annotator seeded.
	// V16: kind_id=1 is 'human' per annotator_kinds seed.
	humanWebCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotators a
         JOIN annotator_kinds ak ON ak.id = a.kind_id
         WHERE a.name='human-web' AND ak.name='human'`)
	if humanWebCount != 1 {
		t.Errorf("V15: expected 1 human-web annotator, got %d", humanWebCount)
	}
}

// TestMigrationV13SeedDependency verifies the V13 seed dependency (outcome→scope).
func TestMigrationV13SeedDependency(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V13 seeds 1 dependency; V19 adds 2 more → 3 total.
	count := queryInt(t, conn, `SELECT COUNT(*) FROM annotation_type_deps`)
	if count != 3 {
		t.Errorf("annotation_type_deps: expected 3 rows (1 V13 + 2 V19), got %d", count)
	}

	// V13 seed: outcome depends on scope, required=0.
	depCount := queryInt(t, conn, `
        SELECT COUNT(*) FROM annotation_type_deps d
        JOIN annotation_types t1 ON t1.id = d.annotation_type_id
        JOIN annotation_types t2 ON t2.id = d.depends_on_id
        WHERE t1.type_id = 'quality.session_outcome'
          AND t2.type_id = 'metadata.session_scope'
          AND d.required = 0`)
	if depCount != 1 {
		t.Error("V13: expected outcome→scope dependency with required=0")
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV13Constraints — CHECK constraints enforced
// ---------------------------------------------------------------------------

// TestMigrationV13TypeIDFormat verifies the type_id LIKE '%.%' CHECK.
func TestMigrationV13TypeIDFormat(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V16: annotation_types uses UUID PK (TEXT), INT FK enum columns.
	// value_domain_type → value_domain_kind_id (1=enumerated)
	// status → status_id (2=active)
	// origin → origin_id (1=system)
	// family_id is now TEXT UUID; use subquery to look up seed family UUID.

	// type_id without dot → rejected.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, display_name, family_id, value_domain_kind_id, datatype, value_constraint, status_id, origin_id, created_at)
         VALUES (?, 'nodot', 'Bad',
             (SELECT id FROM annotation_families WHERE family='session_quality'),
             1, 'text', '[]', 2, 1, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err == nil {
		t.Error("expected CHECK (type_id LIKE '%.%') to reject type_id without dot")
	}

	// type_id with dot → accepted.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, display_name, family_id, value_domain_kind_id, datatype, value_constraint, status_id, origin_id, created_at)
         VALUES (?, 'family.valid_type', 'Valid',
             (SELECT id FROM annotation_families WHERE family='session_quality'),
             1, 'text', '[]', 2, 1, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err != nil {
		t.Errorf("type_id with dot should be accepted: %v", err)
	}
}

// TestMigrationV13AnnotatorsExclusiveArc verifies V24 and V25 CHECK constraints.
func TestMigrationV13AnnotatorsExclusiveArc(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V16: annotators uses UUID PK (TEXT id), kind_id INTEGER FK to annotator_kinds.
	// annotator_kinds: human=1, agent=2, rule=3

	// V24: Agent annotator (kind_id=2) MUST have model_id + provider_key.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotators (id, kind_id, name, display_name, status, created_at)
         VALUES (?, 2, 'agent-no-model', 'Missing model', 'active', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err == nil {
		t.Error("V24: expected CHECK to reject agent annotator without model_id/provider_key")
	}

	// Seed a model for FK validity.
	_ = sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO models (model_id, provider_key, display_name, reasoning, tool_call, last_synced)
         VALUES ('claude-opus-4-6', 'anthropic', 'Claude Opus', 1, 1, '2024-01-01')`,
		nil)

	// V25: Non-agent annotator (kind_id=1=human) MUST NOT have model_id + provider_key.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotators (id, kind_id, name, display_name, model_id, provider_key, status, created_at)
         VALUES (?, 1, 'human-with-model', 'Bad human', 'claude-opus-4-6', 'anthropic', 'active', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err == nil {
		t.Error("V25: expected CHECK to reject non-agent annotator with model_id/provider_key")
	}

	// Valid agent annotator (kind_id=2) WITH model.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotators (id, kind_id, name, display_name, model_id, provider_key, status, created_at)
         VALUES (?, 2, 'good-agent', 'Valid Agent', 'claude-opus-4-6', 'anthropic', 'active', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err != nil {
		t.Errorf("V24: valid agent annotator with model should be accepted: %v", err)
	}

	// Valid rule annotator (kind_id=3) without model.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotators (id, kind_id, name, display_name, status, created_at)
         VALUES (?, 3, 'good-rule', 'Valid Rule', 'active', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err != nil {
		t.Errorf("V25: valid rule annotator without model should be accepted: %v", err)
	}
}

// TestMigrationV13Annotations3ArmArc verifies the TPT enforcement of exclusive targeting (V9).
//
// V16 replaced the annotations table CHECK constraint with TPT (Table-per-Type) child tables.
// The exclusive-arc is enforced by the TPT design: each annotation must have exactly one
// child table row (annotation_target_sessions, annotation_target_entries,
// annotation_target_annotations, or annotation_target_projects).
//
// This test verifies:
//  1. An annotation parent row with target_kind_id=1 (session) + no child row is valid at the
//     parent level (child-table enforcement is a separate concern from schema).
//  2. An annotation parent row + session child row correctly links to the session.
//  3. An annotation-level target via annotation_target_annotations is accepted.
func TestMigrationV13Annotations3ArmArc(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV13(t, ctx, s, v13TestSID1)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// V16: annotations.target_kind_id is NOT NULL — omitting it is rejected.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String(), annotatorID, typeID}})
	if err == nil {
		t.Error("V9: expected NOT NULL on target_kind_id to reject annotation with no target_kind_id")
	}

	// Insert a valid session-arm annotation via TPT parent + child.
	annID1 := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, 1, ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID1, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert session-arm annotation parent: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{annID1, v13TestSID1}}); err != nil {
		t.Fatalf("insert annotation_target_sessions child: %v", err)
	}

	// Insert a second valid session-arm annotation (partial value).
	annID2 := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, 1, ?, ?, 'partial', 1700000000002)`,
		&sqlitex.ExecOptions{Args: []any{annID2, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert second session-arm annotation parent: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{annID2, v13TestSID1}}); err != nil {
		t.Fatalf("insert second annotation_target_sessions child: %v", err)
	}

	// V9: Annotation-arm (target_kind_id=3) referencing annID1 → accepted.
	annID3 := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, 3, ?, ?, 'resolved', 1700000000001)`,
		&sqlitex.ExecOptions{Args: []any{annID3, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert annotation-arm parent: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_annotations (annotation_id, target_annotation_id) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{annID3, annID1}}); err != nil {
		t.Fatalf("insert annotation_target_annotations child: %v", err)
	}
}

// TestMigrationV13EntryLevelArmBothRequired verifies V10: entry arm requires both columns.
//
// V16: entry-arm annotations use the annotation_target_entries child table which has
// session_id, entry_index, and end_index (half-open [start, end) span).
// The CHECK (end_index > entry_index) enforces that spans have at least one entry.
func TestMigrationV13EntryLevelArmBothRequired(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV13(t, ctx, s, v13TestSID2)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Seed a session_entries row for the FK.
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role)
         VALUES (?, 0, 'claude', 'text', 'assistant')`,
		&sqlitex.ExecOptions{Args: []any{v13TestSID2}}); err != nil {
		t.Fatalf("insert session_entry: %v", err)
	}

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// V10: end_index must be > entry_index. end_index = entry_index → rejected.
	annID1 := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, 2, ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID1, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert entry-arm annotation parent: %v", err)
	}
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_entries (annotation_id, session_id, entry_index, end_index)
         VALUES (?, ?, 0, 0)`,
		&sqlitex.ExecOptions{Args: []any{annID1, v13TestSID2}})
	if err == nil {
		t.Error("V10: expected CHECK (end_index > entry_index) to reject end_index=entry_index=0")
	}

	// V10: end_index > entry_index → accepted.
	annID2 := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, 2, ?, ?, 'resolved', 1700000000001)`,
		&sqlitex.ExecOptions{Args: []any{annID2, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert second entry-arm annotation parent: %v", err)
	}
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_entries (annotation_id, session_id, entry_index, end_index)
         VALUES (?, ?, 0, 1)`,
		&sqlitex.ExecOptions{Args: []any{annID2, v13TestSID2}})
	if err != nil {
		t.Errorf("V10: valid entry-level annotation with entry_index=0, end_index=1 should be accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV13Indexes — all annotation indexes created
// ---------------------------------------------------------------------------

// TestMigrationV13Indexes verifies all V16 annotation indexes exist.
//
// V16 replaced V13 index names. Old names → new names:
//   - idx_ann_session  → idx_ann_target_session  (on annotation_target_sessions)
//   - idx_ann_entry    → idx_ann_target_entry    (on annotation_target_entries)
//   - idx_ann_meta     → idx_ann_target_annot    (on annotation_target_annotations)
//   - idx_ann_effective → idx_ann_effective      (still present on annotations, updated columns)
//
// Additional V16 indexes: idx_ann_content_hash, idx_ann_created_at, idx_ann_target_project.
func TestMigrationV13Indexes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V16 annotation indexes (supersede V13 index names).
	expectedIndexes := []string{
		"idx_annfam_class_id",
		"idx_anntype_status",
		"idx_anntype_family_id",
		"idx_anntype_deps_reverse",
		"idx_annotator_kind",
		"idx_annotator_model",
		"idx_ann_type_id",
		"idx_ann_content_hash",
		"idx_ann_created_at",
		"idx_ann_target_session",
		"idx_ann_target_entry",
		"idx_ann_target_annot",
		"idx_ann_target_project",
	}
	for _, idx := range expectedIndexes {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx)
		if count != 1 {
			t.Errorf("index %q not found after migration V16", idx)
		}
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV13View — view returns target_kind, annotator_kind, class (V11)
// ---------------------------------------------------------------------------

// TestMigrationV13ViewColumns verifies annotations_with_target view (V11).
//
// V16: annotations use TPT pattern. INSERT goes to parent annotations table
// (with target_kind_id) and child annotation_target_sessions table.
// View still exposes target_session_id via LEFT JOIN on child table.
func TestMigrationV13ViewColumns(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV13(t, ctx, s, v13TestSID1)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// V16: Insert parent annotation with target_kind_id=1 (session).
	annID := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, 1, ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert annotation parent: %v", err)
	}
	// V16: Insert TPT child row.
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{annID, v13TestSID1}}); err != nil {
		t.Fatalf("insert annotation_target_sessions: %v", err)
	}

	// V11: Verify view returns target_kind, annotator_kind, class.
	var targetKind, annotatorKind, class string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT target_kind, annotator_kind, class FROM annotations_with_target WHERE target_session_id=?`,
		&sqlitex.ExecOptions{
			Args: []any{v13TestSID1},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				targetKind = stmt.ColumnText(0)
				annotatorKind = stmt.ColumnText(1)
				class = stmt.ColumnText(2)
				return nil
			},
		}); err != nil {
		t.Fatalf("query annotations_with_target: %v", err)
	}

	if targetKind != "session" {
		t.Errorf("V11: target_kind: expected %q, got %q", "session", targetKind)
	}
	if annotatorKind != "rule" {
		t.Errorf("V11: annotator_kind: expected %q, got %q", "rule", annotatorKind)
	}
	if class != "quality" {
		t.Errorf("V11: class: expected %q, got %q", "quality", class)
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV13NoUnixepoch — no unixepoch() in V13 DDL (V19)
// ---------------------------------------------------------------------------

// TestMigrationV13NoUnixepoch verifies V19: V13 DDL does not use unixepoch().
func TestMigrationV13NoUnixepoch(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	v13Objects := []string{
		"annotation_classes",
		"annotation_families",
		"annotation_types",
		"annotation_type_deps",
		"annotators",
		"annotations",
		"annotations_with_target",
	}
	for _, obj := range v13Objects {
		count := queryInt(t, conn,
			`SELECT COUNT(*) FROM sqlite_master WHERE name=? AND sql LIKE '%unixepoch%'`, obj)
		if count != 0 {
			t.Errorf("V19: %q DDL contains 'unixepoch()'; use CAST(strftime('%%s','now') AS INTEGER)*1000 instead", obj)
		}
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV13STRICTMode — STRICT modifier on all 6 tables (V18)
// ---------------------------------------------------------------------------

// TestMigrationV13STRICTMode verifies V18: all 6 V13 tables use STRICT mode.
func TestMigrationV13STRICTMode(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	strictTables := []string{
		"annotation_classes",
		"annotation_families",
		"annotation_types",
		"annotation_type_deps",
		"annotators",
		"annotations",
	}
	for _, tbl := range strictTables {
		// STRICT tables have ') STRICT' in their DDL.
		count := queryInt(t, conn,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=? AND sql LIKE '%) STRICT%'`, tbl)
		if count != 1 {
			t.Errorf("V18: table %q does not use STRICT mode", tbl)
		}
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV13LowerIsBetter — nullable CHECK IN (0,1) (V12)
// ---------------------------------------------------------------------------

// TestMigrationV13LowerIsBetter verifies lower_is_better only accepts NULL, 0, or 1 (V12).
//
// V16: annotation_types uses INT FK enum columns.
// value_domain_type → value_domain_kind_id (1=enumerated, 2=described)
// status → status_id (1=proposed, 2=active, 3=deprecated, 4=retired)
// origin → origin_id (1=system, 2=user, 3=group)
// family_id is now TEXT UUID; resolved via subquery.
func TestMigrationV13LowerIsBetter(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// lower_is_better=2 → rejected.
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, display_name, family_id, value_domain_kind_id, datatype, value_constraint, lower_is_better, status_id, origin_id, created_at)
         VALUES (?, 'family.invalid_lib', 'Test',
             (SELECT id FROM annotation_families WHERE family='session_quality'),
             1, 'text', '[]', 2, 2, 1, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err == nil {
		t.Error("V12: expected CHECK to reject lower_is_better=2 (only NULL, 0, 1 allowed)")
	}

	// lower_is_better=0 → accepted.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, display_name, family_id, value_domain_kind_id, datatype, value_constraint, lower_is_better, status_id, origin_id, created_at)
         VALUES (?, 'family.lower_zero', 'Test Zero',
             (SELECT id FROM annotation_families WHERE family='session_quality'),
             1, 'text', '[]', 0, 2, 1, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err != nil {
		t.Errorf("V12: lower_is_better=0 should be accepted: %v", err)
	}

	// lower_is_better=1 → accepted.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, display_name, family_id, value_domain_kind_id, datatype, value_constraint, lower_is_better, status_id, origin_id, created_at)
         VALUES (?, 'family.lower_one', 'Test One',
             (SELECT id FROM annotation_families WHERE family='session_quality'),
             1, 'text', '[]', 1, 2, 1, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err != nil {
		t.Errorf("V12: lower_is_better=1 should be accepted: %v", err)
	}

	// lower_is_better=NULL → accepted.
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_types
         (id, type_id, display_name, family_id, value_domain_kind_id, datatype, value_constraint, lower_is_better, status_id, origin_id, created_at)
         VALUES (?, 'family.lower_null', 'Test Null',
             (SELECT id FROM annotation_families WHERE family='session_quality'),
             1, 'text', '[]', NULL, 2, 1, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{uuid.New().String()}})
	if err != nil {
		t.Errorf("V12: lower_is_better=NULL should be accepted: %v", err)
	}
}

// TestMigrationV13ProvenanceNullable verifies annotations.provenance is nullable TEXT (V13).
//
// V16: annotations table uses TPT pattern. INSERT into parent annotations
// (with target_kind_id) and child annotation_target_sessions.
func TestMigrationV13ProvenanceNullable(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV13(t, ctx, s, v13TestSID1)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify provenance column is nullable (notnull=0).
	nullable := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('annotations') WHERE name='provenance' AND "notnull"=0`)
	if nullable != 1 {
		t.Error("V13: annotations.provenance should be nullable (notnull=0)")
	}

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// provenance=NULL → accepted.
	annID1 := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, provenance, created_at)
         VALUES (?, 1, ?, ?, 'resolved', NULL, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID1, annotatorID, typeID}}); err != nil {
		t.Errorf("V13: provenance=NULL should be accepted: %v", err)
	} else {
		if err := sqlitex.ExecuteTransient(conn,
			`INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`,
			&sqlitex.ExecOptions{Args: []any{annID1, v13TestSID1}}); err != nil {
			t.Fatalf("insert annotation_target_sessions (provenance=NULL): %v", err)
		}
	}

	// provenance=JSON string → accepted.
	annID2 := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, provenance, created_at)
         VALUES (?, 1, ?, ?, 'partial', '{"method":"heuristic","version":"v1"}', 1700000000001)`,
		&sqlitex.ExecOptions{Args: []any{annID2, annotatorID, typeID}}); err != nil {
		t.Errorf("V13: provenance with JSON value should be accepted: %v", err)
	} else {
		if err := sqlitex.ExecuteTransient(conn,
			`INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`,
			&sqlitex.ExecOptions{Args: []any{annID2, v13TestSID1}}); err != nil {
			t.Fatalf("insert annotation_target_sessions (provenance=JSON): %v", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// seedTestSessionV13 creates the FK prerequisite session row for V13 tests.
func seedTestSessionV13(t *testing.T, ctx context.Context, s *store.Store, sessionID string) {
	t.Helper()

	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	// V23+: host_slugs(opaque_id, host_slug, ...); sessions uses opaque_host_id.
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO projects (project_hash, canonical_cwd) VALUES ('v13projhashaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','/v13proj')`, nil); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const v13SlugOpaqueID = "13aa13bb13cc13dd13ee13ff1313131313aa13bb13cc13dd13ee13ff13131313"
	if err := sqlitex.ExecuteTransient(conn, `INSERT OR IGNORE INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+v13SlugOpaqueID+`','v13slug','git@v13test')`, nil); err != nil {
		t.Fatalf("insert host_slug: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO sessions (session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format)
         VALUES (?, 'claude-code','claude-opus-4-6','`+v13SlugOpaqueID+`','v13projhashaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',1,2,3,'/f','jsonl')`,
		&sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		t.Fatalf("insert session %s: %v", sessionID, err)
	}
}
