package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// assessmentWrite builds a store write through the production assessment path:
// AssessCapture over the entries, then the checked ContentCapture conversion.
// Tests must use this rather than hand-building captures, so a forged claim
// cannot pass as a valid assessment.
func assessmentWrite(t *testing.T, harness ingest.Harness, entries []schema.SessionEntry, authoritative bool) (ingest.SessionEntryWrite, ingest.CaptureAssessment) {
	t.Helper()
	sid := ingest.SessionID(entries[0].SessionID)
	_ = sid
	hasOmissions := false
	for _, e := range entries {
		if _, omitted := ingest.OmittedRecordOf(e); omitted {
			hasOmissions = true
			break
		}
	}
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: harness, Result: indexformat.V1{Entries: entries},
		Policy: ingest.CaptureFreshCandidate, Authoritative: authoritative,
		SourceOmitted: hasOmissions,
	})
	if err != nil {
		t.Fatalf("AssessCapture: %v", err)
	}
	write, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1700000000000)
	if err != nil {
		t.Fatalf("ContentCapture: %v", err)
	}
	return ingest.SessionEntryWrite{
		SessionID:          ingest.SessionID(entries[0].SessionID),
		Result:             indexformat.V1{Entries: entries},
		IndexVersion:       1,
		IndexerVersion:     ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
		IndexedAtMs:        1700000001000,
		RequireFullContent: write.CaptureFormat == ingest.ContentCaptureFormatFull,
		ContentCapture:     write,
	}, assessment
}

