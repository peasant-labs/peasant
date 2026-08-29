package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// ---------------------------------------------------------------------------
// Annotator CRUD
// ---------------------------------------------------------------------------

// TestCreateAnnotator_Rule verifies inserting a rule-based annotator.
func TestCreateAnnotator_Rule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	id, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorRule,
		Name:        "test-rule-classifier",
		DisplayName: "Test Rule Classifier",
		Description: "Created in tests",
	})
	if err != nil {
		t.Fatalf("CreateAnnotator: %v", err)
	}
	if id == "" {
		t.Error("CreateAnnotator: expected non-empty ID")
	}
}

// TestGetAnnotator_ByName verifies round-trip of annotator name lookup.
func TestGetAnnotator_ByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	_, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorRule,
		Name:        "round-trip-annotator",
		DisplayName: "Round Trip",
	})
	if err != nil {
		t.Fatalf("CreateAnnotator: %v", err)
	}

	row, err := s.GetAnnotator(ctx, "round-trip-annotator")
	if err != nil {
		t.Fatalf("GetAnnotator: %v", err)
	}
	if row == nil {
		t.Fatal("GetAnnotator: got nil, expected row")
	}
	if row.Name != "round-trip-annotator" {
		t.Errorf("GetAnnotator: Name = %q, want %q", row.Name, "round-trip-annotator")
	}
	if row.Kind != schema.AnnotatorRule {
		t.Errorf("GetAnnotator: Kind = %q, want %q", row.Kind, schema.AnnotatorRule)
	}
}

// TestGetAnnotator_NotFound returns nil for unknown name.
func TestGetAnnotator_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	row, err := s.GetAnnotator(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("GetAnnotator(unknown): unexpected error: %v", err)
	}
	if row != nil {
		t.Errorf("GetAnnotator(unknown): expected nil, got %+v", row)
	}
}

// TestListAnnotators_SeedAnnotators verifies the 4 seed annotators from V13+V15.
// V13 seeds 3 rule-based annotators; V15 adds 1 human annotator ("human-web");
// V18 adds 2 entry-level rule-based annotators.
func TestListAnnotators_SeedAnnotators(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	rows, err := s.ListAnnotators(ctx)
	if err != nil {
		t.Fatalf("ListAnnotators: %v", err)
	}
	if len(rows) != 6 {
		t.Errorf("ListAnnotators: expected 6 seed annotators, got %d", len(rows))
	}

	// Build lookup maps.
	nameSet := make(map[string]bool)
	kindByName := make(map[string]schema.AnnotatorKind)
	for _, row := range rows {
		nameSet[row.Name] = true
		kindByName[row.Name] = row.Kind
	}

	// Verify V13 rule-based annotators.
	for _, expected := range []string{"outcome-classifier", "frustration-classifier", "scope-classifier"} {
		if !nameSet[expected] {
			t.Errorf("ListAnnotators: missing seed annotator %q", expected)
			continue
		}
		if kindByName[expected] != schema.AnnotatorRule {
			t.Errorf("seed annotator %q: Kind = %q, want %q", expected, kindByName[expected], schema.AnnotatorRule)
		}
	}

	// Verify V15 human annotator.
	if !nameSet["human-web"] {
		t.Error("ListAnnotators: missing seed annotator \"human-web\"")
	} else if kindByName["human-web"] != schema.AnnotatorHuman {
		t.Errorf("seed annotator \"human-web\": Kind = %q, want %q", kindByName["human-web"], schema.AnnotatorHuman)
	}
}

// ---------------------------------------------------------------------------
// CreateAnnotation
// ---------------------------------------------------------------------------

// seedAnnotatorIDForTest returns the UUID of the "outcome-classifier" seed annotator.
func seedAnnotatorIDForTest(t *testing.T, s *store.Store) string {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	var id string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT id FROM annotators WHERE name = 'outcome-classifier'`,
		&sqlitex.ExecOptions{ResultFunc: func(stmt *sqlite.Stmt) error {
			id = stmt.ColumnText(0)
			return nil
		}},
	); err != nil || id == "" {
		t.Fatalf("seedAnnotatorIDForTest: outcome-classifier not found")
	}
	return id
}

// seedAnnotationTypeIDForTest returns the UUID of the given annotation type.
func seedAnnotationTypeIDForTest(t *testing.T, s *store.Store, typeID string) string {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	var id string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT id FROM annotation_types WHERE type_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{typeID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				id = stmt.ColumnText(0)
				return nil
			},
		},
	); err != nil || id == "" {
		t.Fatalf("seedAnnotationTypeIDForTest(%q): not found", typeID)
	}
	return id
}

func seedEntryForAnnotationBatchTest(t *testing.T, ctx context.Context, s *store.Store, sessionID string, entryIndex int) {
	t.Helper()
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	defer s.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role)
         VALUES (?, ?, 'claude', 'text', 'assistant')`,
		&sqlitex.ExecOptions{Args: []any{sessionID, entryIndex}}); err != nil {
		t.Fatalf("insert session_entry %s/%d: %v", sessionID, entryIndex, err)
	}
}

func classifierSessionWrite(annotationTypeID, annotatorID, sessionID, value, hash string) ingest.ClassifierAnnotationWrite {
	sid := sessionID
	return ingest.ClassifierAnnotationWrite{
		Create: ingest.CreateAnnotationParams{
			SessionID:        &sid,
			AnnotatorID:      annotatorID,
			AnnotationTypeID: annotationTypeID,
			Value:            value,
		},
		Find: ingest.FindAnnotationParams{
			AnnotationTypeID: annotationTypeID,
			AnnotatorID:      annotatorID,
			SessionID:        &sid,
		},
		ContentHash: hash,
	}
}

func classifierEntryWrite(annotationTypeID, annotatorID, sessionID string, entryIndex int, value, hash string) ingest.ClassifierAnnotationWrite {
	sid := sessionID
	idx := entryIndex
	return ingest.ClassifierAnnotationWrite{
		Create: ingest.CreateAnnotationParams{
			EntryTarget:      &ingest.EntryTarget{SessionID: sessionID, EntryIndex: entryIndex},
			AnnotatorID:      annotatorID,
			AnnotationTypeID: annotationTypeID,
			Value:            value,
		},
		Find: ingest.FindAnnotationParams{
			AnnotationTypeID: annotationTypeID,
			AnnotatorID:      annotatorID,
			SessionID:        &sid,
			EntryIndex:       &idx,
		},
		ContentHash: hash,
	}
}

