package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/peasant-labs/peasant/internal/store"
)

// ---------------------------------------------------------------------------
// CreatePendingAnnotation
// ---------------------------------------------------------------------------

// TestCreatePendingAnnotation_Basic verifies a minimal pending annotation round-trip.
func TestCreatePendingAnnotation_Basic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	rec := store.PendingAnnotationRecord{
		ID:        uuid.New().String(),
		SessionID: "session-e2e-001",
		TypeID:    "quality.session_outcome",
		Value:     "resolved",
		CreatedAt: time.Now().Unix(),
	}
	if err := s.CreatePendingAnnotation(ctx, rec); err != nil {
		t.Fatalf("CreatePendingAnnotation: %v", err)
	}
}

// TestCreatePendingAnnotation_WithEntryIndex verifies entry-level pending annotations.
func TestCreatePendingAnnotation_WithEntryIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	idx := 3
	end := 5
	rec := store.PendingAnnotationRecord{
		ID:         uuid.New().String(),
		SessionID:  "session-e2e-002",
		TypeID:     "quality.frustration_signal",
		Value:      "true",
		EntryIndex: &idx,
		EndIndex:   &end,
		CreatedAt:  time.Now().Unix(),
	}
	if err := s.CreatePendingAnnotation(ctx, rec); err != nil {
		t.Fatalf("CreatePendingAnnotation (entry-level): %v", err)
	}
}

// TestCreatePendingAnnotation_EmptyID returns error for missing id.
func TestCreatePendingAnnotation_EmptyID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	err := s.CreatePendingAnnotation(ctx, store.PendingAnnotationRecord{
		SessionID: "session-e2e-003",
		TypeID:    "quality.session_outcome",
		Value:     "resolved",
	})
	if err == nil {
		t.Fatal("CreatePendingAnnotation(empty id): expected error, got nil")
	}
}

// TestCreatePendingAnnotation_EmptySessionID returns error for missing session_id.
func TestCreatePendingAnnotation_EmptySessionID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	err := s.CreatePendingAnnotation(ctx, store.PendingAnnotationRecord{
		ID:     uuid.New().String(),
		TypeID: "quality.session_outcome",
		Value:  "resolved",
	})
	if err == nil {
		t.Fatal("CreatePendingAnnotation(empty session_id): expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// ListPendingBySession
// ---------------------------------------------------------------------------

// TestListPendingBySession_Empty returns empty slice for unknown session.
func TestListPendingBySession_Empty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	recs, err := s.ListPendingBySession(ctx, "nonexistent-session")
	if err != nil {
		t.Fatalf("ListPendingBySession: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected 0 records, got %d", len(recs))
	}
}

// TestListPendingBySession_Multiple verifies ordering by created_at ASC.
func TestListPendingBySession_Multiple(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "session-list-multi-" + uuid.New().String()
	now := time.Now().Unix()

	recs := []store.PendingAnnotationRecord{
		{ID: uuid.New().String(), SessionID: sessionID, TypeID: "quality.session_outcome", Value: "resolved", CreatedAt: now + 2},
		{ID: uuid.New().String(), SessionID: sessionID, TypeID: "quality.user_frustration", Value: "low", CreatedAt: now + 1},
		{ID: uuid.New().String(), SessionID: sessionID, TypeID: "quality.session_outcome", Value: "partial", CreatedAt: now},
	}
	for _, r := range recs {
		if err := s.CreatePendingAnnotation(ctx, r); err != nil {
			t.Fatalf("CreatePendingAnnotation: %v", err)
		}
	}

	got, err := s.ListPendingBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListPendingBySession: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	// Verify ascending order by created_at.
	if got[0].CreatedAt > got[1].CreatedAt || got[1].CreatedAt > got[2].CreatedAt {
		t.Errorf("records not in ascending order: %v, %v, %v", got[0].CreatedAt, got[1].CreatedAt, got[2].CreatedAt)
	}
	// Verify the oldest record (now) is first.
	if got[0].Value != "partial" {
		t.Errorf("first record Value = %q, want %q", got[0].Value, "partial")
	}
}

// TestListPendingBySession_IsolatedBySession verifies session isolation.
func TestListPendingBySession_IsolatedBySession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sidA := "session-isolated-A-" + uuid.New().String()
	sidB := "session-isolated-B-" + uuid.New().String()

	recA := store.PendingAnnotationRecord{
		ID:        uuid.New().String(),
		SessionID: sidA,
		TypeID:    "quality.session_outcome",
		Value:     "resolved",
		CreatedAt: time.Now().Unix(),
	}
	recB := store.PendingAnnotationRecord{
		ID:        uuid.New().String(),
		SessionID: sidB,
		TypeID:    "quality.session_outcome",
		Value:     "failed",
		CreatedAt: time.Now().Unix(),
	}
	for _, r := range []store.PendingAnnotationRecord{recA, recB} {
		if err := s.CreatePendingAnnotation(ctx, r); err != nil {
			t.Fatalf("CreatePendingAnnotation: %v", err)
		}
	}

	gotA, err := s.ListPendingBySession(ctx, sidA)
	if err != nil {
		t.Fatalf("ListPendingBySession(A): %v", err)
	}
	if len(gotA) != 1 || gotA[0].Value != "resolved" {
		t.Errorf("session A: expected 1 record with value=resolved, got %+v", gotA)
	}

	gotB, err := s.ListPendingBySession(ctx, sidB)
	if err != nil {
		t.Fatalf("ListPendingBySession(B): %v", err)
	}
	if len(gotB) != 1 || gotB[0].Value != "failed" {
		t.Errorf("session B: expected 1 record with value=failed, got %+v", gotB)
	}
}