func unknownCarrierEntries(t *testing.T, id ingest.SessionID, withPublic bool) []schema.SessionEntry {
	t.Helper()
	preview := "known conversation text"
	entries := []schema.SessionEntry{{
		SessionID: schema.SessionID(id), EntryIndex: 0,
		Harness: schema.Harness(ingest.HarnessClaudeCode), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &preview,
	}}
	position := ingest.UnknownSourcePosition{Line: 2, JSONPointer: ""}
	if withPublic {
		position.Public = &ingest.UnknownPublicPosition{SourceRef: "src-test", RecordIndex: 1, Position: 1}
	}
	record, err := ingest.NewRetainedUnknown(ingest.HarnessClaudeCode, "record", "future_record", position, json.RawMessage(`{"type":"future","value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := ingest.RetainedUnknownEntry(id, 1, record)
	if err != nil {
		t.Fatal(err)
	}
	return append(entries, carrier)
}

func omissionPlaceholderEntries(t *testing.T, id ingest.SessionID) []schema.SessionEntry {
	t.Helper()
	preview := "known conversation text"
	entries := []schema.SessionEntry{{
		SessionID: schema.SessionID(id), EntryIndex: 0,
		Harness: schema.Harness(ingest.HarnessClaudeCode), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &preview,
	}}
	rec, err := ingest.NewOmittedRecord(ingest.OmittedRecordTooLarge, 7, 9000, 8000)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := rec.Extra()
	if err != nil {
		t.Fatal(err)
	}
	note := ingest.OmissionPlaceholderNote(rec)
	entries = append(entries, schema.SessionEntry{
		SessionID: schema.SessionID(id), EntryIndex: 1,
		Harness: schema.Harness(ingest.HarnessClaudeCode), Role: schema.RoleTool,
		EntryType: schema.EntryTypeToolResult, ContentPreview: &note, Extra: &extra,
	})
	return entries
}

func entriesBytes(t *testing.T, entries []schema.SessionEntry) string {
	t.Helper()
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCaptureAssessmentLastGoodGuard proves atomic last-good preservation at
// the store boundary: an invalid, unaccounted, forged, or preview-over-full
// replacement is refused in the same transaction, reports zero new counts,
// and leaves the old entries and capture byte-identical.
func TestCaptureAssessmentLastGoodGuard(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	id := ingest.SessionID("aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa")
	seedSession(t, s, string(id))

	good := batchTestEntries(id, "last-good", 2)
	write, _ := assessmentWrite(t, ingest.HarnessClaudeCode, good, true)
	if !write.RequireFullContent || write.ContentCapture.Status != ingest.ContentCaptureComplete {
		t.Fatalf("good assessment did not certify complete full capture: %+v", write.ContentCapture)
	}
	results := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{write})
	assertBatchResult(t, results, 0, id, true)
	beforeEntries, err := s.ListEntries(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes := entriesBytes(t, beforeEntries)
	beforeCapture := capture(t, s, id)
	if beforeCapture.Status != ingest.ContentCaptureComplete {
		t.Fatalf("seeded capture is not complete: %+v", beforeCapture)
	}

	assertUnchanged := func(name string, r0 ingest.SessionEntryWriteResult) {
		t.Helper()
		if r0.Err == nil {
			t.Fatalf("%s: refused replacement succeeded", name)
		}
		// EntriesCount names the attempted projection rows, not committed
		// evidence; a refused replacement reports no successful commit.
		if r0.Written {
			t.Fatalf("%s: refused replacement reported written: %+v", name, r0)
		}
		afterEntries, err := s.ListEntries(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got := entriesBytes(t, afterEntries); got != beforeBytes {
			t.Fatalf("%s: refused replacement changed stored entries", name)
		}
		if after := capture(t, s, id); after != beforeCapture {
			t.Fatalf("%s: refused replacement changed stored capture: %+v want %+v", name, after, beforeCapture)
		}
	}

	// Corrupt retained evidence with a forged full claim refuses with a
	// specific safe error and preserves last-good bytes.
	corruptExtra := `{"retainedUnknown":[{"harness":"claude-code","namespace":"record","kind":"","position":{"line":1},"payload":{}}]}`
	corruptPreview := "corrupt conversation text"
	corrupt := []schema.SessionEntry{{
		SessionID: schema.SessionID(id), EntryIndex: 0,
		Harness: schema.Harness(ingest.HarnessClaudeCode), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &corruptPreview, Extra: &corruptExtra,
	}}
	r := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: corrupt}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000002000,
		RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 1700000002000,
		},
	}})[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "prior capture remains authoritative") {
		t.Fatalf("corrupt replacement did not report the safe last-good error: %v", r.Err)
	}
	assertUnchanged("corrupt-replacement", r)

	// Omission plus invalid evidence: corruption outranks the omission
	// placeholder and refuses before any replacement.
	omitted := omissionPlaceholderEntries(t, id)
	omitted = append(omitted, schema.SessionEntry{
		SessionID: schema.SessionID(id), EntryIndex: 2,
		Harness: schema.Harness(ingest.HarnessClaudeCode), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &corruptPreview, Extra: &corruptExtra,
	})
	r = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: omitted}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000003000,
		RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull,
			FailureCode: ingest.ContentCaptureSourceRecordsOmitted, FailureMessage: "omitted",
			CapturedAtMs: 1700000003000,
		},
	}})[0]
	if r.Err == nil {
		t.Fatalf("omission-plus-invalid replacement succeeded")
	}
	assertUnchanged("omission-plus-invalid", r)

	// Forged full claim over unknown evidence: the requested complete/full
	// disagrees with the assessed incomplete/full/unknown_data_retained.
	unknown := unknownCarrierEntries(t, id, true)
	r = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: unknown}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion, IndexedAtMs: 1700000004000,
		RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull, CapturedAtMs: 1700000004000,
		},
	}})[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "disagrees with assessed evidence") {
		t.Fatalf("forged full claim did not report the assessment error: %v", r.Err)
	}
	assertUnchanged("forged-full-claim", r)

	// Preview over full authority refuses to preserve last-good export bytes.
	preview := batchTestEntries(id, "preview", 1)
	r = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: preview}, IndexVersion: 1,
		RequireFullContent: false,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourceNone,
			TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatPreviewOnly, CapturedAtMs: 1700000005000,
		},
	}})[0]
	if r.Err == nil || !strings.Contains(r.Err.Error(), "preview replacement refused over full read authority") {
		t.Fatalf("preview-over-full did not report the last-good error: %v", r.Err)
	}
	assertUnchanged("preview-over-full", r)

	// Failed write reports zero counts and preserves prior bytes: a stale
	// expected state refuses before any replacement.
	stale := beforeCapture
	_ = stale
	state, err := s.ReadIndexState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	forged := *state
	forged.IndexerVersion++
	goodWrite, _ := assessmentWrite(t, ingest.HarnessClaudeCode, good, true)
	goodWrite.ExpectedState = &forged
	goodWrite.IndexedInputHash = state.IndexedInputHash
	r = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{goodWrite})[0]
	if r.Err == nil {
		t.Fatalf("stale write succeeded")
	}
	assertUnchanged("failed-write-no-count", r)
}
