package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite/sqlitex"
)

const v41TestSessionID = "41000000-0000-0000-0000-000000000001"

// TestMigrationV41AssociationTarget verifies the new association annotation
// arm: its lookup seed, normalized storage, index, foreign keys, recreated
// target view, and the public exclusive-target boundary.
func TestMigrationV41AssociationTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	seedTestSession(t, ctx, s, v41TestSessionID)
	sessionID := ingest.SessionID(v41TestSessionID)
	if err := s.UpsertSessionCommits(ctx, sessionID, []ingest.CommitInfo{{
		Hash:       "v41-observed-commit",
		Message:    "preserve association target",
		AuthorTime: 1700000000000,
	}}); err != nil {
		t.Fatalf("UpsertSessionCommits: %v", err)
	}

	associations, err := s.ListCurrentSessionCommitAssociations(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListCurrentSessionCommitAssociations: %v", err)
	}
	if len(associations) != 1 {
		t.Fatalf("ListCurrentSessionCommitAssociations returned %d rows, want 1", len(associations))
	}
	associationID := associations[0].ID

	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	if got := queryInt(t, conn, `SELECT COUNT(*) FROM target_kinds WHERE id = 5 AND name = 'association'`); got != 1 {
		t.Errorf("association target kind: got %d rows, want 1", got)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM target_kinds`); got != 5 {
		t.Errorf("target kind count after V41: got %d, want 5", got)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'annotation_target_associations'`); got != 1 {
		t.Errorf("annotation_target_associations table: got %d, want 1", got)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_ann_target_association'`); got != 1 {
		t.Errorf("association target index: got %d, want 1", got)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_foreign_key_list('annotation_target_associations') WHERE "from" = 'annotation_id' AND "table" = 'annotations'`); got != 1 {
		t.Errorf("annotation target annotation FK: got %d rows, want 1", got)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_foreign_key_list('annotation_target_associations') WHERE "from" = 'association_id' AND "table" = 'session_commit_associations'`); got != 1 {
		t.Errorf("annotation target association FK: got %d rows, want 1", got)
	}
	if got := queryInt(t, conn, `SELECT COUNT(*) FROM pragma_table_info('annotations_with_target') WHERE name = 'target_association_id'`); got != 1 {
		t.Errorf("annotations_with_target.target_association_id: got %d columns, want 1", got)
	}

	typeID := queryText(t, conn, `SELECT id FROM annotation_types WHERE type_id = 'quality.session_outcome'`)
	annotatorID := queryText(t, conn, `SELECT id FROM annotators WHERE name = 'outcome-classifier'`)
	annotationID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AssociationID:    &associationID,
		AnnotationTypeID: typeID,
		AnnotatorID:      annotatorID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(association target): %v", err)
	}

	rows, err := s.GetAssociationAnnotationsForSession(ctx, v41TestSessionID)
	if err != nil {
		t.Fatalf("GetAssociationAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("GetAssociationAnnotationsForSession returned %d rows, want 1", len(rows))
	}
	if rows[0].ID != annotationID {
		t.Errorf("association annotation ID = %q, want %q", rows[0].ID, annotationID)
	}
	if rows[0].TargetKind != schema.TargetAssociation {
		t.Errorf("association annotation target kind = %q, want %q", rows[0].TargetKind, schema.TargetAssociation)
	}
	if rows[0].TargetAssociationID == nil || *rows[0].TargetAssociationID != associationID {
		t.Errorf("association annotation target ID = %v, want %q", rows[0].TargetAssociationID, associationID)
	}
	if rows[0].TargetSessionID != nil || rows[0].TargetEntryIndex != nil || rows[0].TargetAnnotID != nil || rows[0].TargetProjectHash != nil {
		t.Errorf("association annotation leaked another target arm: %+v", rows[0])
	}

	sessionTarget := v41TestSessionID
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionTarget,
		AssociationID:    &associationID,
		AnnotationTypeID: typeID,
		AnnotatorID:      annotatorID,
		Value:            "resolved",
	}); err == nil || !strings.Contains(err.Error(), "exactly one target") {
		t.Errorf("CreateAnnotation with session and association targets = %v, want exclusive-target error", err)
	}

	invalidAnnotationID := uuid.NewString()
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotations (
		id, target_kind_id, annotation_type_id, annotator_id, value, is_primary, created_at
	) VALUES (?, (SELECT id FROM target_kinds WHERE name = 'association'), ?, ?, 'resolved', 0, 1700000000000)`, &sqlitex.ExecOptions{
		Args: []any{invalidAnnotationID, typeID, annotatorID},
	}); err != nil {
		t.Fatalf("insert association annotation parent: %v", err)
	}
	invalidAssociationID := schema.AssociationID("assoc-00000000-0000-0000-0000-000000000041")
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO annotation_target_associations (annotation_id, association_id) VALUES (?, ?)`, &sqlitex.ExecOptions{
		Args: []any{invalidAnnotationID, invalidAssociationID.String()},
	}); err == nil {
		t.Error("association target accepted a missing durable association, want foreign-key failure")
	}
}
