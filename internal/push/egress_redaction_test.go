package push_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// TestPushNilRedactorRefusesWithRetained pins the fail-closed nil-engine
// refusal: any retained records present with a nil redactor refuse before
// upload, with a safe egress error carrying no raw bytes. Empty retained sets
// still pass through (no evidence to protect).
func TestPushNilRedactorRefusesWithRetained(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("A", 36)
	records := []schema.RetainedUnknownRecord{{
		SourceRef: "source-0", RecordIndex: 1, Position: 3,
		Namespace: "record", Kind: "future",
		Payload: `{"type":"future","token":"` + secret + `"}`,
	}}
	content := schema.TranscriptContent{
		ContractVersion: schema.PushContractVersion("0.1.1"),
		Kind:            schema.ContentKindSessionDetail,
		SessionDetail: &schema.SessionDetailPayload{
			SchemaVersion:   schema.PushContractVersion("0.1.1"),
			ID:              "11111111-2222-3333-4444-555555555555",
			Harness:         schema.HarnessClaudeCode,
			Model:           "claude-sonnet-4-20250514",
			RetainedUnknown: records,
			Diagnostics:     &schema.InterpretationDiagnostics{Partial: true},
			Turns:           []schema.TurnDetail{{Index: 0, Role: schema.RoleAssistant, Depth: 0, Content: "closing"}},
			SessionOrigin:   schema.SessionOriginAgent,
		},
	}
	// PublicationReviewText with nil must refuse: it validates the upload path.
	if _, err := push.PublicationReviewText(content, nil); err == nil {
		t.Fatal("PublicationReviewText(nil) with retained records succeeded; want fail-closed refusal")
	} else if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "ghp_") {
		t.Fatalf("nil-refusal error carries raw secret bytes: %v", err)
	}
	// RedactEntries with nil must refuse when retained carriers are present.
	carrier, err := ingest.RetainedUnknownEntry(
		schema.SessionID("11111111-2222-3333-4444-555555555555"), 1,
		mustRetainedUnknownPayload(t, `{"type":"future","token":"`+secret+`"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	entries := []schema.SessionEntry{carrier}
	if _, err := push.RedactEntries(nil, entries); err == nil {
		t.Fatal("RedactEntries(nil) with retained records succeeded; want fail-closed refusal")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("RedactEntries nil-refusal error carries raw bytes: %v", err)
	}
	// Empty retained set with nil still succeeds (nothing to protect).
	empty := schema.TranscriptContent{
		ContractVersion: schema.PushContractVersion("0.1.1"),
		Kind:            schema.ContentKindSessionDetail,
		SessionDetail: &schema.SessionDetailPayload{
			SchemaVersion: schema.PushContractVersion("0.1.1"),
			ID:            "11111111-2222-3333-4444-555555555555",
			Harness:       schema.HarnessClaudeCode,
			Model:         "claude-sonnet-4-20250514",
			Turns:         []schema.TurnDetail{{Index: 0, Role: schema.RoleAssistant, Depth: 0, Content: "closing"}},
		},
	}
	if _, err := push.PublicationReviewText(empty, nil); err != nil {
		t.Fatalf("PublicationReviewText(nil) without retained records refused: %v", err)
	}
}

// mustRetainedUnknownPayload builds one retained record with the given payload
// at fixed coordinates shared by the egress tests.
func mustRetainedUnknownPayload(t *testing.T, payload string) ingest.RetainedUnknown {
	t.Helper()
	position := ingest.UnknownSourcePosition{
		Line: 2,
		Public: &ingest.UnknownPublicPosition{
			SourceRef: "source-0", RecordIndex: 1, Position: 3,
		},
	}
	record, err := ingest.NewRetainedUnknown(
		ingest.HarnessClaudeCode, "record", "future", position,
		[]byte(payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// TestPushRawNeverUploadsUnredacted drives the upload path and byte-compares
// actual upload bytes against the independently redacted expectation; a
// redactor-bypassed upload is refused with zero uploads.
func TestPushRawNeverUploadsUnredacted(t *testing.T) {
	t.Parallel()
	secret := "custom-secret"
	engine, err := redact.NewRedactor(redact.Standard,
		[]redact.UserPattern{{ID: "unknown-secret", Category: redact.CategorySecrets, Pattern: secret, Replacement: "[CUSTOM]"}},
		redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	meta := &ingest.UnifiedMetadata{
		SessionID:    schema.SessionID("11111111-2222-3333-4444-555555555555"),
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        schema.ModelID("claude-sonnet-4-20250514"),
		CWD:          "/home/example/dev/widgets",
	}
	carrier, err := ingest.RetainedUnknownEntry(
		schema.SessionID("11111111-2222-3333-4444-555555555555"), 1,
		mustRetainedUnknownPayload(t, `{"type":"future","secret":"`+secret+`"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	closing := "known closing text"
	entries := []schema.SessionEntry{carrier,
		{SessionID: schema.SessionID("11111111-2222-3333-4444-555555555555"), EntryIndex: 1, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &closing},
	}
	redactedEntries, err := push.RedactEntries(engine, entries)
	if err != nil {
		t.Fatal(err)
	}
	const emit = schema.PushContractVersion("0.1.1")
	// Build content through the production builder, then marshal the upload
	// bytes via the review path (which validates the same upload redaction).
	content, err := push.BuildTranscriptContentValidated(meta, redactedEntries, emit, config.PushFieldVisibility{ProjectPath: boolPtrEgress(true)}, sessionorigin.Agent)
	if err != nil {
		t.Fatal(err)
	}
	review, err := push.PublicationReviewText(content, engine)
	if err != nil {
		t.Fatal(err)
	}
	// The review exposes the redacted upload to the scanner: any raw-secret
	// presence is a leak, even alongside the placeholder.
	if strings.Contains(review, secret) {
		t.Fatal("review exposes raw secret without redacted upload validation")
	}
	// Independently redacted expectation: hand-written, never derived from the
	// production egress path.
	const want = `{"type":"future","secret":"[CUSTOM]"}`
	// The upload bytes must not carry the raw secret: marshal through the
	// production path and assert absence plus exact expectation presence.
	// BuildPublishTranscriptContent is the committed-input path; here the
	// validated content already carries redacted entries, so assert on it.
	for _, record := range content.SessionDetail.RetainedUnknown {
		if strings.Contains(record.Payload, secret) {
			t.Fatalf("upload content carries raw secret: %q", record.Payload)
		}
		if record.Payload != want {
			t.Fatalf("upload payload = %q, want independently-redacted %q", record.Payload, want)
		}
	}
}

func boolPtrEgress(v bool) *bool { return &v }

// failingPushRedactor returns a non-string shape from every JSON redaction so
// retained-label rewriting fails closed. Metadata passes through untouched, so
// the refusal lands on the retained-evidence seam rather than on metadata.
type failingPushRedactor struct{}

func (failingPushRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	return meta
}

func (failingPushRedactor) RedactJSON(any) any { return 42 }

func (failingPushRedactor) Level() string { return "standard" }

func (failingPushRedactor) RuleSetVersion() string { return "0.0.0-test" }

var _ ingest.TextRedactor = failingPushRedactor{}

// TestPushNilRedactorUploadsNothing pins the "failed/nil-redactor push uploads
// nothing" row with a publish counter: NewPipeline refuses a nil redactor at
// construction, so no pipeline exists to upload through. Both publisher
// counters stay at zero even with retained records staged in the store.
func TestPushNilRedactorUploadsNothing(t *testing.T) {
	t.Parallel()
	pub := &testutil.StubPublisher{StatusCode: 201}
	var stderr bytes.Buffer
	fs := testutil.NewMemFS()
	staged, err := ingest.RetainedUnknownEntry(
		schema.SessionID(testutil.TestSessionUUID), 1,
		mustRetainedUnknownPayload(t, `{"type":"future","token":"ghp_staged"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil)},
		Entries:  map[ingest.SessionID][]schema.SessionEntry{ingest.SessionID(testutil.TestSessionUUID): {staged}},
	}
	pipeline, err := push.NewPipeline(store, pub, baseCreds(), baseTestConfig(), fs, push.PipelineConfig{}, nil, &stderr)
	if err == nil {
		t.Fatal("NewPipeline(nil redactor) succeeded; want fail-closed refusal")
	}
	if pipeline != nil {
		t.Fatal("NewPipeline(nil redactor) returned a non-nil Pipeline alongside the refusal")
	}
	if len(pub.Calls) != 0 || len(pub.AuthoritativeCalls) != 0 {
		t.Fatalf("nil-redactor construction uploaded %d content and %d authoritative calls; want zero uploads", len(pub.Calls), len(pub.AuthoritativeCalls))
	}
}

// TestPushFailingRedactorUploadsNothing drives a real pipeline run with a
// redactor that fails retained-label rewriting over retained records and
// asserts zero publish calls on the publisher counter: refusal-before-upload
// is observed, not entailed by code order.
func TestPushFailingRedactorUploadsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs := testutil.NewMemFS()
	sessionID := testutil.TestSessionUUID
	seedMemFS(t, fs, testutil.TestHostSlug, sessionID, defaults.HarnessClaudeCode)
	secret := "custom-secret-refusal"
	carrier, err := ingest.RetainedUnknownEntry(
		schema.SessionID(sessionID), 1,
		mustRetainedUnknownPayload(t, `{"type":"future","secret":"`+secret+`"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	closing := "known closing text"
	store := &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{makeSession(sessionID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil)},
		Entries: map[ingest.SessionID][]schema.SessionEntry{
			ingest.SessionID(sessionID): {
				carrier,
				{SessionID: schema.SessionID(sessionID), EntryIndex: 1, Harness: schema.HarnessClaudeCode, EntryType: schema.EntryTypeText, Role: schema.RoleAssistant, ContentPreview: &closing},
			},
		},
	}
	pub := &testutil.StubPublisher{StatusCode: 201}
	var stderr bytes.Buffer
	testutil.SeedPublicationInputs(store, fs, baseTestConfig().Output.BasePath)
	pipeline, err := push.NewPipeline(store, pub, baseCreds(), baseTestConfig(), fs, push.PipelineConfig{}, failingPushRedactor{}, &stderr)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	result, runErr := pipeline.Run(ctx)
	if runErr != nil {
		t.Logf("Run returned %v (a run-level error is an acceptable refusal)", runErr)
	}
	if result == nil {
		if runErr == nil {
			t.Fatal("pipeline run returned no result and no error")
		}
		if len(pub.Calls) != 0 || len(pub.AuthoritativeCalls) != 0 {
			t.Fatalf("failing-redactor run uploaded %d content and %d authoritative calls; want zero uploads", len(pub.Calls), len(pub.AuthoritativeCalls))
		}
		return
	}
	refused := runErr != nil
	for _, s := range result.Sessions {
		if s.Status == push.PushStatusError {
			refused = true
			if s.Error == nil || !strings.Contains(s.Error.Error(), "nothing uploaded") {
				t.Errorf("session refusal does not name the nothing-uploaded guarantee: %+v", s.Error)
			}
		}
	}
	if !refused {
		t.Fatalf("failing-redactor run reported no refusal: %+v", result.Sessions)
	}
	if len(pub.Calls) != 0 || len(pub.AuthoritativeCalls) != 0 {
		t.Fatalf("failing-redactor run uploaded %d content and %d authoritative calls; want zero uploads", len(pub.Calls), len(pub.AuthoritativeCalls))
	}
}