// TestListPendingBySession_EntryIndexRoundtrip verifies nullable EntryIndex/EndIndex round-trip.
func TestListPendingBySession_EntryIndexRoundtrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "session-entry-rt-" + uuid.New().String()
	entryIdx := 7
	endIdx := 10
	rec := store.PendingAnnotationRecord{
		ID:         uuid.New().String(),
		SessionID:  sessionID,
		TypeID:     "quality.frustration_signal",
		Value:      "true",
		EntryIndex: &entryIdx,
		EndIndex:   &endIdx,
		CreatedAt:  time.Now().Unix(),
	}
	if err := s.CreatePendingAnnotation(ctx, rec); err != nil {
		t.Fatalf("CreatePendingAnnotation: %v", err)
	}

	got, err := s.ListPendingBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListPendingBySession: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	if got[0].EntryIndex == nil || *got[0].EntryIndex != entryIdx {
		t.Errorf("EntryIndex = %v, want %d", got[0].EntryIndex, entryIdx)
	}
	if got[0].EndIndex == nil || *got[0].EndIndex != endIdx {
		t.Errorf("EndIndex = %v, want %d", got[0].EndIndex, endIdx)
	}
}

// ---------------------------------------------------------------------------
// DeletePendingByID
// ---------------------------------------------------------------------------

// TestDeletePendingByID_DeletesRecord verifies deletion by UUID.
func TestDeletePendingByID_DeletesRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "session-del-by-id-" + uuid.New().String()
	id := uuid.New().String()
	rec := store.PendingAnnotationRecord{
		ID:        id,
		SessionID: sessionID,
		TypeID:    "quality.session_outcome",
		Value:     "resolved",
		CreatedAt: time.Now().Unix(),
	}
	if err := s.CreatePendingAnnotation(ctx, rec); err != nil {
		t.Fatalf("CreatePendingAnnotation: %v", err)
	}

	if err := s.DeletePendingByID(ctx, id); err != nil {
		t.Fatalf("DeletePendingByID: %v", err)
	}

	recs, err := s.ListPendingBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListPendingBySession after delete: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected 0 records after delete, got %d", len(recs))
	}
}

// TestDeletePendingByID_Idempotent verifies double-delete returns nil.
func TestDeletePendingByID_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	id := uuid.New().String()
	// Do not insert — just delete a non-existent ID.
	if err := s.DeletePendingByID(ctx, id); err != nil {
		t.Errorf("DeletePendingByID(non-existent): expected nil, got %v", err)
	}
}

