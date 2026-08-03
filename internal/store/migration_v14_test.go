package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// v14 test session/project IDs.
const (
	v14TestSID1     = "c1400000-0000-0000-0000-000000000001"
	v14TestSID2     = "c1400000-0000-0000-0000-000000000002"
	v14TestProjHash = "v14projhashaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// ---------------------------------------------------------------------------
// TestMigrationV16Applies — schema structure + user_version
// ---------------------------------------------------------------------------

// TestMigrationV16Applies verifies that V16 migration applies cleanly:
//   - annotations table has V16 columns (id TEXT, target_kind_id, is_primary)
//   - annotation_types.priority_override column exists
//   - V16 TPT child tables exist
//   - INT lookup tables exist
//   - user_version = 17 (V17 also applied)
func TestMigrationV16Applies(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V16 columns on annotations.
	for _, col := range []string{"target_kind_id", "is_primary", "content_hash"} {
		count := queryInt(t, conn,
			`SELECT COUNT(*) FROM pragma_table_info('annotations') WHERE name=?`, col)
		if count != 1 {
			t.Errorf("annotations.%s column not found after V16 migration", col)
		}
	}

	// V16 column on annotation_types.
	count := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('annotation_types') WHERE name='priority_override'`)
	if count != 1 {
		t.Error("annotation_types.priority_override column not found after V16 migration")
	}

	// V16 TPT child tables.
	for _, tbl := range []string{
		"annotation_target_sessions",
		"annotation_target_entries",
		"annotation_target_annotations",
		"annotation_target_projects",
	} {
		count := queryInt(t, conn,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl)
		if count != 1 {
			t.Errorf("TPT child table %q not found after V16 migration", tbl)
		}
	}

	// V16 INT lookup tables.
	for _, tbl := range []string{
		"target_kinds",
		"annotator_kinds",
		"annotation_statuses",
		"value_domain_kinds",
		"type_origins",
	} {
		count := queryInt(t, conn,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl)
		if count != 1 {
			t.Errorf("INT lookup table %q not found after V16 migration", tbl)
		}
	}

	// Verify user_version >= 14 (V14 migration and all subsequent applied).
	uv := queryInt(t, conn, `PRAGMA user_version`)
	if uv < 14 {
		t.Errorf("user_version: expected >= 14, got %d", uv)
	}
}

// TestMigrationV16_AllAnnotationIndexes verifies annotation indexes exist after V16.
func TestMigrationV16_AllAnnotationIndexes(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V16 indexes on parent and child tables (v16CreateAnnotationsIndexes).
	// NOTE: idx_ann_effective was dropped in V16; it does not exist here.
	expectedIndexes := []string{
		"idx_ann_type_id",
		"idx_ann_annotator",
		"idx_ann_target_kind",
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
			t.Errorf("index %q not found after V16 migration", idx)
		}
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV16_TPTTargetKind — TPT parent has target_kind_id FK
// ---------------------------------------------------------------------------

// TestMigrationV16_TPTProjectTarget verifies that a project-level annotation
// can be created via the TPT parent + child pattern.
func TestMigrationV16_TPTProjectTarget(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV14(t, ctx, s, v14TestSID1)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// V16 TPT: insert parent + child.
	annID := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, (SELECT id FROM target_kinds WHERE name='project'), ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert parent annotation: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_projects (annotation_id, project_hash) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{annID, v14TestProjHash}}); err != nil {
		t.Fatalf("insert TPT child: %v", err)
	}

	// Verify via view.
	var targetKind string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT target_kind FROM annotations_with_target WHERE target_project_hash=?`,
		&sqlitex.ExecOptions{
			Args: []any{v14TestProjHash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				targetKind = stmt.ColumnText(0)
				return nil
			},
		}); err != nil {
		t.Fatalf("query annotations_with_target: %v", err)
	}
	if targetKind != string(schema.TargetProject) {
		t.Errorf("target_kind: expected %q, got %q", schema.TargetProject, targetKind)
	}
}