func TestApplyClassifierAnnotations_CreatesSessionAndEntryWithHashes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	sessionID := "c1305555-0000-0000-0000-000000000201"
	seedTestSessionV13(t, ctx, s, sessionID)
	seedEntryForAnnotationBatchTest(t, ctx, s, sessionID, 0)
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	results := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{
		classifierSessionWrite(typeID, annotatorID, sessionID, "resolved", "hash-session-create"),
		classifierEntryWrite(typeID, annotatorID, sessionID, 0, "resolved", "hash-entry-create"),
	})
	if len(results) != 2 {
		t.Fatalf("ApplyClassifierAnnotations results = %d, want 2", len(results))
	}
	for i, result := range results {
		if result.Err != nil {
			t.Fatalf("result %d error: %v", i, result.Err)
		}
		if result.Dedup != ingest.DedupCreate {
			t.Fatalf("result %d dedup = %s, want create", i, result.Dedup)
		}
		if result.AnnotationID == "" {
			t.Fatalf("result %d annotation ID is empty", i)
		}
	}

	sessionRows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(sessionRows) != 1 || sessionRows[0].ContentHash == nil || *sessionRows[0].ContentHash != "hash-session-create" {
		t.Fatalf("session annotation hash mismatch: %+v", sessionRows)
	}
	entryRows, err := s.GetAnnotationsForEntry(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("GetAnnotationsForEntry: %v", err)
	}
	if len(entryRows) != 1 || entryRows[0].ContentHash == nil || *entryRows[0].ContentHash != "hash-entry-create" {
		t.Fatalf("entry annotation hash mismatch: %+v", entryRows)
	}
}

func TestApplyClassifierAnnotationsWithProfile_RecordsBatchDetail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	sessionID := "c1305555-0000-0000-0000-000000000211"
	seedTestSessionV13(t, ctx, s, sessionID)
	seedEntryForAnnotationBatchTest(t, ctx, s, sessionID, 0)
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)
	var stats ingest.AnnotationProfileStats

	results := s.ApplyClassifierAnnotationsWithProfile(ctx, []ingest.ClassifierAnnotationWrite{
		classifierSessionWrite(typeID, annotatorID, sessionID, "resolved", "hash-session-profile"),
		classifierEntryWrite(typeID, annotatorID, sessionID, 0, "resolved", "hash-entry-profile"),
	}, &stats)
	for i, result := range results {
		if result.Err != nil {
			t.Fatalf("result %d error: %v", i, result.Err)
		}
	}
	if stats.BatchMutexWaitCount != 1 || stats.BatchConnectionCount != 1 || stats.BatchCommitCount != 1 {
		t.Fatalf("batch setup counters = mutex:%d connection:%d commit:%d, want 1 each", stats.BatchMutexWaitCount, stats.BatchConnectionCount, stats.BatchCommitCount)
	}
	if stats.BatchSavepointCount != 4 || stats.BatchDedupLookupCount != 2 || stats.BatchInsertParentCount != 2 || stats.BatchInsertTargetCount != 2 || stats.BatchUpdateHashCount != 2 {
		t.Fatalf("batch detail counters mismatch: %+v", stats)
	}
	if stats.BatchSupersedeCount != 0 {
		t.Fatalf("batch supersede count = %d, want 0", stats.BatchSupersedeCount)
	}
}

func TestApplyClassifierAnnotations_SkipsMatchingContentHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	sessionID := "c1305555-0000-0000-0000-000000000202"
	seedTestSessionV13(t, ctx, s, sessionID)
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	first := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{classifierSessionWrite(typeID, annotatorID, sessionID, "resolved", "same-hash")})
	if first[0].Err != nil {
		t.Fatalf("first write: %v", first[0].Err)
	}
	second := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{classifierSessionWrite(typeID, annotatorID, sessionID, "resolved", "same-hash")})
	if second[0].Err != nil {
		t.Fatalf("second write: %v", second[0].Err)
	}
	if second[0].Dedup != ingest.DedupSkip {
		t.Fatalf("second dedup = %s, want skip", second[0].Dedup)
	}
	if second[0].AnnotationID != first[0].AnnotationID {
		t.Fatalf("skip annotation ID = %q, want existing %q", second[0].AnnotationID, first[0].AnnotationID)
	}
	rows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("non-superseded rows = %d, want 1", len(rows))
	}
}

func TestApplyClassifierAnnotations_SupersedesDifferentContentHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	sessionID := "c1305555-0000-0000-0000-000000000203"
	seedTestSessionV13(t, ctx, s, sessionID)
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	first := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{classifierSessionWrite(typeID, annotatorID, sessionID, "partial", "old-hash")})
	if first[0].Err != nil {
		t.Fatalf("first write: %v", first[0].Err)
	}
	second := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{classifierSessionWrite(typeID, annotatorID, sessionID, "resolved", "new-hash")})
	if second[0].Err != nil {
		t.Fatalf("second write: %v", second[0].Err)
	}
	if second[0].Dedup != ingest.DedupSupersede {
		t.Fatalf("second dedup = %s, want supersede", second[0].Dedup)
	}
	if second[0].ExistingAnnotationID != first[0].AnnotationID {
		t.Fatalf("existing annotation ID = %q, want %q", second[0].ExistingAnnotationID, first[0].AnnotationID)
	}
	rows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 || rows[0].Value != "resolved" || rows[0].ContentHash == nil || *rows[0].ContentHash != "new-hash" {
		t.Fatalf("non-superseded annotation mismatch: %+v", rows)
	}
}

func TestApplyClassifierAnnotations_SavepointKeepsGoodWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	sessionID := "c1305555-0000-0000-0000-000000000204"
	seedTestSessionV13(t, ctx, s, sessionID)
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	bad := classifierSessionWrite(typeID, annotatorID, sessionID, "bad", "bad-hash")
	bad.Create.SessionID = nil
	results := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{
		classifierSessionWrite(typeID, annotatorID, sessionID, "first", "first-hash"),
		bad,
		classifierSessionWrite(typeID, annotatorID, sessionID, "last", "last-hash"),
	})
	if len(results) != 3 {
		t.Fatalf("results = %d, want 3", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("first write error: %v", results[0].Err)
	}
	if results[1].Err == nil {
		t.Fatal("bad write error is nil")
	}
	if results[2].Err != nil {
		t.Fatalf("last write error: %v", results[2].Err)
	}
	rows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 || rows[0].Value != "last" {
		t.Fatalf("expected last good write to supersede first after bad write was skipped; rows: %+v", rows)
	}
}

func TestApplyClassifierAnnotations_ConcurrentBatchesSerializeWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	const batchCount = 16
	sessionIDs := make([]string, batchCount)
	for i := range sessionIDs {
		sessionIDs[i] = uuid.New().String()
		seedTestSessionV13(t, ctx, s, sessionIDs[i])
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, batchCount)
	for i, sessionID := range sessionIDs {
		wg.Add(1)
		go func(i int, sessionID string) {
			defer wg.Done()
			<-start
			results := s.ApplyClassifierAnnotations(ctx, []ingest.ClassifierAnnotationWrite{
				classifierSessionWrite(typeID, annotatorID, sessionID, "resolved", sessionID+"-hash"),
			})
			if len(results) != 1 {
				errs <- fmt.Errorf("batch %d results = %d, want 1", i, len(results))
				return
			}
			if results[0].Err != nil {
				errs <- fmt.Errorf("batch %d write error: %w", i, results[0].Err)
			}
		}(i, sessionID)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, sessionID := range sessionIDs {
		rows, err := s.GetAnnotationsForSession(ctx, sessionID)
		if err != nil {
			t.Fatalf("GetAnnotationsForSession(%s): %v", sessionID, err)
		}
		if len(rows) != 1 {
			t.Fatalf("session %s annotation rows = %d, want 1", sessionID, len(rows))
		}
	}
}

