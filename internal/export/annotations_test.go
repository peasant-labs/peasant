package export_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// testAnnotatorName is the name used for the test annotator in export tests.
const testAnnotatorName = "export-test-human"

// testSessionID is a deterministic UUID for export tests.
const testSessionID = testutil.TestSessionUUID

// seedAnnotator creates a human annotator and returns its DB UUID.
func seedAnnotator(t *testing.T, s *store.Store) string {
	t.Helper()
	id, err := s.CreateAnnotator(context.Background(), store.CreateAnnotatorParams{
		Kind:        schema.AnnotatorHuman,
		Name:        testAnnotatorName,
		DisplayName: "Export Test Human",
		Description: "Test annotator for export tests",
	})
	if err != nil {
		t.Fatalf("seedAnnotator: %v", err)
	}
	return id
}

// lookupTypeDBID resolves a type_id string (e.g. "quality.session_approval") to its DB UUID.
func lookupTypeDBID(t *testing.T, s *store.Store, typeID string) string {
	t.Helper()
	row, err := s.GetAnnotationTypeByTypeID(context.Background(), typeID)
	if err != nil {
		t.Fatalf("lookupTypeDBID(%q): %v", typeID, err)
	}
	if row == nil {
		t.Fatalf("lookupTypeDBID(%q): type not found in store (check migration seeds)", typeID)
	}
	return row.ID
}

// seedSessionEntries inserts n session entry rows (indices 0..n-1) in a single
// IndexSessionEntries call, avoiding the per-call DELETE that SeedSessionEntry uses.
func seedSessionEntries(t *testing.T, s *store.Store, sessionID string, n int) {
	t.Helper()
	entries := make([]schema.SessionEntry, n)
	for i := 0; i < n; i++ {
		entries[i] = schema.SessionEntry{
			SessionID:  schema.SessionID(sessionID),
			EntryIndex: i,
			Harness:    ingest.HarnessClaudeCode,
			EntryType:  schema.EntryTypeText,
			Role:       schema.RoleAssistant,
		}
	}
	if err := s.IndexSessionEntries(context.Background(), schema.SessionID(sessionID), entries); err != nil {
		t.Fatalf("seedSessionEntries(%q, %d): %v", sessionID, n, err)
	}
}

