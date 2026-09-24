package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// Omission placeholder dimensions shared with the ingest assessment pins: the
// source line that overflowed, its byte size, and the limit it exceeded.
const (
	omissionPlaceholderLine  = 7
	omissionPlaceholderSize  = 9000
	omissionPlaceholderLimit = 8000
)

// lastGoodHarnesses exercises the guard across JSONL harnesses with retained
// evidence support. The unknown_public_source.yaml family extension (raw
// source parsing per harness, duplicate-member sources) stays with the codec
// lane; this guard proves the store boundary itself is harness-agnostic.
var lastGoodHarnesses = []ingest.Harness{
	ingest.HarnessClaudeCode,
	ingest.HarnessCodex,
	ingest.HarnessCursor,
}

// assessmentWrite builds a store write through the production assessment path:
// AssessCapture over the entries, then the checked ContentCapture conversion.
// Tests must use this rather than hand-building captures, so a forged claim
// cannot pass as a valid assessment.
func assessmentWrite(t *testing.T, harness ingest.Harness, entries []schema.SessionEntry, authoritative bool) (ingest.SessionEntryWrite, ingest.CaptureAssessment) {
	t.Helper()
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
		IndexerVersion:     ingest.HarvesterVersionRegistry[harness].IndexerVersion,
		IndexedAtMs:        1700000001000,
		RequireFullContent: write.CaptureFormat == ingest.ContentCaptureFormatFull,
		ContentCapture:     write,
	}, assessment
}

