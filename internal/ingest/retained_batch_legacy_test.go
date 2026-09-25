package ingest_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestRetainedBatchLegacyPreviewStorable pins the SLICE-5 integration check:
// a retained-batch reindex of old wholly-legacy rows must stay
// preview-storable, while mixed legacy+valid rows refuse (corruption outranks
// compatibility) without destroying the prior preview. Wholly-absent sets
// write through the honest legacy-preview path; mixed sets return the legacy
// sentinel as an integrity refusal; the stored preview remains readable after
// close/reopen.
func TestRetainedBatchLegacyPreviewStorable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dbPath := storetest.CopyGoldenDB(t)
	db, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := ingest.NewSessionID(testutil.TestSessionUUID)
	if err != nil {
		t.Fatal(err)
	}
	storetest.SeedSession(t, db, sid.String())

	legacyPosition := ingest.UnknownSourcePosition{Line: 1, SourceID: "legacy-stream"}
	legacy, err := ingest.NewRetainedUnknown(ingest.HarnessClaudeCode, "record", "future", legacyPosition, json.RawMessage(`{"type":"future","n":0}`))
	if err != nil {
		t.Fatal(err)
	}
	legacyCarrier, err := ingest.RetainedUnknownEntry(sid, 0, legacy)
	if err != nil {
		t.Fatal(err)
	}
	versions := testutil.HarvesterVersionsForSeed(t, ingest.HarnessClaudeCode)

	// Wholly-legacy rows stay preview-storable through the honest preview path.
	previewWrite := ingest.SessionEntryWrite{
		SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{legacyCarrier}},
		IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
		CaptureRevision: 1, RequireFullContent: false,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourceNewIngest,
			CaptureFormat: ingest.ContentCaptureFormatLegacyPreviewOnly,
			FailureCode:   ingest.ContentCaptureLegacyPreviewOnly,
		},
	}
	written := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{previewWrite})
	if len(written) != 1 || written[0].Err != nil {
		t.Fatalf("wholly-legacy preview write refused: %+v", written)
	}

	// Fresh-policy assessment of wholly-legacy rows refuses recertification
	// with the legacy sentinel, without touching storage.
	if _, err := ingest.AssessCapture(ingest.V1CaptureFacts(
		ingest.HarnessClaudeCode, indexformat.V1{Entries: []schema.SessionEntry{legacyCarrier}}, true, false,
	)); err == nil {
		t.Fatal("fresh assessment certified wholly-legacy evidence; want refusal")
	} else if !errors.Is(err, ingest.ErrUnknownPositionUnavailable) {
		t.Fatalf("fresh refusal is not the legacy sentinel: %v", err)
	}

	// Mixed legacy+valid rows refuse: corruption outranks compatibility, and
	// the prior preview is preserved.
	validPosition := ingest.UnknownSourcePosition{
		Line: 2, SourceID: "legacy-stream",
		Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: 1, Position: 3},
	}
	valid, err := ingest.NewRetainedUnknown(ingest.HarnessClaudeCode, "record", "future", validPosition, json.RawMessage(`{"type":"future","n":1}`))
	if err != nil {
		t.Fatal(err)
	}
	validCarrier, err := ingest.RetainedUnknownEntry(sid, 1, valid)
	if err != nil {
		t.Fatal(err)
	}
	mixed := []schema.SessionEntry{legacyCarrier, validCarrier}
	if _, err := ingest.AssessCapture(ingest.V1CaptureFacts(
		ingest.HarnessClaudeCode, indexformat.V1{Entries: mixed}, true, false,
	)); err == nil {
		t.Fatal("fresh assessment certified mixed legacy evidence; want refusal")
	}
	mixedWrite := ingest.SessionEntryWrite{
		SessionID: sid, Result: indexformat.V1{Entries: mixed},
		IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
		CaptureRevision: 2, RequireFullContent: false,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourceNewIngest,
			CaptureFormat: ingest.ContentCaptureFormatLegacyPreviewOnly,
			FailureCode:   ingest.ContentCaptureLegacyPreviewOnly,
		},
	}
	refused := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{mixedWrite})
	if len(refused) != 1 || refused[0].Err == nil {
		t.Fatalf("mixed legacy+valid preview write certified: %+v", refused)
	}

	// The stored preview survives close/reopen and stays readable; a corrupt
	// follow-up cannot replace it.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db2, err := store.Open(dbPath, store.WithSkipMigrations())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	reopened, err := db2.ListEntries(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened) == 0 {
		t.Fatal("preview rows missing after reopen")
	}
	corruptExtra := `{"retainedUnknown":{"harness":"claude-code","namespace":42}}`
	corrupt := schema.SessionEntry{SessionID: schema.SessionID(sid.String()), EntryIndex: 9, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, Extra: &corruptExtra}
	corruptRefused := db2.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{legacyCarrier, corrupt}},
		IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
		CaptureRevision: 2, RequireFullContent: false,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status: ingest.ContentCaptureIncomplete, SourceAuthority: ingest.ContentSourceNewIngest,
			CaptureFormat: ingest.ContentCaptureFormatLegacyPreviewOnly,
			FailureCode:   ingest.ContentCaptureLegacyPreviewOnly,
		},
	}})
	if len(corruptRefused) != 1 || corruptRefused[0].Err == nil {
		t.Fatalf("corrupt mixed follow-up certified: %+v", corruptRefused)
	}
	still, err := db2.ListEntries(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(still) != len(reopened) {
		t.Fatal("corrupt follow-up replaced prior preview rows")
	}
}