// TestExportAnnotations_SessionAndEntryLevel verifies that ExportAnnotations returns
// correctly mapped annotations for both session-level and entry-level targets.
func TestExportAnnotations_SessionAndEntryLevel(t *testing.T) {
	s := storetest.Open(t)
	ctx := context.Background()

	storetest.SeedSession(t, s, testSessionID)
	seedSessionEntries(t, s, testSessionID, 3)

	annotatorID := seedAnnotator(t, s)
	approvalTypeDBID := lookupTypeDBID(t, s, testutil.TestTypeIDSessionApproval)
	frustrationTypeDBID := lookupTypeDBID(t, s, testutil.TestTypeIDFrustrationSignal)

	// Create session-level annotation.
	sid := testSessionID
	conf := 0.95
	reason := "looks good"
	_, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: approvalTypeDBID,
		Value:            "approve",
		Confidence:       &conf,
		Reason:           &reason,
	})
	if err != nil {
		t.Fatalf("create session annotation: %v", err)
	}

	// Create entry-level annotation (span [0, 3)).
	_, err = s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		EntryTarget: &store.EntryTarget{
			SessionID:  testSessionID,
			EntryIndex: 0,
			EndIndex:   3,
		},
		AnnotatorID:      annotatorID,
		AnnotationTypeID: frustrationTypeDBID,
		Value:            "detected",
	})
	if err != nil {
		t.Fatalf("create entry annotation: %v", err)
	}

	// Export and verify.
	results, err := export.ExportAnnotations(ctx, s, testSessionID)
	if err != nil {
		t.Fatalf("ExportAnnotations: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 annotations, got %d", len(results))
	}

	// Find session-level and entry-level annotations.
	var sessionAnn, entryAnn *export.ExportedAnnotation
	for i := range results {
		if results[i].TypeID == testutil.TestTypeIDSessionApproval {
			sessionAnn = &results[i]
		}
		if results[i].TypeID == testutil.TestTypeIDFrustrationSignal {
			entryAnn = &results[i]
		}
	}

	if sessionAnn == nil {
		t.Fatal("expected session-level annotation with type quality.session_approval")
	}
	if entryAnn == nil {
		t.Fatal("expected entry-level annotation with type quality.frustration_signal")
	}

	// Session-level: verify field mapping.
	if sessionAnn.SessionID != testSessionID {
		t.Errorf("session annotation SessionID = %q; want %q", sessionAnn.SessionID, testSessionID)
	}
	if sessionAnn.Value != "approve" {
		t.Errorf("session annotation Value = %q; want %q", sessionAnn.Value, "approve")
	}
	if sessionAnn.Annotator != testAnnotatorName {
		t.Errorf("session annotation Annotator = %q; want %q", sessionAnn.Annotator, testAnnotatorName)
	}
	if sessionAnn.AnnotatorKind != schema.AnnotatorHuman.String() {
		t.Errorf("session annotation AnnotatorKind = %q; want %q", sessionAnn.AnnotatorKind, schema.AnnotatorHuman.String())
	}
	if sessionAnn.Confidence == nil || *sessionAnn.Confidence != 0.95 {
		t.Errorf("session annotation Confidence = %v; want 0.95", sessionAnn.Confidence)
	}
	if sessionAnn.Reason != "looks good" {
		t.Errorf("session annotation Reason = %q; want %q", sessionAnn.Reason, "looks good")
	}
	if sessionAnn.StartEntry != nil {
		t.Errorf("session annotation StartEntry should be nil; got %v", sessionAnn.StartEntry)
	}
	if sessionAnn.EndEntry != nil {
		t.Errorf("session annotation EndEntry should be nil; got %v", sessionAnn.EndEntry)
	}
	if sessionAnn.CreatedAt == 0 {
		t.Error("session annotation CreatedAt should be non-zero")
	}

	// Entry-level: verify entry indices are set.
	if entryAnn.StartEntry == nil || *entryAnn.StartEntry != 0 {
		t.Errorf("entry annotation StartEntry = %v; want 0", entryAnn.StartEntry)
	}
	if entryAnn.EndEntry == nil || *entryAnn.EndEntry != 3 {
		t.Errorf("entry annotation EndEntry = %v; want 3", entryAnn.EndEntry)
	}
	if entryAnn.Value != "detected" {
		t.Errorf("entry annotation Value = %q; want %q", entryAnn.Value, "detected")
	}
}

// TestExportAnnotations_OptionalFieldsNil verifies that annotations without
// confidence or reason produce nil/empty values in the exported record (omitted
// in JSON via omitempty).
func TestExportAnnotations_OptionalFieldsNil(t *testing.T) {
	s := storetest.Open(t)
	ctx := context.Background()

	storetest.SeedSession(t, s, testSessionID)

	annotatorID := seedAnnotator(t, s)
	approvalTypeDBID := lookupTypeDBID(t, s, testutil.TestTypeIDSessionApproval)

	sid := testSessionID
	_, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: approvalTypeDBID,
		Value:            "deny",
		// No Confidence, no Reason.
	})
	if err != nil {
		t.Fatalf("create annotation: %v", err)
	}

	results, err := export.ExportAnnotations(ctx, s, testSessionID)
	if err != nil {
		t.Fatalf("ExportAnnotations: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 annotation, got %d", len(results))
	}

	ann := results[0]
	if ann.Confidence != nil {
		t.Errorf("expected nil Confidence, got %v", ann.Confidence)
	}
	if ann.Reason != "" {
		t.Errorf("expected empty Reason, got %q", ann.Reason)
	}

	// Verify omitempty works in JSON serialization.
	b, err := json.Marshal(ann)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	jsonStr := string(b)
	if bytes.Contains(b, []byte(`"confidence"`)) {
		t.Errorf("expected confidence to be omitted from JSON; got: %s", jsonStr)
	}
	if bytes.Contains(b, []byte(`"reason"`)) {
		t.Errorf("expected reason to be omitted from JSON; got: %s", jsonStr)
	}
}

