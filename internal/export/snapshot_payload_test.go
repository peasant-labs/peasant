package export_test

import (
	"context"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// stubExportReader serves one canned snapshot through the production
// SnapshotReader contract so the export wiring test proves which production
// path ExportSession selects.
type stubExportReader struct {
	snapshot  indexformat.ReadSnapshot
	err       error
	supported bool
}

func (s stubExportReader) WithSessionSnapshot(_ context.Context, _ schema.SessionID, fn func(indexformat.ReadSnapshot) error) error {
	if s.err != nil {
		return s.err
	}
	return fn(s.snapshot)
}

func (s stubExportReader) GenerationSnapshotsSupported() bool { return s.supported }

type stubExportResolver struct {
	blobs   map[schema.SourceEntryRef][]byte
	corrupt schema.SourceEntryRef
}

func (s stubExportResolver) ReadFullContent(_ context.Context, _ schema.SessionID, _ string, record indexformat.ContentRecord) ([]byte, error) {
	if record.Ref == s.corrupt {
		return nil, errors.New("stub resolver: managed content fails its integrity digest")
	}
	blob, ok := s.blobs[record.Ref]
	if !ok {
		return nil, errors.New("stub resolver: no captured blob")
	}
	return blob, nil
}

func exportWiringSnapshot() (indexformat.ReadSnapshot, map[schema.SourceEntryRef][]byte) {
	sessionID := schema.SessionID("45454545-4545-4545-4545-454545454548")
	harness := schema.HarnessCodex
	entries := []schema.SessionEntry{
		{SessionID: sessionID, EntryIndex: 0, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleUser, SourceEntryRef: "e_export_u"},
		{SessionID: sessionID, EntryIndex: 1, Harness: harness, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, SourceEntryRef: "e_export_a"},
	}
	inputCount := int64(1)
	snapshot := indexformat.ReadSnapshot{
		Session: schema.SessionDetailPayload{
			ID: string(sessionID), Harness: harness, TurnCount: 2,
			InputSubmissionCount: &inputCount, Purpose: schema.SessionPurposeInteraction,
		},
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: 1, SessionID: sessionID, ModelHarness: harness,
			Stats:   schema.SessionStats{TurnCount: 2, InputSubmissionCount: &inputCount},
			Purpose: schema.SessionPurposeInteraction,
		},
		GenerationID: "g-export",
		Completeness: indexformat.GenerationCompletenessComplete,
		IndexVersion: 2,
		Main:         indexformat.Partition{Entries: entries},
		Content: []indexformat.ContentRecord{
			{Ref: "e_export_u", RelativeBlob: "c_export_u.blob", ByteLength: 12, Digest: "export"},
			{Ref: "e_export_a", RelativeBlob: "c_export_a.blob", ByteLength: 12, Digest: "export"},
		},
	}
	blobs := map[schema.SourceEntryRef][]byte{
		"e_export_u": []byte("export input"),
		"e_export_a": []byte("export reply"),
	}
	return snapshot, blobs
}

func TestExportSnapshotPayloadWiring(t *testing.T) {
	snapshot, blobs := exportWiringSnapshot()
	sessionID := snapshot.Metadata.SessionID

	payload, err := export.ExportSnapshotPayload(context.Background(), stubExportReader{snapshot: snapshot, supported: true}, stubExportResolver{blobs: blobs}, sessionID)
	if err != nil {
		t.Fatalf("snapshot export: %v", err)
	}
	if len(payload.Turns) != 2 || payload.Turns[0].Content != "export input" {
		t.Fatalf("snapshot export turns = %+v, want hydrated export turns", payload.Turns)
	}

	legacySnapshot := snapshot
	legacySnapshot.IndexVersion = 1
	legacySnapshot.GenerationID = ""
	legacySnapshot.Main = indexformat.Partition{}
	legacySnapshot.Content = nil
	if _, err := export.ExportSnapshotPayload(context.Background(), stubExportReader{snapshot: legacySnapshot, supported: true}, stubExportResolver{blobs: blobs}, sessionID); !errors.Is(err, transcript.ErrLegacySnapshot) {
		t.Fatalf("legacy snapshot must report the legacy path, got %v", err)
	}

	if _, err := export.ExportSnapshotPayload(context.Background(), stubExportReader{snapshot: snapshot, supported: true}, stubExportResolver{blobs: blobs, corrupt: "e_export_u"}, sessionID); err == nil {
		t.Fatal("corrupt managed content must fail the export")
	}
}