// TestMigrationV16_TPTMissingTargetKindRejected verifies that annotations parent
// table requires target_kind_id (NOT NULL constraint).
func TestMigrationV16_TPTMissingTargetKindRejected(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// Missing target_kind_id → should fail (NOT NULL).
	annID := uuid.New().String()
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID, annotatorID, typeID}})
	if err == nil {
		t.Error("annotations INSERT without target_kind_id should be rejected (NOT NULL)")
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV16_IsPrimary — is_primary column
// ---------------------------------------------------------------------------

// TestMigrationV16_IsPrimaryDefaultZero verifies that is_primary defaults to 0.
func TestMigrationV16_IsPrimaryDefaultZero(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV14(t, ctx, s, v14TestSID1)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// V16: insert via parent + child, without setting is_primary.
	annID := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, (SELECT id FROM target_kinds WHERE name='session'), ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert annotation without is_primary: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{annID, v14TestSID1}}); err != nil {
		t.Fatalf("insert TPT child: %v", err)
	}

	isPrimary := queryInt(t, conn, `SELECT is_primary FROM annotations WHERE id=?`, annID)
	if isPrimary != 0 {
		t.Errorf("is_primary: expected default 0, got %d", isPrimary)
	}
}

// TestMigrationV16_IsPrimaryCheckConstraint verifies is_primary only accepts 0 or 1.
func TestMigrationV16_IsPrimaryCheckConstraint(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV14(t, ctx, s, v14TestSID2)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	// is_primary=2 → rejected.
	annID1 := uuid.New().String()
	err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, is_primary, created_at)
         VALUES (?, (SELECT id FROM target_kinds WHERE name='session'), ?, ?, 'resolved', 2, 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID1, annotatorID, typeID}})
	if err == nil {
		t.Error("is_primary=2 should be rejected by CHECK (only 0 or 1 allowed)")
	}

	// is_primary=1 → accepted.
	annID2 := uuid.New().String()
	err = sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, is_primary, created_at)
         VALUES (?, (SELECT id FROM target_kinds WHERE name='session'), ?, ?, 'resolved', 1, 1700000000001)`,
		&sqlitex.ExecOptions{Args: []any{annID2, annotatorID, typeID}})
	if err != nil {
		t.Errorf("is_primary=1 should be accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV16_PriorityOverride — priority_override column
// ---------------------------------------------------------------------------

// TestMigrationV16_PriorityOverrideNullableByDefault verifies that
// priority_override is nullable and NULL for all seed types.
func TestMigrationV16_PriorityOverrideNullableByDefault(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// All seed types should have NULL priority_override.
	nonNullCount := queryInt(t, conn,
		`SELECT COUNT(*) FROM annotation_types WHERE priority_override IS NOT NULL`)
	if nonNullCount != 0 {
		t.Errorf("expected all seed annotation_types to have NULL priority_override, got %d non-null", nonNullCount)
	}

	// Column accepts NULL.
	nullable := queryInt(t, conn,
		`SELECT COUNT(*) FROM pragma_table_info('annotation_types') WHERE name='priority_override' AND "notnull"=0`)
	if nullable != 1 {
		t.Error("annotation_types.priority_override should be nullable")
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV16_ViewProjectArm — view returns target_kind='project'
// ---------------------------------------------------------------------------

// TestMigrationV16_ViewProjectArm verifies that annotations_with_target returns
// target_kind='project' for annotations targeting a project via TPT.
func TestMigrationV16_ViewProjectArm(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV14(t, ctx, s, v14TestSID1)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name='outcome-classifier'`)

	annID := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotations (id, target_kind_id, annotator_id, annotation_type_id, value, created_at)
         VALUES (?, (SELECT id FROM target_kinds WHERE name='project'), ?, ?, 'resolved', 1700000000000)`,
		&sqlitex.ExecOptions{Args: []any{annID, annotatorID, typeID}}); err != nil {
		t.Fatalf("insert parent annotation: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO annotation_target_projects (annotation_id, project_hash) VALUES (?, ?)`,
		&sqlitex.ExecOptions{Args: []any{annID, v14TestProjHash}}); err != nil {
		t.Fatalf("insert TPT child: %v", err)
	}

	var targetKind string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT target_kind FROM annotations_with_target WHERE target_project_hash=?`,
		&sqlitex.ExecOptions{
			Args: []any{v14TestProjHash},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				targetKind = stmt.ColumnText(0)
				return nil
			},
		}); err != nil {
		t.Fatalf("query annotations_with_target: %v", err)
	}

	if targetKind != string(schema.TargetProject) {
		t.Errorf("target_kind: expected %q, got %q", schema.TargetProject, targetKind)
	}
}

// ---------------------------------------------------------------------------
// TestMigrationV16_ExistingDataPreserved — V16 API creates annotations correctly
// ---------------------------------------------------------------------------

// TestMigrationV16_SessionAnnotationViaAPI verifies that the V16 CreateAnnotation
// API correctly creates session-arm annotations with TPT pattern.
func TestMigrationV16_SessionAnnotationViaAPI(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	seedTestSessionV14(t, ctx, s, v14TestSID1)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	sid := v14TestSID1
	id, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(session arm): %v", err)
	}
	if id == "" {
		t.Fatal("CreateAnnotation(session arm): expected non-empty ID")
	}

	rows, err := s.GetAnnotationsForSession(ctx, v14TestSID1)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].IsPrimary {
		t.Error("IsPrimary: expected false (default), got true")
	}
	if rows[0].TargetProjectHash != nil {
		t.Errorf("TargetProjectHash: expected nil, got %q", *rows[0].TargetProjectHash)
	}
}

// ---------------------------------------------------------------------------
// V16: INT lookup tables — seed data verification
// ---------------------------------------------------------------------------

// TestMigrationV16_LookupTablesSeeded verifies the V16 seed rows remain present.
// Later migrations may extend a lookup table, so this test proves V16's target
// kinds are a subset of the final database rather than freezing that database.
func TestMigrationV16_LookupTablesSeeded(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// V16 seeded exactly these four names. Their unique lookup constraint means a
	// count of four within this named subset proves every V16 target kind exists,
	// while allowing later migrations to add new target kinds.
	if count := queryInt(t, conn, `SELECT COUNT(*) FROM target_kinds
		WHERE name IN ('session', 'entry', 'annotation', 'project')`); count != 4 {
		t.Errorf("V16 target-kind seed subset: expected all 4 names, got %d", count)
	}

	tests := []struct {
		table    string
		expected int
	}{
		{"annotator_kinds", 3},     // rule, agent, human
		{"annotation_statuses", 4}, // proposed, active, deprecated, retired
		{"value_domain_kinds", 2},  // enumerated, described
		{"type_origins", 3},        // system, user, group
	}
	for _, tc := range tests {
		count := queryInt(t, conn, `SELECT COUNT(*) FROM `+tc.table)
		if count != tc.expected {
			t.Errorf("%s: expected %d rows, got %d", tc.table, tc.expected, count)
		}
	}
}

// TestMigrationV16_UUIDPKs verifies that seed entities have TEXT UUID primary keys.
func TestMigrationV16_UUIDPKs(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	// Verify annotation_types have UUID PKs (36 chars).
	typeUUID := queryText(t, conn,
		`SELECT id FROM annotation_types WHERE type_id='quality.session_outcome'`)
	if len(typeUUID) != 36 {
		t.Errorf("annotation_types.id expected UUID (36 chars), got %q (%d chars)", typeUUID, len(typeUUID))
	}

	// Verify annotators have UUID PKs.
	annotatorUUID := queryText(t, conn,
		`SELECT id FROM annotators WHERE name='outcome-classifier'`)
	if len(annotatorUUID) != 36 {
		t.Errorf("annotators.id expected UUID (36 chars), got %q (%d chars)", annotatorUUID, len(annotatorUUID))
	}

	// Verify annotation_families have UUID PKs.
	familyUUID := queryText(t, conn,
		`SELECT id FROM annotation_families LIMIT 1`)
	if len(familyUUID) != 36 {
		t.Errorf("annotation_families.id expected UUID (36 chars), got %q (%d chars)", familyUUID, len(familyUUID))
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// seedTestSessionV14 seeds the FK prerequisite rows (project, host_slug, session)
// for V14/V16 tests, including the test project hash.
func seedTestSessionV14(t *testing.T, ctx context.Context, s *store.Store, sessionID string) {
	t.Helper()

	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	// V23+: host_slugs(opaque_id, host_slug, ...); sessions uses opaque_host_id.
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO projects (project_hash, canonical_cwd) VALUES (?, '/v14proj')`,
		&sqlitex.ExecOptions{Args: []any{v14TestProjHash}}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	const v14SlugOpaqueID = "14aa14bb14cc14dd14ee14ff1414141414aa14bb14cc14dd14ee14ff14141414"
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO host_slugs (opaque_id, host_slug, git_remote) VALUES ('`+v14SlugOpaqueID+`','v14slug','git@v14test')`, nil); err != nil {
		t.Fatalf("insert host_slug: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO sessions (session_id,model_harness,model_id,opaque_host_id,project_hash,start_ms,end_ms,ingested_ms,source_path,source_format)
         VALUES (?, 'claude-code','claude-opus-4-6','`+v14SlugOpaqueID+`',?,1,2,3,'/f','jsonl')`,
		&sqlitex.ExecOptions{Args: []any{sessionID, v14TestProjHash}}); err != nil {
		t.Fatalf("insert session %s: %v", sessionID, err)
	}
}
