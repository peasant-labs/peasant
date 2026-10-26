package push_test

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// This file proves the publication half of the coexistence read contract over
// the real store: a session that keeps a newer incomplete attempt is refused by
// the default publication read with the actionable coexistence reason, and the
// explicit last-good selection uploads the last-good authority instead.

// lastGoodPushStore opens a generation-capable store over a fresh golden copy.
func lastGoodPushStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := store.NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(dir, "generations.db")
	storetest.CopyGoldenTo(t, destPath)
	db, err := store.Open(
		destPath,
		store.WithSkipMigrations(),
		store.WithPoolSize(2),
		store.WithIndexFormats(store.V2IndexFormat()),
		store.WithGenerationArtifacts(artifacts, locker),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// lastGoodPushSeed seeds a complete full authority (A) plus one latest-attempt
// incomplete preview attempt (B) over it.
func lastGoodPushSeed(t *testing.T, db *store.Store, sid schema.SessionID, authorityText, previewText string) {
	t.Helper()
	authority, blobs := testutil.CompleteGeneration(t, sid, "gen_push_a", authorityText)
	meta := authority.Generation.Metadata
	storetest.SeedGenerationPublication(t, db, &meta, authority, blobs)

	identity := strings.Repeat("d", 64)
	lastGoodPushExec(t, db, `UPDATE sessions SET artifact_hash = ? WHERE session_id = ?`, identity, string(sid))
	preview, previewBlobs := testutil.CompleteGeneration(t, sid, "gen_push_b", previewText)
	outcome, err := db.ActivateGeneration(context.Background(), store.GenerationActivation{
		Generation:     preview,
		Blobs:          previewBlobs,
		IndexerVersion: 18,
		IndexedAtMs:    2,
		ContentCapture: ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureIncomplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatPreviewOnly,
			FailureCode:      ingest.ContentCaptureSourceRecordsOmitted,
			FailureMessage:   "oversized source records were omitted without a stand-in",
			CapturedAtMs:     2,
		},
	})
	if err != nil {
		t.Fatalf("retain latest attempt: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("latest-attempt disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}
}

func lastGoodPushExec(t *testing.T, db *store.Store, statement string, args ...any) {
	t.Helper()
	conn, err := db.Pool().Take(context.Background())
	if err != nil {
		t.Fatalf("pool take: %v", err)
	}
	defer db.Pool().Put(conn)
	if err := sqlitex.Execute(conn, statement, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("execute %q: %v", statement, err)
	}
}

// TestPublicationReadRefusesWithCoexistenceReasonAndLastGoodServesAuthority
// proves the publication exit end to end: the default read names the pending
// attempt and refuses with the coexistence reason, and the explicit last-good
// selection clears only that refusal and hydrates the last-good authority.
func TestPublicationReadRefusesWithCoexistenceReasonAndLastGoodServesAuthority(t *testing.T) {
	db := lastGoodPushStore(t)
	sid := schema.SessionID("b1b1b1b1-1111-4111-8111-111111111111")
	const authorityText = "authority publication transcript"
	lastGoodPushSeed(t, db, sid, authorityText, "latest attempt preview transcript")
	ctx := context.Background()

	input, detail, err := push.LoadPublicationInput(ctx, db, string(sid))
	if err != nil {
		t.Fatalf("default publication read: %v", err)
	}
	if !input.LatestAttemptPending {
		t.Fatal("the default publication read did not name the pending latest attempt")
	}
	if detail != nil {
		t.Fatal("the default publication read hydrated a transcript for a session it must refuse")
	}
	refusal := push.ValidatePublicationInput(input)
	if refusal == nil {
		t.Fatal("the default publication read passed validation for a session with a latest attempt")
	}
	for _, want := range []string{"latest capture is an incomplete preview", "re-index the complete source", "--last-good"} {
		if !strings.Contains(refusal.Error(), want) {
			t.Fatalf("coexistence refusal %q does not name %q", refusal.Error(), want)
		}
	}

	selected, selectedDetail, err := push.LoadPublicationInputSelected(ctx, db, string(sid), ingest.SessionReadSelection{LastGood: true})
	if err != nil {
		t.Fatalf("explicit last-good publication read: %v", err)
	}
	if selected.LatestAttemptPending {
		t.Fatal("the explicit last-good read kept the latest-attempt refusal")
	}
	if err := push.ValidatePublicationInput(selected); err != nil {
		t.Fatalf("the explicit last-good read did not pass publication validation: %v", err)
	}
	if selectedDetail == nil {
		t.Fatal("the explicit last-good read hydrated no transcript")
	}
	served := false
	for i := range selectedDetail.Turns {
		if strings.Contains(selectedDetail.Turns[i].Content, authorityText) {
			served = true
		}
		if strings.Contains(selectedDetail.Turns[i].Content, "latest attempt preview transcript") {
			t.Fatalf("the explicit last-good read served the newer incomplete attempt: %+v", selectedDetail.Turns[i])
		}
	}
	if !served {
		t.Fatalf("the explicit last-good read did not serve the last-good authority: %+v", selectedDetail.Turns)
	}
}

// TestPipelineLastGoodPublishesAuthorityOverLatestAttempt pins the hop the
// CLI cannot: PipelineConfig.LastGood is what makes readPublicationInput read
// the last-good authority. Without it the pipeline refuses the session (the
// negative control), and with it the uploaded transcript carries the authority
// content and never the newer incomplete attempt.
func TestPipelineLastGoodPublishesAuthorityOverLatestAttempt(t *testing.T) {
	db := lastGoodPushStore(t)
	sid := schema.SessionID("b2b2b2b2-2222-4222-8222-222222222222")
	const authorityText = "authority publication transcript"
	const previewText = "latest attempt preview transcript"
	lastGoodPushSeed(t, db, sid, authorityText, previewText)

	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatal(err)
	}
	publisher := &testutil.StubPublisher{
		StatusCode: 201,
		// The last-good authority detail carries graph evidence (a measured
		// input-submission count), so the receiver must advertise it or the
		// upload is refused before the read is ever exercised.
		SchemaVersionResp: &schema.SchemaVersionResponse{
			MinPushContractVersion: schema.PushContractVersion("0.0.1"),
			PushContractVersion:    defaults.PublishSchemaVersion,
			ContentCapabilities:    append([]schema.ContentCapability(nil), schema.AllContentCapabilities...),
		},
	}
	var output bytes.Buffer
	run := func(cfg push.PipelineConfig) (*push.PushResult, error) {
		cfg.FilterSessionIDs = []string{string(sid)}
		pipeline, err := push.NewPipeline(db, publisher, baseCreds(), baseTestConfig(), forbiddenPublicationFS{}, cfg, redactor, &output)
		if err != nil {
			t.Fatal(err)
		}
		return pipeline.Run(context.Background())
	}

	// Negative control: without LastGood the session is refused and nothing is
	// uploaded.
	result, runErr := run(push.PipelineConfig{Force: true})
	if runErr != nil {
		t.Fatalf("default run: %v", runErr)
	}
	if result.Errors == 0 || result.New != 0 {
		t.Fatalf("the default run did not refuse the session: %+v", result)
	}
	if len(publisher.Calls) != 0 {
		t.Fatalf("the default run uploaded a session it must refuse: %+v", publisher.Calls)
	}

	// With LastGood the last-good authority is uploaded.
	result, runErr = run(push.PipelineConfig{Force: true, LastGood: true})
	if runErr != nil {
		t.Fatalf("last-good run: %v", runErr)
	}
	if result.Errors != 0 || result.New != 1 {
		t.Fatalf("the last-good run did not upload the session: %+v (output: %s)", result, output.String())
	}
	if len(publisher.Calls) != 1 {
		t.Fatalf("last-good uploads = %d, want 1", len(publisher.Calls))
	}
	body := string(publisher.Calls[0].TranscriptBody)
	if !strings.Contains(body, authorityText) {
		t.Fatalf("the uploaded transcript does not carry the last-good authority content: %s", body)
	}
	if strings.Contains(body, previewText) {
		t.Fatalf("the uploaded transcript carries the newer incomplete attempt: %s", body)
	}
}