// TestCreateAnnotation_SessionTarget verifies inserting a session-level annotation.
func TestCreateAnnotation_SessionTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000001"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	id, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}
	if id == "" {
		t.Error("CreateAnnotation: expected non-empty ID")
	}
}

// TestCreateAnnotation_NoTarget verifies that omitting all targets returns an error.
func TestCreateAnnotation_NoTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	_, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err == nil {
		t.Fatal("CreateAnnotation(no target): expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// GetAnnotationsForSession
// ---------------------------------------------------------------------------

// TestGetAnnotationsForSession_Empty returns empty slice when no annotations exist.
func TestGetAnnotationsForSession_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	rows, err := s.GetAnnotationsForSession(ctx, "nonexistent-session")
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// TestGetAnnotationsForSession_ReturnsAnnotations verifies basic round-trip.
func TestGetAnnotationsForSession_ReturnsAnnotations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000002"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("CreateAnnotation: %v", err)
	}

	rows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	row := rows[0]
	if row.Value != "resolved" {
		t.Errorf("Value = %q, want %q", row.Value, "resolved")
	}
	if row.TargetKind != schema.TargetSession {
		t.Errorf("TargetKind = %q, want %q", row.TargetKind, schema.TargetSession)
	}
	if row.AnnotatorKind != schema.AnnotatorRule {
		t.Errorf("AnnotatorKind = %q, want %q", row.AnnotatorKind, schema.AnnotatorRule)
	}
	if row.TypeID != testutil.TestTypeIDSessionOutcome {
		t.Errorf("TypeID = %q, want %q", row.TypeID, testutil.TestTypeIDSessionOutcome)
	}
}

// TestGetAnnotationsForSession_ExcludesSuperseded verifies that superseded annotations
// are not returned.
func TestGetAnnotationsForSession_ExcludesSuperseded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000003"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	// Create first annotation.
	firstID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "partial",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation (first): %v", err)
	}

	// Create second annotation.
	secondID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation (second): %v", err)
	}

	// Mark first as superseded by second.
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE annotations SET superseded_by = ? WHERE id = ?`,
		&sqlitex.ExecOptions{Args: []any{secondID, firstID}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("mark superseded: %v", err)
	}
	s.PoolForTest().Put(conn)

	rows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 non-superseded row, got %d", len(rows))
	}
	if rows[0].Value != "resolved" {
		t.Errorf("Value = %q, want %q (superseded should be excluded)", rows[0].Value, "resolved")
	}
}

// ---------------------------------------------------------------------------
// GetEffectiveAnnotation — priority resolution
// ---------------------------------------------------------------------------

// TestGetEffectiveAnnotation_Empty returns nil when no annotation exists.
func TestGetEffectiveAnnotation_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	result, err := s.GetEffectiveAnnotation(ctx, "nonexistent-session", testutil.TestTypeIDSessionOutcome)
	if err != nil {
		t.Fatalf("GetEffectiveAnnotation: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil, got %+v", result)
	}
}

// TestGetEffectiveAnnotation_PriorityHumanOverRule verifies that human annotations
// take priority over rule-based annotations (V16).
func TestGetEffectiveAnnotation_PriorityHumanOverRule(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000004"
	seedTestSessionV13(t, ctx, s, sessionID)

	approvalTypeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionApproval)
	ruleAnnotatorID := seedAnnotatorIDForTest(t, s)

	// Create human annotator.
	humanAnnotatorID, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorHuman,
		Name:        "test-human-reviewer",
		DisplayName: "Human Reviewer",
	})
	if err != nil {
		t.Fatalf("CreateAnnotator(human): %v", err)
	}

	// Rule says "deny".
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      ruleAnnotatorID,
		AnnotationTypeID: approvalTypeID,
		Value:            "deny",
	}); err != nil {
		t.Fatalf("CreateAnnotation(rule): %v", err)
	}

	// Human says "approve" — should win.
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      humanAnnotatorID,
		AnnotationTypeID: approvalTypeID,
		Value:            "approve",
	}); err != nil {
		t.Fatalf("CreateAnnotation(human): %v", err)
	}

	result, err := s.GetEffectiveAnnotation(ctx, sessionID, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatalf("GetEffectiveAnnotation: %v", err)
	}
	if result == nil {
		t.Fatal("GetEffectiveAnnotation: got nil, expected human annotation")
	}
	if result.Value != "approve" {
		t.Errorf("GetEffectiveAnnotation: Value = %q, want %q (human should win over rule)", result.Value, "approve")
	}
	if result.AnnotatorKind != schema.AnnotatorHuman {
		t.Errorf("GetEffectiveAnnotation: AnnotatorKind = %q, want %q", result.AnnotatorKind, schema.AnnotatorHuman)
	}
}

// ---------------------------------------------------------------------------
// GetAnnotationsForEntry
// ---------------------------------------------------------------------------

// TestGetAnnotationsForEntry_EntryLevel verifies entry-level annotation retrieval.
func TestGetAnnotationsForEntry_EntryLevel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000005"
	seedTestSessionV13(t, ctx, s, sessionID)

	// Seed a session_entries row for the FK.
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role)
         VALUES (?, 0, 'claude', 'text', 'assistant')`,
		&sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("insert session_entry: %v", err)
	}
	s.PoolForTest().Put(conn)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	entryTarget := &store.EntryTarget{SessionID: sessionID, EntryIndex: 0}
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		EntryTarget:      entryTarget,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("CreateAnnotation(entry): %v", err)
	}

	rows, err := s.GetAnnotationsForEntry(ctx, sessionID, 0)
	if err != nil {
		t.Fatalf("GetAnnotationsForEntry: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 entry-level annotation, got %d", len(rows))
	}
	if rows[0].TargetKind != schema.TargetEntry {
		t.Errorf("TargetKind = %q, want %q", rows[0].TargetKind, schema.TargetEntry)
	}
	if rows[0].Value != "resolved" {
		t.Errorf("Value = %q, want %q", rows[0].Value, "resolved")
	}
}

