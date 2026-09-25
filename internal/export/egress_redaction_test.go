package export_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// failingExportRedactor returns a non-string shape to force a redaction failure.
type failingExportRedactor struct{}

func (failingExportRedactor) RedactJSON(any) any { return 42 }

var _ redact.JSONRedactor = failingExportRedactor{}

// TestExportBaselineRedactsSecrets pins that export emits the baseline-redacted
// form: stored evidence stays raw, exported bytes carry the standard-engine
// redaction, and the two differ exactly on the secret.
func TestExportBaselineRedactsSecrets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	sessionID := testutil.TestSessionUUID
	storetest.SeedSession(t, s, sessionID)
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	secret := "ghp_" + strings.Repeat("A", 36)
	rawPayload := `{"type":"future","token":"` + secret + `"}`
	position := ingest.UnknownSourcePosition{Line: 2, Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: 1, Position: 3}}
	record, err := ingest.NewRetainedUnknown(ingest.HarnessClaudeCode, "record", "future", position, json.RawMessage(rawPayload))
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := ingest.RetainedUnknownEntry(sid, 1, record)
	if err != nil {
		t.Fatal(err)
	}
	versions := testutil.HarvesterVersionsForSeed(t, ingest.HarnessClaudeCode)
	written := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{carrier}},
		IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
		CaptureRevision: 1, RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, CaptureFormat: ingest.ContentCaptureFormatFull, SourceAuthority: ingest.ContentSourceNewIngest},
	}})
	if len(written) != 1 || written[0].Err != nil {
		t.Fatalf("seed retained capture: %+v", written)
	}
	// Stored evidence stays raw.
	stored, err := ingest.RetainedUnknownOf(carrier)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored[0].Payload) != rawPayload {
		t.Fatal("stored evidence is not raw")
	}
	exported, err := export.ExportSession(ctx, s, testutil.NewMemFS(), sessionID)
	if err != nil {
		t.Fatalf("ExportSession: %v", err)
	}
	if len(exported.RetainedUnknown) != 1 {
		t.Fatalf("exported retained count: %d", len(exported.RetainedUnknown))
	}
	got := exported.RetainedUnknown[0].Payload
	if strings.Contains(got, secret) {
		t.Fatalf("exported bytes carry raw secret: %.200s", got)
	}
	if got == rawPayload {
		t.Fatal("exported bytes equal raw stored bytes; want baseline-redacted egress")
	}
	if !strings.Contains(got, "GITHUB_PAT") {
		t.Fatalf("exported bytes lack baseline placeholder: %.200s", got)
	}
}

// TestExportNilEngineRefuses pins fail-closed nil-engine refusal: a nil engine
// with retained records present refuses, and the error carries no raw bytes.
func TestExportNilEngineRefuses(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("B", 36)
	records := []schema.RetainedUnknownRecord{{
		SourceRef: "source-0", RecordIndex: 1, Position: 3,
		Namespace: "record", Kind: "future",
		Payload: `{"type":"future","token":"` + secret + `"}`,
	}}
	if _, err := export.RedactExportRetained(records, nil); err == nil {
		t.Fatal("RedactExportRetained(nil) with retained records succeeded; want refusal")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("nil-refusal error carries raw bytes: %v", err)
	}
	// Empty set with nil succeeds (nothing to protect).
	if _, err := export.RedactExportRetained(nil, nil); err != nil {
		t.Fatalf("empty redact with nil refused: %v", err)
	}
}

// TestExportRedactionFailureLeavesTargetUntouched pins atomic no-prefix
// failure: a failing baseline engine refuses the export, and the error carries
// no raw bytes. The file writer only writes on nil error (cmd_export atomic
// boundary), so no prefix or partial target is observable here.
func TestExportRedactionFailureLeavesTargetUntouched(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := storetest.Open(t)
	sessionID := testutil.TestSessionUUID
	storetest.SeedSession(t, s, sessionID)
	sid, err := ingest.NewSessionID(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	secret := "ghp_" + strings.Repeat("C", 36)
	rawPayload := `{"type":"future","token":"` + secret + `"}`
	position := ingest.UnknownSourcePosition{Line: 2, Public: &ingest.UnknownPublicPosition{SourceRef: "source-0", RecordIndex: 1, Position: 3}}
	record, err := ingest.NewRetainedUnknown(ingest.HarnessClaudeCode, "record", "future", position, json.RawMessage(rawPayload))
	if err != nil {
		t.Fatal(err)
	}
	carrier, err := ingest.RetainedUnknownEntry(sid, 1, record)
	if err != nil {
		t.Fatal(err)
	}
	versions := testutil.HarvesterVersionsForSeed(t, ingest.HarnessClaudeCode)
	written := s.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID: sid, Result: indexformat.V1{Entries: []schema.SessionEntry{carrier}},
		IndexVersion: versions.IndexVersion, IndexerVersion: versions.IndexerVersion,
		CaptureRevision: 1, RequireFullContent: true,
		ContentCapture: ingest.SessionContentCaptureWrite{Status: ingest.ContentCaptureIncomplete, FailureCode: ingest.ContentCaptureUnknownDataRetained, CaptureFormat: ingest.ContentCaptureFormatFull, SourceAuthority: ingest.ContentSourceNewIngest},
	}})
	if len(written) != 1 || written[0].Err != nil {
		t.Fatalf("seed retained capture: %+v", written)
	}
	before, err := export.ExportSession(ctx, s, testutil.NewMemFS(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, _ := json.Marshal(before)
	if _, err := export.ExportSessionWithRedactor(ctx, s, testutil.NewMemFS(), sessionID, failingExportRedactor{}); err == nil {
		t.Fatal("failing-engine export succeeded; want refusal")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("redaction-failure error carries raw bytes: %v", err)
	}
	after, err := export.ExportSession(ctx, s, testutil.NewMemFS(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, _ := json.Marshal(after)
	if string(beforeBytes) != string(afterBytes) {
		t.Fatal("failed export changed prior export bytes")
	}
}

// TestExportFinalSerializationCaps pins cap checks on actual emitted bytes:
// an aggregate that is small per record but too large as a document refuses,
// and the refusal names the transfer limit without raw bytes.
func TestExportFinalSerializationCaps(t *testing.T) {
	t.Parallel()
	// Individually small records aggregating over the 8 MiB document cap.
	var records []schema.RetainedUnknownRecord
	for i := 0; i < 40; i++ {
		records = append(records, schema.RetainedUnknownRecord{
			SourceRef: "source-0", RecordIndex: int64(i), Position: int64(i),
			Namespace: "record", Kind: "future",
			Payload: `{"type":"future","body":"` + strings.Repeat("x", 300_000) + `"}`,
		})
	}
	payload := &schema.SessionDetailPayload{
		ID:              testutil.TestSessionUUID,
		Harness:         schema.HarnessClaudeCode,
		Model:           "claude-sonnet-4-20250514",
		RetainedUnknown: records,
		Diagnostics:     &schema.InterpretationDiagnostics{Partial: true},
		Turns:           []schema.TurnDetail{{Index: 0, Role: schema.RoleAssistant, Depth: 0, Content: "closing"}},
	}
	if err := export.ValidateExportPayload(payload); err == nil {
		t.Fatal("aggregate over-cap export validated; want refusal on actual emitted bytes")
	} else if !strings.Contains(err.Error(), "transfer limit") && !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("cap refusal does not name the transfer limit: %v", err)
	}
}