// seedHarnessSession inserts a minimal session row under the given harness so
// FK constraints on session_entries and session_metrics are satisfied.
func seedHarnessSession(t *testing.T, s *store.Store, sessionID string, harness ingest.Harness) {
	t.Helper()
	hash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	entry := makeStoreEntry(t, sessionID, hash, "github.com-test", harness, 1700000000000, 100, 50)
	if err := s.InsertSessions(context.Background(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatalf("seedHarnessSession: %v", err)
	}
}

// withEntryHarness restates plain helper entries under the subtest harness so
// the evidence owner, the session row, and the assessment facts agree.
func withEntryHarness(entries []schema.SessionEntry, harness ingest.Harness) []schema.SessionEntry {
	for i := range entries {
		entries[i].Harness = schema.Harness(harness)
	}
	return entries
}

func unknownCarrierEntries(t *testing.T, id ingest.SessionID, harness ingest.Harness, withPublic bool) []schema.SessionEntry {
	t.Helper()
	preview := "known conversation text"
	entries := []schema.SessionEntry{{
		SessionID: schema.SessionID(id), EntryIndex: 0,
		Harness: schema.Harness(harness), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &preview,
	}}
	position := ingest.UnknownSourcePosition{Line: 2, JSONPointer: ""}
	if withPublic {
		position.Public = &ingest.UnknownPublicPosition{SourceRef: "src-test", RecordIndex: 1, Position: 1}
	}
	record, err := ingest.NewRetainedUnknown(harness, "record", "future_record", position, json.RawMessage(`{"type":"future","value":1}`))
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := ingest.RetainedUnknownEntry(id, 1, record)
	if err != nil {
		t.Fatal(err)
	}
	return append(entries, carrier)
}

func omissionPlaceholderEntries(t *testing.T, id ingest.SessionID, harness ingest.Harness) []schema.SessionEntry {
	t.Helper()
	preview := "known conversation text"
	entries := []schema.SessionEntry{{
		SessionID: schema.SessionID(id), EntryIndex: 0,
		Harness: schema.Harness(harness), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &preview,
	}}
	rec, err := ingest.NewOmittedRecord(ingest.OmittedRecordTooLarge, omissionPlaceholderLine, omissionPlaceholderSize, omissionPlaceholderLimit)
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
		Harness: schema.Harness(harness), Role: schema.RoleTool,
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

// exportSessionBytes reads the session through the production export path
// (verified full content through EntriesToTurns/SessionToDetail). A refused
// replacement must leave these bytes identical, not just the stored entries:
// export derives through a distinct read path the entries check alone would
// miss. The filesystem argument is caller-compat only; export reads verified
// database content.
func exportSessionBytes(t *testing.T, s *store.Store, id ingest.SessionID) string {
	t.Helper()
	payload, err := export.ExportSession(context.Background(), s, testutil.NewMemFS(), string(id))
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal export payload: %v", err)
	}
	return string(b)
}

// TestCaptureAssessmentLastGoodGuard proves atomic last-good preservation at
// the store boundary across harnesses: an invalid, unaccounted, forged, or
// preview-over-full replacement is refused in the same transaction, reports
// zero new counts, and leaves the old entries, capture, and export bytes
// byte-identical.
func TestCaptureAssessmentLastGoodGuard(t *testing.T) {
	t.Parallel()
	for _, harness := range lastGoodHarnesses {
		t.Run(string(harness), func(t *testing.T) {
			t.Parallel()
			runCaptureAssessmentLastGoodGuard(t, harness)
		})
	}
}

func runCaptureAssessmentLastGoodGuard(t *testing.T, harness ingest.Harness) {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	id := ingest.SessionID(testutil.TestSessionUUID)
	seedHarnessSession(t, s, string(id), harness)
	// An unrelated session proves a refusal on one session never poisons
	// another session's commits through the shared batch path.
	other := ingest.SessionID(testutil.TestSessionUUID2)
	seedHarnessSession(t, s, string(other), harness)

	good := withEntryHarness(batchTestEntries(id, "last-good", 2), harness)
	write, _ := assessmentWrite(t, harness, good, true)
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
	beforeExport := exportSessionBytes(t, s, id)

	assertUnchanged := func(name string, r0 ingest.SessionEntryWriteResult) {
		t.Helper()
		if r0.Err == nil {
			t.Fatalf("%s: refused replacement succeeded", name)
		}
		if r0.Written {
			t.Fatalf("%s: refused replacement reported written: %+v", name, r0)
		}
		// EntriesCount names the attempted projection rows, not committed
		// evidence: entries_writer sets it from the projected entries even
		// when the write is refused after projection, so a refused
		// replacement reports no successful commit via Written, not via this
		// field. Zero new COMMITS are proven by the unchanged stored rows,
		// capture, and export bytes below. Stats must still be zero: no
		// writer branch ran on a refused replacement.
		if r0.Stats != (ingest.SessionEntryWriteStats{}) {
			t.Fatalf("%s: refused replacement recorded writer stats %+v, want zero", name, r0.Stats)
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
		if got := exportSessionBytes(t, s, id); got != beforeExport {
			t.Fatalf("%s: refused replacement changed export bytes", name)
		}
	}

	// Corrupt retained evidence with a forged full claim refuses with a
	// specific safe error and preserves last-good bytes.
	corruptExtra := fmt.Sprintf(`{"retainedUnknown":[{"harness":%q,"namespace":"record","kind":"","position":{"line":1},"payload":{}}]}`, string(harness))
	corruptPreview := "corrupt conversation text"
	corrupt := []schema.SessionEntry{{
		SessionID: schema.SessionID(id), EntryIndex: 0,
		Harness: schema.Harness(harness), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &corruptPreview, Extra: &corruptExtra,
	}}
	r := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: corrupt}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[harness].IndexerVersion, IndexedAtMs: 1700000002000,
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
	omitted := omissionPlaceholderEntries(t, id, harness)
	omitted = append(omitted, schema.SessionEntry{
		SessionID: schema.SessionID(id), EntryIndex: 2,
		Harness: schema.Harness(harness), Role: schema.RoleUser,
		EntryType: schema.EntryTypeText, ContentPreview: &corruptPreview, Extra: &corruptExtra,
	})
	r = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: omitted}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[harness].IndexerVersion, IndexedAtMs: 1700000003000,
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
	unknown := unknownCarrierEntries(t, id, harness, true)
	r = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: id, Result: indexformat.V1{Entries: unknown}, IndexVersion: 1,
		IndexerVersion: ingest.HarvesterVersionRegistry[harness].IndexerVersion, IndexedAtMs: 1700000004000,
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
	preview := withEntryHarness(batchTestEntries(id, "preview", 1), harness)
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
	state, err := s.ReadIndexState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	forged := *state
	forged.IndexerVersion++
	goodWrite, _ := assessmentWrite(t, harness, good, true)
	goodWrite.ExpectedState = &forged
	goodWrite.IndexedInputHash = state.IndexedInputHash
	r = s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{goodWrite})[0]
	if r.Err == nil {
		t.Fatalf("stale write succeeded")
	}
	assertUnchanged("failed-write-no-count", r)
	// A stale expected state refuses in validateIndexWriteOnConn, before the
	// format handler projects any rows, so unlike post-projection refusals
	// this mode reports zero attempted rows as well.
	if r.EntriesCount != 0 {
		t.Fatalf("failed-write-no-count: stale refusal reported %d attempted rows, want zero before projection", r.EntriesCount)
	}

	// The unrelated session still commits after the refusals above.
	otherGood := withEntryHarness(batchTestEntries(other, "unrelated", 2), harness)
	otherWrite, _ := assessmentWrite(t, harness, otherGood, true)
	otherResults := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{otherWrite})
	assertBatchResult(t, otherResults, 0, other, true)
	if got := capture(t, s, other); got.Status != ingest.ContentCaptureComplete {
		t.Fatalf("unrelated session capture = %q, want complete", string(got.Status))
	}
}