// TestGetEntryAnnotationsForSession_ReturnsAllEntries verifies that the
// session-scoped entry query returns every per-turn annotation for the session
// (the path that surfaces ingest-generated rule labels), and that the
// session-only GetAnnotationsForSession does NOT return them — the gap this
// query closes.
func TestGetEntryAnnotationsForSession_ReturnsAllEntries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000099"
	seedTestSessionV13(t, ctx, s, sessionID)

	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role)
         VALUES (?1, 0, 'claude', 'text', 'assistant'), (?1, 1, 'claude', 'text', 'assistant')`,
		&sqlitex.ExecOptions{Args: []any{sessionID}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("insert session_entries: %v", err)
	}
	s.PoolForTest().Put(conn)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	for _, idx := range []int{0, 1} {
		if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
			EntryTarget:      &store.EntryTarget{SessionID: sessionID, EntryIndex: idx},
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            "resolved",
		}); err != nil {
			t.Fatalf("CreateAnnotation(entry %d): %v", idx, err)
		}
	}

	entryRows, err := s.GetEntryAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetEntryAnnotationsForSession: %v", err)
	}
	if len(entryRows) != 2 {
		t.Fatalf("expected 2 entry annotations for session, got %d", len(entryRows))
	}
	for _, r := range entryRows {
		if r.TargetKind != schema.TargetEntry {
			t.Errorf("TargetKind = %q, want %q", r.TargetKind, schema.TargetEntry)
		}
	}

	// Regression: the session-only query must NOT surface entry annotations.
	sessionRows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(sessionRows) != 0 {
		t.Fatalf("GetAnnotationsForSession should return 0 (entry-only session), got %d", len(sessionRows))
	}
}

// ---------------------------------------------------------------------------
// GetAnnotationTypeByTypeID / GetAnnotationTypeByDBID
// ---------------------------------------------------------------------------

// TestGetAnnotationTypeByTypeID_SeedTypes verifies retrieval of seed types.
func TestGetAnnotationTypeByTypeID_SeedTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	row, err := s.GetAnnotationTypeByTypeID(ctx, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatalf("GetAnnotationTypeByTypeID: %v", err)
	}
	if row == nil {
		t.Fatal("expected row, got nil")
	}
	if row.TypeID != testutil.TestTypeIDSessionApproval {
		t.Errorf("TypeID = %q, want %q", row.TypeID, testutil.TestTypeIDSessionApproval)
	}
	if row.Status != schema.StatusDeprecated {
		t.Errorf("Status = %q, want %q", row.Status, schema.StatusDeprecated)
	}
	if row.ValueDomainKind != schema.DomainEnumerated {
		t.Errorf("ValueDomainKind = %q, want %q", row.ValueDomainKind, schema.DomainEnumerated)
	}
}

// TestGetAnnotationTypeByTypeID_NotFound returns nil for unknown type.
func TestGetAnnotationTypeByTypeID_NotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	row, err := s.GetAnnotationTypeByTypeID(ctx, "quality.does_not_exist")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if row != nil {
		t.Errorf("expected nil, got %+v", row)
	}
}

// TestListAnnotationTypes_AllSeedTypes returns all seed types.
func TestListAnnotationTypes_AllSeedTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	rows, err := s.ListAnnotationTypes(ctx, "", "")
	if err != nil {
		t.Fatalf("ListAnnotationTypes: %v", err)
	}
	// 4 from V13 + 2 from V18 + 1 from V20 + 1 from V25 + 1 from V35 (user.custom_label)
	// + 2 from V39 (quality.turn_outcome, quality.turn_flag).
	if len(rows) != 11 {
		t.Errorf("expected 11 seed annotation types, got %d", len(rows))
	}
}

// TestListAnnotationTypes_StatusFilter returns only active types.
func TestListAnnotationTypes_StatusFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	rows, err := s.ListAnnotationTypes(ctx, string(schema.StatusActive), "")
	if err != nil {
		t.Fatalf("ListAnnotationTypes: %v", err)
	}
	// Active types: research.friction_episode (V25) + user.custom_label (V35)
	// + quality.turn_outcome + quality.turn_flag (V39). V25 deprecated every
	// earlier type; these four were seeded active after it.
	if len(rows) != 4 {
		t.Errorf("expected 4 active types (research.friction_episode from V25, user.custom_label from V35, quality.turn_outcome + quality.turn_flag from V39), got %d", len(rows))
	}
	for _, row := range rows {
		if row.Status != schema.StatusActive {
			t.Errorf("row %q: Status = %q, want %q", row.TypeID, row.Status, schema.StatusActive)
		}
	}
}

// TestListAnnotationTypes_OriginFilter returns only system types.
func TestListAnnotationTypes_OriginFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	rows, err := s.ListAnnotationTypes(ctx, "", string(schema.OriginSystem))
	if err != nil {
		t.Fatalf("ListAnnotationTypes: %v", err)
	}
	if len(rows) != 7 {
		t.Errorf("expected 7 system-origin types (4 from V13 + 2 from V18 + 1 from V20), got %d", len(rows))
	}
	for _, row := range rows {
		if row.Origin != schema.OriginSystem {
			t.Errorf("row %q: Origin = %q, want %q", row.TypeID, row.Origin, schema.OriginSystem)
		}
	}
}

// ---------------------------------------------------------------------------
// CreateAnnotationType + ActivateAnnotationType + DeprecateAnnotationType
// ---------------------------------------------------------------------------

// TestCreateAnnotationType_NewType verifies inserting a new annotation type.
func TestCreateAnnotationType_NewType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	dbID, err := s.CreateAnnotationType(ctx, store.CreateAnnotationTypeParams{
		TypeID:          "quality.new_test_type",
		DisplayName:     "New Test Type",
		FamilyID:        store.GenerateEntityUUID("annotation_families", 1),
		ValueDomainKind: schema.DomainEnumerated,
		Datatype:        schema.DatatypeText,
		ValueConstraint: `["yes","no"]`,
		Origin:          schema.OriginUser,
	})
	if err != nil {
		t.Fatalf("CreateAnnotationType: %v", err)
	}
	if dbID == "" {
		t.Error("CreateAnnotationType: expected non-empty DBID")
	}

	row, err := s.GetAnnotationTypeByTypeID(ctx, "quality.new_test_type")
	if err != nil {
		t.Fatalf("GetAnnotationTypeByTypeID: %v", err)
	}
	if row == nil {
		t.Fatal("expected row, got nil")
	}
	if row.Status != schema.StatusProposed {
		t.Errorf("Status = %q, want %q (new types start as proposed)", row.Status, schema.StatusProposed)
	}
}

// TestActivateAnnotationType_ProposedToActive verifies lifecycle transition.
func TestActivateAnnotationType_ProposedToActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.CreateAnnotationType(ctx, store.CreateAnnotationTypeParams{
		TypeID:          "quality.activate_me",
		DisplayName:     "Activate Me",
		FamilyID:        store.GenerateEntityUUID("annotation_families", 1),
		ValueDomainKind: schema.DomainEnumerated,
		Datatype:        schema.DatatypeText,
		ValueConstraint: `["v"]`,
		Origin:          schema.OriginUser,
	}); err != nil {
		t.Fatalf("CreateAnnotationType: %v", err)
	}

	if err := s.ActivateAnnotationType(ctx, "quality.activate_me"); err != nil {
		t.Fatalf("ActivateAnnotationType: %v", err)
	}

	row, err := s.GetAnnotationTypeByTypeID(ctx, "quality.activate_me")
	if err != nil {
		t.Fatalf("GetAnnotationTypeByTypeID: %v", err)
	}
	if row.Status != schema.StatusActive {
		t.Errorf("after Activate: Status = %q, want %q", row.Status, schema.StatusActive)
	}
}

// ---------------------------------------------------------------------------
// AddAnnotationTypeDependency + GetAnnotationTypeDependencies + Cycle Detection
// ---------------------------------------------------------------------------

// TestAddAnnotationTypeDependency_Valid verifies inserting a valid dep edge.
func TestAddAnnotationTypeDependency_Valid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	// Register two new types to test with (avoids interfering with seed data deps).
	for _, id := range []string{"quality.dep_source", "quality.dep_target"} {
		if _, err := s.CreateAnnotationType(ctx, store.CreateAnnotationTypeParams{
			TypeID:          id,
			DisplayName:     id,
			FamilyID:        store.GenerateEntityUUID("annotation_families", 1),
			ValueDomainKind: schema.DomainEnumerated,
			Datatype:        schema.DatatypeText,
			ValueConstraint: `["v"]`,
			Origin:          schema.OriginUser,
		}); err != nil {
			t.Fatalf("CreateAnnotationType(%q): %v", id, err)
		}
	}

	if err := s.AddAnnotationTypeDependency(ctx, "quality.dep_source", "quality.dep_target", true, "test reason"); err != nil {
		t.Fatalf("AddAnnotationTypeDependency: %v", err)
	}

	deps, err := s.GetAnnotationTypeDependencies(ctx, "quality.dep_source")
	if err != nil {
		t.Fatalf("GetAnnotationTypeDependencies: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].DependsOn != "quality.dep_target" {
		t.Errorf("DependsOn = %q, want %q", deps[0].DependsOn, "quality.dep_target")
	}
	if !deps[0].Required {
		t.Error("Required = false, want true")
	}
}

// TestAddAnnotationTypeDependency_CycleDetected verifies V14: cycle detection.
func TestAddAnnotationTypeDependency_CycleDetected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	// Register X, Y, Z.
	for _, id := range []string{"quality.cycle_x", "quality.cycle_y", "quality.cycle_z"} {
		if _, err := s.CreateAnnotationType(ctx, store.CreateAnnotationTypeParams{
			TypeID:          id,
			DisplayName:     id,
			FamilyID:        store.GenerateEntityUUID("annotation_families", 1),
			ValueDomainKind: schema.DomainEnumerated,
			Datatype:        schema.DatatypeText,
			ValueConstraint: `["v"]`,
			Origin:          schema.OriginUser,
		}); err != nil {
			t.Fatalf("CreateAnnotationType(%q): %v", id, err)
		}
	}

	// Build chain: X → Y → Z.
	if err := s.AddAnnotationTypeDependency(ctx, "quality.cycle_x", "quality.cycle_y", false, "x→y"); err != nil {
		t.Fatalf("AddDep(x→y): %v", err)
	}
	if err := s.AddAnnotationTypeDependency(ctx, "quality.cycle_y", "quality.cycle_z", false, "y→z"); err != nil {
		t.Fatalf("AddDep(y→z): %v", err)
	}

	// Z → X would create a cycle.
	err := s.AddAnnotationTypeDependency(ctx, "quality.cycle_z", "quality.cycle_x", false, "z→x cycle")
	if err == nil {
		t.Fatal("AddDep(z→x cycle): expected error, got nil")
	}
	// Error should mention cycle.
	if !contains(err.Error(), "cycle") {
		t.Errorf("expected cycle error, got: %v", err)
	}
}

// contains checks whether substr is present in s (avoids errors import for simple string check).
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && searchString(s, substr))
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// CreateAnnotation — meta-annotation (ARM 3) AC4
// ---------------------------------------------------------------------------

// TestCreateAnnotation_MetaAnnotation verifies that an annotation can target
// another annotation (ARM 3: target_annotation_id), satisfying AC4.
func TestCreateAnnotation_MetaAnnotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000006"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	// ARM 1: create A1 targeting the session.
	a1ID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(A1): %v", err)
	}
	if a1ID == "" {
		t.Fatal("CreateAnnotation(A1): expected non-empty ID")
	}

	// ARM 3: create A2 targeting A1 (meta-annotation).
	approvalTypeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionApproval)
	a2ID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		AnnotationID:     &a1ID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: approvalTypeID,
		Value:            "approve",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(A2 meta): %v", err)
	}
	if a2ID == "" {
		t.Fatal("CreateAnnotation(A2 meta): expected non-empty ID")
	}

	// Verify A2 is stored with target_annotation_id = A1.id.
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	var storedAnnotID string
	var storedValue string
	if err := sqlitex.ExecuteTransient(conn,
		`SELECT ata.target_annotation_id, a.value FROM annotation_target_annotations ata
		 JOIN annotations a ON a.id = ata.annotation_id
		 WHERE ata.annotation_id = ?`,
		&sqlitex.ExecOptions{
			Args: []any{a2ID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				storedAnnotID = stmt.ColumnText(0)
				storedValue = stmt.ColumnText(1)
				return nil
			},
		},
	); err != nil {
		t.Fatalf("verify meta-annotation: %v", err)
	}
	if storedAnnotID != a1ID {
		t.Errorf("meta-annotation target_annotation_id = %q, want %q (A1.id)", storedAnnotID, a1ID)
	}
	if storedValue != "approve" {
		t.Errorf("meta-annotation value = %q, want %q", storedValue, "approve")
	}
}

// ---------------------------------------------------------------------------
// CreateAnnotator — exclusive-arc constraint rejection (AC14)
// ---------------------------------------------------------------------------

// TestCreateAnnotator_AgentKindRequiresModelID verifies that the DB CHECK constraint
// rejects an agent annotator inserted without model_id + provider_key (AC14).
func TestCreateAnnotator_AgentKindRequiresModelID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	_, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorAgent,
		Name:        "agent-without-model",
		DisplayName: "Bad Agent",
	})
	if err == nil {
		t.Fatal("CreateAnnotator(agent without model_id): expected DB CHECK error, got nil")
	}
}

// TestCreateAnnotator_NonAgentForbidsModelID verifies that the DB CHECK constraint
// rejects a rule annotator inserted with model_id (AC14).
func TestCreateAnnotator_NonAgentForbidsModelID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	modelID := "some-model"
	provKey := "anthropic"
	_, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorRule,
		Name:        "rule-with-model",
		DisplayName: "Bad Rule",
		ModelID:     &modelID,
		ProviderKey: &provKey,
	})
	if err == nil {
		t.Fatal("CreateAnnotator(rule with model_id): expected DB CHECK error, got nil")
	}
}

// ---------------------------------------------------------------------------
// CreateAnnotation: project target (ARM 4)
// ---------------------------------------------------------------------------

// testProjectHash is a valid 64-char project hash for store tests.
const testProjectHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// seedTestProjectForStore seeds a project row used by project annotation tests.
func seedTestProjectForStore(t *testing.T, s *store.Store) {
	t.Helper()
	conn, err := s.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.PoolForTest().Put(conn)

	if err := sqlitex.ExecuteTransient(conn,
		`INSERT OR IGNORE INTO projects VALUES (?, 'store-test-proj', '/store-test-proj')`,
		&sqlitex.ExecOptions{Args: []any{testProjectHash}}); err != nil {
		t.Fatalf("insert project: %v", err)
	}
}

// TestCreateAnnotation_ProjectTarget verifies inserting a project-level annotation (ARM 4).
func TestCreateAnnotation_ProjectTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	seedTestProjectForStore(t, s)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	projHash := testProjectHash
	id, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		ProjectHash:      &projHash,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(project): %v", err)
	}
	if id == "" {
		t.Error("CreateAnnotation(project): expected non-empty ID")
	}
}

// TestGetAnnotationsForProject_ReturnsAnnotations verifies GetAnnotationsForProject
// returns annotations targeting a project hash.
func TestGetAnnotationsForProject_ReturnsAnnotations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	seedTestProjectForStore(t, s)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	projHash := testProjectHash
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		ProjectHash:      &projHash,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("CreateAnnotation(project): %v", err)
	}

	rows, err := s.GetAnnotationsForProject(ctx, testProjectHash)
	if err != nil {
		t.Fatalf("GetAnnotationsForProject: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}

	row := rows[0]
	if row.TargetKind != schema.TargetProject {
		t.Errorf("TargetKind = %q, want %q", row.TargetKind, schema.TargetProject)
	}
	if row.TargetProjectHash == nil || *row.TargetProjectHash != testProjectHash {
		t.Errorf("TargetProjectHash = %v, want %q", row.TargetProjectHash, testProjectHash)
	}
	if row.Value != "resolved" {
		t.Errorf("Value = %q, want %q", row.Value, "resolved")
	}
}

// TestGetAnnotationsForProject_Empty returns empty slice when no project annotations exist.
func TestGetAnnotationsForProject_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	rows, err := s.GetAnnotationsForProject(ctx, "nonexistent-project-hash")
	if err != nil {
		t.Fatalf("GetAnnotationsForProject: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows, got %d", len(rows))
	}
}

// TestGetAnnotationsForProject_SupersededExcluded verifies that annotations whose
// superseded_by column is non-NULL are excluded from GetAnnotationsForProject results.
// The SQL query filters WHERE a.superseded_by IS NULL; this test asserts that filter works.
func TestGetAnnotationsForProject_SupersededExcluded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	seedTestProjectForStore(t, s)
	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	projHash := testProjectHash

	// Insert the first annotation (will be superseded).
	firstID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		ProjectHash:      &projHash,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "needs-work",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(first): %v", err)
	}

	// Insert the second annotation (the superseding one).
	secondID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		ProjectHash:      &projHash,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation(second): %v", err)
	}

	// Mark the first annotation as superseded by the second via raw SQL.
	conn := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn)

	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE annotations SET superseded_by = ? WHERE id = ?`,
		&sqlitex.ExecOptions{Args: []any{secondID, firstID}}); err != nil {
		t.Fatalf("UPDATE superseded_by: %v", err)
	}

	// GetAnnotationsForProject must return only the non-superseded annotation.
	rows, err := s.GetAnnotationsForProject(ctx, testProjectHash)
	if err != nil {
		t.Fatalf("GetAnnotationsForProject: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (superseded excluded), got %d", len(rows))
	}
	if rows[0].ID != secondID {
		t.Errorf("returned annotation ID = %q, want secondID=%q (superseded first excluded)", rows[0].ID, secondID)
	}
	if rows[0].Value != "resolved" {
		t.Errorf("Value = %q, want %q", rows[0].Value, "resolved")
	}
}