// TestDeletePendingByID_OnlyDeletesOne verifies other records in session survive.
func TestDeletePendingByID_OnlyDeletesOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "session-del-one-" + uuid.New().String()
	idToDelete := uuid.New().String()
	idToKeep := uuid.New().String()
	now := time.Now().Unix()

	for _, r := range []store.PendingAnnotationRecord{
		{ID: idToDelete, SessionID: sessionID, TypeID: "quality.session_outcome", Value: "resolved", CreatedAt: now},
		{ID: idToKeep, SessionID: sessionID, TypeID: "quality.user_frustration", Value: "low", CreatedAt: now + 1},
	} {
		if err := s.CreatePendingAnnotation(ctx, r); err != nil {
			t.Fatalf("CreatePendingAnnotation: %v", err)
		}
	}

	if err := s.DeletePendingByID(ctx, idToDelete); err != nil {
		t.Fatalf("DeletePendingByID: %v", err)
	}

	recs, err := s.ListPendingBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListPendingBySession after delete-one: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("expected 1 remaining record, got %d", len(recs))
	}
	if recs[0].ID != idToKeep {
		t.Errorf("remaining record ID = %q, want %q", recs[0].ID, idToKeep)
	}
}

// ---------------------------------------------------------------------------
// DeleteAllPendingBySession
// ---------------------------------------------------------------------------

// TestDeleteAllPendingBySession_DeletesAll verifies all records for a session are removed.
func TestDeleteAllPendingBySession_DeletesAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sessionID := "session-del-all-" + uuid.New().String()
	now := time.Now().Unix()

	for i := range 3 {
		r := store.PendingAnnotationRecord{
			ID:        uuid.New().String(),
			SessionID: sessionID,
			TypeID:    "quality.session_outcome",
			Value:     "resolved",
			CreatedAt: now + int64(i),
		}
		if err := s.CreatePendingAnnotation(ctx, r); err != nil {
			t.Fatalf("CreatePendingAnnotation %d: %v", i, err)
		}
	}

	if err := s.DeleteAllPendingBySession(ctx, sessionID); err != nil {
		t.Fatalf("DeleteAllPendingBySession: %v", err)
	}

	recs, err := s.ListPendingBySession(ctx, sessionID)
	if err != nil {
		t.Fatalf("ListPendingBySession after delete-all: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("expected 0 records after delete-all, got %d", len(recs))
	}
}

// TestDeleteAllPendingBySession_Idempotent verifies empty-session delete returns nil.
func TestDeleteAllPendingBySession_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.DeleteAllPendingBySession(ctx, "session-no-pending-"+uuid.New().String()); err != nil {
		t.Errorf("DeleteAllPendingBySession(empty): expected nil, got %v", err)
	}
}

// TestDeleteAllPendingBySession_OtherSessionIsolated verifies cross-session isolation.
func TestDeleteAllPendingBySession_OtherSessionIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	sidToDel := "session-del-target-" + uuid.New().String()
	sidToKeep := "session-del-keep-" + uuid.New().String()
	now := time.Now().Unix()

	for _, r := range []store.PendingAnnotationRecord{
		{ID: uuid.New().String(), SessionID: sidToDel, TypeID: "quality.session_outcome", Value: "resolved", CreatedAt: now},
		{ID: uuid.New().String(), SessionID: sidToKeep, TypeID: "quality.session_outcome", Value: "failed", CreatedAt: now},
	} {
		if err := s.CreatePendingAnnotation(ctx, r); err != nil {
			t.Fatalf("CreatePendingAnnotation: %v", err)
		}
	}

	if err := s.DeleteAllPendingBySession(ctx, sidToDel); err != nil {
		t.Fatalf("DeleteAllPendingBySession: %v", err)
	}

	kept, err := s.ListPendingBySession(ctx, sidToKeep)
	if err != nil {
		t.Fatalf("ListPendingBySession(keepSession): %v", err)
	}
	if len(kept) != 1 {
		t.Errorf("expected 1 record in keep-session, got %d", len(kept))
	}
}
