package metrics_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/metrics"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestClassifierCapturedCompletionAndRetirement(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(filepath.Join(t.TempDir(), "classifiers.db"), store.WithPoolSize(1))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sid := mustSessionID(t, testutil.TestSessionUUID)
	seedSession(t, ctx, db, string(sid))
	var entries []schema.SessionEntry
	for _, row := range loadTitleFixture(t).Cases {
		if row.Name != "nested_user_is_not_title" {
			continue
		}
		for index, entry := range row.Entries {
			preview := entry.Preview
			entries = append(entries, schema.SessionEntry{SessionID: sid, Harness: row.Harness, EntryIndex: index, EntryType: ingest.EntryTypeText, Role: entry.Role, Depth: entry.Depth, ContentPreview: &preview})
		}
	}
	if len(entries) == 0 {
		t.Fatal("existing title fixture is missing")
	}
	if err := db.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatal(err)
	}
	engine := metrics.NewEngineWithModels(db, db)
	if _, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid}); err != nil {
		t.Fatal(err)
	}
	classifier := metrics.NewClassifierAnnotator(db, db)
	batch, err := classifier.PrepareAnnotations(ctx, sid, nil)
	if err != nil || batch.Input == nil || batch.RunState == nil || len(batch.Writes) == 0 || len(batch.Owners) == 0 {
		t.Fatalf("prepare captured output: %+v %v", batch, err)
	}
	if err := classifier.Annotate(ctx, sid); err != nil {
		t.Fatal(err)
	}
	current, err := classifier.PrepareAnnotations(ctx, sid, nil)
	if err != nil || !current.Skipped {
		t.Fatalf("equal input did not skip: %+v %v", current, err)
	}
	rows, err := db.GetAnnotationsForSession(ctx, string(sid))
	if err != nil || len(rows) == 0 {
		t.Fatalf("initial outputs unavailable: %v", err)
	}
	priorID := rows[0].ID
	userID, err := db.CreateAnnotator(ctx, store.CreateAnnotatorParams{Kind: schema.AnnotatorHuman, Name: "fixture-reviewer", DisplayName: "fixture reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	approvalID, err := db.GetAnnotationTypeID(ctx, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatal(err)
	}
	dependencyID, err := db.CreateAnnotation(ctx, store.CreateAnnotationParams{AnnotationID: &priorID, AnnotatorID: userID, AnnotationTypeID: approvalID, Value: "approve"})
	if err != nil {
		t.Fatal(err)
	}
	entries[len(entries)-1].IsError = true
	if err := db.IndexSessionEntries(ctx, sid, entries); err != nil {
		t.Fatal(err)
	}
	if _, err := classifier.PrepareAnnotations(ctx, sid, nil); err == nil {
		t.Fatal("classifier accepted metrics for obsolete entries")
	}
	stale := db.ApplyClassifierAnnotationBatches(ctx, []ingest.SessionAnnotationBatch{batch})
	if len(stale) != 1 || stale[0].Err == nil {
		t.Fatalf("stale captured write was accepted: %+v", stale)
	}
	if _, err := engine.ComputeMetrics(ctx, []ingest.SessionID{sid}); err != nil {
		t.Fatal(err)
	}
	changed, err := classifier.PrepareAnnotations(ctx, sid, nil)
	if err != nil || changed.Skipped || changed.RunState == nil || changed.RunState.MetricsOutputHash == batch.RunState.MetricsOutputHash {
		t.Fatalf("changed same-version metrics did not invalidate classification: %+v %v", changed, err)
	}
	// Exercise a complete now-empty configured pass through the actual writer.
	changed.Writes = nil
	empty := db.ApplyClassifierAnnotationBatches(ctx, []ingest.SessionAnnotationBatch{changed})
	if len(empty) != 1 || empty[0].Err != nil {
		t.Fatalf("empty output retirement failed: %+v", empty)
	}
	rows, err = db.GetAnnotationsForSession(ctx, string(sid))
	if err != nil || len(rows) != 0 {
		t.Fatalf("retired output remained current: %+v %v", rows, err)
	}
	retracted, err := db.ListSupersededAnnotations(ctx)
	if err != nil || len(retracted) == 0 {
		t.Fatalf("retirement did not produce existing retractions: %+v %v", retracted, err)
	}
	found, err := db.FindExistingAnnotation(ctx, batch.Writes[0].Write.Find)
	if err != nil || found != nil {
		t.Fatalf("dedup reused retired output: %+v %v", found, err)
	}
	conn, err := db.Pool().Take(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var preserved bool
	err = sqlitex.ExecuteTransient(conn, `SELECT a.retired_at, a.superseded_by, v.target_session_id
FROM annotation_target_annotations d JOIN annotations a ON a.id = d.target_annotation_id
JOIN annotations_with_target v ON v.id = a.id
WHERE d.annotation_id = ? AND a.id = ?`, &sqlitex.ExecOptions{Args: []any{dependencyID, priorID}, ResultFunc: func(stmt *sqlite.Stmt) error {
		preserved = stmt.ColumnType(0) != sqlite.TypeNull && stmt.ColumnType(1) == sqlite.TypeNull && stmt.ColumnText(2) == string(sid)
		return nil
	}})
	if err != nil || !preserved {
		t.Fatalf("retirement lost historical target or user dependency: preserved=%v %v", preserved, err)
	}
}