// ---------------------------------------------------------------------------
// is_primary
// ---------------------------------------------------------------------------

// TestAnnotationRow_IsPrimary_StoredAndReturned verifies that IsPrimary=true is
// stored as is_primary=1 and returned correctly in AnnotationRow.
func TestAnnotationRow_IsPrimary_StoredAndReturned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000007"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
		IsPrimary:        true,
	}); err != nil {
		t.Fatalf("CreateAnnotation(IsPrimary=true): %v", err)
	}

	rows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if !rows[0].IsPrimary {
		t.Error("IsPrimary: expected true, got false")
	}
}

// TestAnnotationRow_IsPrimary_FalseByDefault verifies that IsPrimary defaults to
// false when not set in CreateAnnotationParams.
func TestAnnotationRow_IsPrimary_FalseByDefault(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000008"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionID,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "partial",
		// IsPrimary not set → defaults to false
	}); err != nil {
		t.Fatalf("CreateAnnotation(default IsPrimary): %v", err)
	}

	rows, err := s.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].IsPrimary {
		t.Error("IsPrimary: expected false (default), got true")
	}
}

// ---------------------------------------------------------------------------
// priority_override
// ---------------------------------------------------------------------------

// TestAnnotationTypeRow_PriorityOverride_StoredAndReturned verifies that
// PriorityOverride is stored and returned in AnnotationTypeRow.
func TestAnnotationTypeRow_PriorityOverride_StoredAndReturned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	// Set priority_override on the session_outcome type via direct SQL.
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE annotation_types SET priority_override = 10 WHERE type_id = ?`,
		&sqlitex.ExecOptions{Args: []any{testutil.TestTypeIDSessionOutcome}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("set priority_override: %v", err)
	}
	s.PoolForTest().Put(conn)

	row, err := s.GetAnnotationTypeByTypeID(ctx, testutil.TestTypeIDSessionOutcome)
	if err != nil {
		t.Fatalf("GetAnnotationTypeByTypeID: %v", err)
	}
	if row == nil {
		t.Fatal("expected row, got nil")
	}
	if row.PriorityOverride == nil {
		t.Fatal("PriorityOverride: expected non-nil, got nil")
	}
	if *row.PriorityOverride != 10 {
		t.Errorf("PriorityOverride: expected 10, got %d", *row.PriorityOverride)
	}
}

// TestGetEffectiveAnnotation_PriorityOverrideChangesOrdering verifies that
// priority_override=0 causes a rule annotation (normally priority=1) to be
// treated as priority=0, allowing a newer rule annotation to win over an older
// one even if both are rule-based — and more importantly, verifying that the
// COALESCE(priority_override, kind_priority) logic is applied correctly.
// Scenario: type has priority_override=0; rule creates at T1, human creates at T2>T1.
// Without priority_override: human wins (priority 3 > 1).
// With priority_override=0: both have priority 0; tie broken by T2 > T1 → human wins.
// This confirms the priority_override is used in the ORDER BY.
func TestGetEffectiveAnnotation_PriorityOverrideApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000009"
	seedTestSessionV13(t, ctx, s, sessionID)

	approvalTypeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionApproval)
	ruleAnnotatorID := seedAnnotatorIDForTest(t, s)

	// Set priority_override=0 on the approval type so kind-based priority is suppressed.
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE annotation_types SET priority_override = 0 WHERE type_id = ?`,
		&sqlitex.ExecOptions{Args: []any{testutil.TestTypeIDSessionApproval}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("set priority_override: %v", err)
	}
	s.PoolForTest().Put(conn)

	// Create human annotator.
	humanAnnotatorID, err := s.CreateAnnotator(ctx, store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorHuman,
		Name:        "test-human-priority-override",
		DisplayName: "Human Reviewer (priority test)",
	})
	if err != nil {
		t.Fatalf("CreateAnnotator(human): %v", err)
	}

	// Use explicit timestamps (1 ms apart) so the tie-break is deterministic
	// and does not depend on wall-clock resolution. Rule gets T, human gets T+1.
	const (
		tsRule  = int64(1700000000000)
		tsHuman = int64(1700000000001)
	)

	// V16: insert via parent annotations table + TPT child annotation_target_sessions.
	insertAnnotSQL := `INSERT INTO annotations (
		id, target_kind_id, annotator_id, annotation_type_id, value,
		confidence, reason, provenance, is_primary, created_at
	) VALUES (?, (SELECT id FROM target_kinds WHERE name = 'session'), ?, ?, ?, NULL, NULL, NULL, 0, ?)`
	insertTargetSQL := `INSERT INTO annotation_target_sessions (annotation_id, session_id) VALUES (?, ?)`

	conn2 := takeConn(t, s.PoolForTest())
	defer s.PoolForTest().Put(conn2)

	// Rule says "deny" at T.
	ruleAnnID := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn2, insertAnnotSQL, &sqlitex.ExecOptions{
		Args: []any{ruleAnnID, ruleAnnotatorID, approvalTypeID, "deny", tsRule},
	}); err != nil {
		t.Fatalf("insert rule annotation: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn2, insertTargetSQL, &sqlitex.ExecOptions{
		Args: []any{ruleAnnID, sessionID},
	}); err != nil {
		t.Fatalf("insert rule target session: %v", err)
	}

	// Human says "approve" at T+1 ms → wins the tie-break.
	humanAnnID := uuid.New().String()
	if err := sqlitex.ExecuteTransient(conn2, insertAnnotSQL, &sqlitex.ExecOptions{
		Args: []any{humanAnnID, humanAnnotatorID, approvalTypeID, "approve", tsHuman},
	}); err != nil {
		t.Fatalf("insert human annotation: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn2, insertTargetSQL, &sqlitex.ExecOptions{
		Args: []any{humanAnnID, sessionID},
	}); err != nil {
		t.Fatalf("insert human target session: %v", err)
	}

	result, err := s.GetEffectiveAnnotation(ctx, sessionID, testutil.TestTypeIDSessionApproval)
	if err != nil {
		t.Fatalf("GetEffectiveAnnotation: %v", err)
	}
	if result == nil {
		t.Fatal("GetEffectiveAnnotation: got nil, expected annotation")
	}
	// With priority_override=0, both rule and human get priority=COALESCE(0,*)=0.
	// Tie broken by most recent created_at → human wins (tsHuman > tsRule).
	if result.Value != "approve" {
		t.Errorf("GetEffectiveAnnotation: Value = %q, want %q (human has newer created_at, should win tie)", result.Value, "approve")
	}
}