// TestExportAnnotations_SupersededExcluded verifies that superseded annotations
// are NOT returned by ExportAnnotations.
func TestExportAnnotations_SupersededExcluded(t *testing.T) {
	s := storetest.Open(t)
	ctx := context.Background()

	storetest.SeedSession(t, s, testSessionID)

	annotatorID := seedAnnotator(t, s)
	approvalTypeDBID := lookupTypeDBID(t, s, testutil.TestTypeIDSessionApproval)

	sid := testSessionID

	// Create original annotation.
	oldID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: approvalTypeDBID,
		Value:            "deny",
	})
	if err != nil {
		t.Fatalf("create original annotation: %v", err)
	}

	// Create replacement annotation.
	newID, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: approvalTypeDBID,
		Value:            "approve",
	})
	if err != nil {
		t.Fatalf("create replacement annotation: %v", err)
	}

	// Supersede the original.
	if err := s.SupersedeAnnotation(ctx, oldID, newID); err != nil {
		t.Fatalf("supersede annotation: %v", err)
	}

	// Export: should only contain the replacement.
	results, err := export.ExportAnnotations(ctx, s, testSessionID)
	if err != nil {
		t.Fatalf("ExportAnnotations: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 annotation (superseded excluded), got %d", len(results))
	}
	if results[0].Value != "approve" {
		t.Errorf("expected surviving annotation value %q, got %q", "approve", results[0].Value)
	}
}

// TestExportAnnotations_EmptySession verifies that exporting annotations for a
// session with no annotations returns an empty (non-nil) slice.
func TestExportAnnotations_EmptySession(t *testing.T) {
	s := storetest.Open(t)
	ctx := context.Background()

	storetest.SeedSession(t, s, testSessionID)

	results, err := export.ExportAnnotations(ctx, s, testSessionID)
	if err != nil {
		t.Fatalf("ExportAnnotations: %v", err)
	}

	if results == nil {
		t.Fatal("expected non-nil empty slice, got nil")
	}
	if len(results) != 0 {
		t.Errorf("expected 0 annotations, got %d", len(results))
	}
}

// TestExportAnnotations_JSONLRoundTrip writes exported annotations as JSONL,
// reads them back line by line, and verifies the round-trip is lossless.
func TestExportAnnotations_JSONLRoundTrip(t *testing.T) {
	s := storetest.Open(t)
	ctx := context.Background()

	storetest.SeedSession(t, s, testSessionID)
	// Seed a single entry at index 0 for the entry-level annotation.
	seedSessionEntries(t, s, testSessionID, 1)

	annotatorID := seedAnnotator(t, s)
	approvalTypeDBID := lookupTypeDBID(t, s, testutil.TestTypeIDSessionApproval)
	frustrationTypeDBID := lookupTypeDBID(t, s, testutil.TestTypeIDFrustrationSignal)

	sid := testSessionID
	conf := 0.8
	reason := "test reason"

	// Create session-level annotation with optional fields.
	_, err := s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		SessionID:        &sid,
		AnnotatorID:      annotatorID,
		AnnotationTypeID: approvalTypeDBID,
		Value:            "approve",
		Confidence:       &conf,
		Reason:           &reason,
	})
	if err != nil {
		t.Fatalf("create session annotation: %v", err)
	}

	// Create entry-level annotation without optional fields.
	_, err = s.CreateAnnotation(ctx, store.CreateAnnotationParams{
		EntryTarget: &store.EntryTarget{
			SessionID:  testSessionID,
			EntryIndex: 0,
			EndIndex:   1,
		},
		AnnotatorID:      annotatorID,
		AnnotationTypeID: frustrationTypeDBID,
		Value:            "not_detected",
	})
	if err != nil {
		t.Fatalf("create entry annotation: %v", err)
	}

	originals, err := export.ExportAnnotations(ctx, s, testSessionID)
	if err != nil {
		t.Fatalf("ExportAnnotations: %v", err)
	}
	if len(originals) != 2 {
		t.Fatalf("expected 2 annotations, got %d", len(originals))
	}

	// Serialize to JSONL buffer.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, ann := range originals {
		if err := enc.Encode(ann); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}

	// Read back line by line and deserialize.
	scanner := bufio.NewScanner(&buf)
	var roundTripped []export.ExportedAnnotation
	for scanner.Scan() {
		var ann export.ExportedAnnotation
		if err := json.Unmarshal(scanner.Bytes(), &ann); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		roundTripped = append(roundTripped, ann)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	if len(roundTripped) != len(originals) {
		t.Fatalf("round-trip count: got %d, want %d", len(roundTripped), len(originals))
	}

	// Compare each annotation by re-serializing to JSON (canonical comparison).
	for i := range originals {
		origJSON, _ := json.Marshal(originals[i])
		rtJSON, _ := json.Marshal(roundTripped[i])
		if string(origJSON) != string(rtJSON) {
			t.Errorf("annotation %d mismatch:\n  original:    %s\n  round-trip:  %s", i, origJSON, rtJSON)
		}
	}
}