// ---------------------------------------------------------------------------
// CountAnnotationsByAnnotator / DeleteAnnotationsByAnnotator
// ---------------------------------------------------------------------------

// pruneAnnotatorName is the seed annotator used in prune tests.
const pruneAnnotatorName = "outcome-classifier"

// TestDeleteAnnotationsByAnnotator_Unscoped verifies that an unscoped delete removes
// all annotations by the given annotator and returns the correct count.
func TestDeleteAnnotationsByAnnotator_Unscoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000010"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	// Create two annotations for the annotator ("resolved" and "partial" are valid
	// quality.session_outcome values).
	for _, val := range []string{"resolved", "partial"} {
		if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
			SessionID:        &sessionID,
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            val,
		}); err != nil {
			t.Fatalf("CreateAnnotation(%s): %v", val, err)
		}
	}

	// Count before delete.
	count, err := s.CountAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
	if err != nil {
		t.Fatalf("CountAnnotationsByAnnotator: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountAnnotationsByAnnotator: expected 2, got %d", count)
	}

	// Delete all.
	deleted, err := s.DeleteAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
	if err != nil {
		t.Fatalf("DeleteAnnotationsByAnnotator: %v", err)
	}
	if deleted != 2 {
		t.Errorf("DeleteAnnotationsByAnnotator: expected 2 deleted, got %d", deleted)
	}

	// Count after delete — should be zero.
	afterCount, err := s.CountAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
	if err != nil {
		t.Fatalf("CountAnnotationsByAnnotator (after delete): %v", err)
	}
	if afterCount != 0 {
		t.Errorf("CountAnnotationsByAnnotator after delete: expected 0, got %d", afterCount)
	}
}

// TestDeleteAnnotationsByAnnotator_Scoped verifies that a scoped delete removes
// only annotations for the specified sessions and leaves other sessions intact.
func TestDeleteAnnotationsByAnnotator_Scoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionA := "c1305555-0000-0000-0000-000000000011"
	sessionB := "c1305555-0000-0000-0000-000000000012"
	seedTestSessionV13(t, ctx, s, sessionA)
	seedTestSessionV13(t, ctx, s, sessionB)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	// Create one annotation on each session.
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionA,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("CreateAnnotation(sessionA): %v", err)
	}
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionB,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "abandoned",
	}); err != nil {
		t.Fatalf("CreateAnnotation(sessionB): %v", err)
	}

	// Delete scoped to session A only.
	deleted, err := s.DeleteAnnotationsByAnnotator(ctx, pruneAnnotatorName, []string{sessionA})
	if err != nil {
		t.Fatalf("DeleteAnnotationsByAnnotator(scoped): %v", err)
	}
	if deleted != 1 {
		t.Errorf("DeleteAnnotationsByAnnotator(scoped): expected 1 deleted, got %d", deleted)
	}

	// Session B annotation should survive.
	rows, err := s.GetAnnotationsForSession(ctx, sessionB)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession(sessionB): %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("GetAnnotationsForSession(sessionB): expected 1 surviving annotation, got %d", len(rows))
	}

	// Session A annotation should be gone.
	rowsA, err := s.GetAnnotationsForSession(ctx, sessionA)
	if err != nil {
		t.Fatalf("GetAnnotationsForSession(sessionA): %v", err)
	}
	if len(rowsA) != 0 {
		t.Errorf("GetAnnotationsForSession(sessionA): expected 0 after scoped delete, got %d", len(rowsA))
	}
}

// TestDeleteAnnotationsByAnnotator_UnknownAnnotator verifies that an unknown
// annotator name returns (0, nil) — not an error.
func TestDeleteAnnotationsByAnnotator_UnknownAnnotator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	count, err := s.DeleteAnnotationsByAnnotator(ctx, "does-not-exist-annotator", nil)
	if err != nil {
		t.Fatalf("DeleteAnnotationsByAnnotator(unknown): expected nil error, got: %v", err)
	}
	if count != 0 {
		t.Errorf("DeleteAnnotationsByAnnotator(unknown): expected 0, got %d", count)
	}
}

// TestCountMatchesDelete verifies that CountAnnotationsByAnnotator returns the same
// number that DeleteAnnotationsByAnnotator subsequently deletes.
func TestCountMatchesDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "c1305555-0000-0000-0000-000000000013"
	seedTestSessionV13(t, ctx, s, sessionID)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	// Create three annotations with valid quality.session_outcome values.
	for _, val := range []string{"resolved", "partial", "failed"} {
		if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
			SessionID:        &sessionID,
			AnnotatorID:      annotatorID,
			AnnotationTypeID: typeID,
			Value:            val,
		}); err != nil {
			t.Fatalf("CreateAnnotation(%s): %v", val, err)
		}
	}

	predicted, err := s.CountAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
	if err != nil {
		t.Fatalf("CountAnnotationsByAnnotator: %v", err)
	}

	deleted, err := s.DeleteAnnotationsByAnnotator(ctx, pruneAnnotatorName, nil)
	if err != nil {
		t.Fatalf("DeleteAnnotationsByAnnotator: %v", err)
	}

	if predicted != deleted {
		t.Errorf("count/delete mismatch: Count=%d, Delete=%d", predicted, deleted)
	}
}

// TestListSystemAndSupersededAnnotations_Partition verifies the two annotation-push
// queries partition annotations by supersession against a REAL database: a
// non-superseded system annotation appears ONLY in ListSystemAnnotations, and a
// superseded one appears ONLY in ListSupersededAnnotations (the retraction source,
// carrying its original content fields). This guards the shared-SQL refactor —
// the two queries differ only in their IS NULL / IS NOT NULL predicate.
func TestListSystemAndSupersededAnnotations_Partition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	activeSession := "c1305555-0000-0000-0000-0000000000a1"
	supersededSession := "c1305555-0000-0000-0000-0000000000a2"
	seedTestSessionV13(t, ctx, s, activeSession)
	seedTestSessionV13(t, ctx, s, supersededSession)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	activeID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID: &activeSession, AnnotatorID: annotatorID, AnnotationTypeID: typeID, Value: "active-val",
	})
	if err != nil {
		t.Fatalf("create active annotation: %v", err)
	}
	supersededID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID: &supersededSession, AnnotatorID: annotatorID, AnnotationTypeID: typeID, Value: "superseded-val",
	})
	if err != nil {
		t.Fatalf("create superseded annotation: %v", err)
	}

	// Mark the second annotation superseded by the first.
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE annotations SET superseded_by = ? WHERE id = ?`,
		&sqlitex.ExecOptions{Args: []any{activeID, supersededID}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("mark superseded: %v", err)
	}
	s.PoolForTest().Put(conn)

	// ListSystemAnnotations → only the active (non-superseded) row.
	sys, err := s.ListSystemAnnotations(ctx)
	if err != nil {
		t.Fatalf("ListSystemAnnotations: %v", err)
	}
	if len(sys) != 1 {
		t.Fatalf("ListSystemAnnotations returned %d rows, want 1 (active only)", len(sys))
	}
	if sys[0].Value != "active-val" {
		t.Errorf("system row Value = %q, want active-val", sys[0].Value)
	}
	if sys[0].TypeID != testutil.TestTypeIDSessionOutcome {
		t.Errorf("system row TypeID = %q, want %q", sys[0].TypeID, testutil.TestTypeIDSessionOutcome)
	}

	// ListSupersededAnnotations → only the superseded row, with content intact.
	sup, err := s.ListSupersededAnnotations(ctx)
	if err != nil {
		t.Fatalf("ListSupersededAnnotations: %v", err)
	}
	if len(sup) != 1 {
		t.Fatalf("ListSupersededAnnotations returned %d rows, want 1 (superseded only)", len(sup))
	}
	if sup[0].Value != "superseded-val" {
		t.Errorf("superseded row Value = %q, want superseded-val", sup[0].Value)
	}
	if sup[0].SessionID == nil || *sup[0].SessionID != supersededSession {
		t.Errorf("superseded row SessionID = %v, want %s", sup[0].SessionID, supersededSession)
	}
}

// TestGetSessionAnnotationsBulk verifies the whole-corpus grouped query the
// quality snapshot uses in place of one GetAnnotationsForSession round-trip
// per session: rows group by target session, superseded annotations are
// excluded, and entry-level annotations never leak into the session groups.
func TestGetSessionAnnotationsBulk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionA := "c1305555-0000-0000-0000-00000000000a"
	sessionB := "c1305555-0000-0000-0000-00000000000b"
	seedTestSessionV13(t, ctx, s, sessionA)
	seedTestSessionV13(t, ctx, s, sessionB)

	annotatorID := seedAnnotatorIDForTest(t, s)
	typeID := seedAnnotationTypeIDForTest(t, s, testutil.TestTypeIDSessionOutcome)

	// Session A: one live annotation plus one that gets superseded.
	supersededID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionA,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "partial",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation (superseded): %v", err)
	}
	liveID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionA,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	})
	if err != nil {
		t.Fatalf("CreateAnnotation (live A): %v", err)
	}

	// Session B: one live annotation.
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sessionB,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "failed",
	}); err != nil {
		t.Fatalf("CreateAnnotation (live B): %v", err)
	}

	// Session B also gets an ENTRY-level annotation — the bulk query is
	// session-level only, so this must not appear in any group.
	conn, err := s.PoolForTest().Take(ctx)
	if err != nil {
		t.Fatalf("pool.Take: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`INSERT INTO session_entries (session_id, entry_index, provider, entry_type, role)
         VALUES (?, 0, 'claude', 'text', 'assistant')`,
		&sqlitex.ExecOptions{Args: []any{sessionB}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("insert session_entry: %v", err)
	}
	if err := sqlitex.ExecuteTransient(conn,
		`UPDATE annotations SET superseded_by = ? WHERE id = ?`,
		&sqlitex.ExecOptions{Args: []any{liveID, supersededID}}); err != nil {
		s.PoolForTest().Put(conn)
		t.Fatalf("mark superseded: %v", err)
	}
	s.PoolForTest().Put(conn)

	entryTarget := &store.EntryTarget{SessionID: sessionB, EntryIndex: 0}
	if _, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		EntryTarget:      entryTarget,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: typeID,
		Value:            "resolved",
	}); err != nil {
		t.Fatalf("CreateAnnotation (entry): %v", err)
	}

	grouped, err := s.GetSessionAnnotationsBulk(ctx)
	if err != nil {
		t.Fatalf("GetSessionAnnotationsBulk: %v", err)
	}

	if len(grouped[sessionA]) != 1 {
		t.Fatalf("session A: expected 1 non-superseded row, got %d", len(grouped[sessionA]))
	}
	if grouped[sessionA][0].Value != "resolved" {
		t.Errorf("session A Value = %q, want %q (superseded excluded)", grouped[sessionA][0].Value, "resolved")
	}
	if len(grouped[sessionB]) != 1 {
		t.Fatalf("session B: expected 1 session-level row (entry-level excluded), got %d", len(grouped[sessionB]))
	}
	if grouped[sessionB][0].Value != "failed" {
		t.Errorf("session B Value = %q, want %q", grouped[sessionB][0].Value, "failed")
	}

	// Parity with the per-session path for a session it never annotated.
	if rows := grouped["c1305555-0000-0000-0000-0000000000ff"]; rows != nil {
		t.Errorf("unknown session: expected no group, got %d rows", len(rows))
	}
}
