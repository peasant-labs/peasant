package push_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
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

// lastGoodPushGeneration builds a complete generation whose single user entry
// carries the supplied text, so the hydrated detail names which generation the
// publication read served.
func lastGoodPushGeneration(t *testing.T, sid schema.SessionID, genID, text string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = schema.HarnessClaudeCode
	meta.HostSlug = testutil.TestHostSlug
	meta.Model = testutil.TestModel
	meta.CWD = "/home/test/myapp"
	meta.Project = ingest.ProjectInfo{Hash: testutil.TestProjectHash, Name: "myapp", FilePath: "/home/test/myapp"}
	ingested := int64(1700000060000)
	meta.Timestamp = ingest.TimestampInfo{Start: 1700000000000, End: 1700000060000, Ingested: &ingested}
	meta.Source = ingest.SourceInfo{FilePath: "/source/file.jsonl", Format: ingest.SourceFormatJSONL}
	meta.Stats = ingest.StatsInfo{TurnCount: 1, DurationMs: 60000, TokensIn: 1, TokensOut: 1}
	submissions := int64(1)
	meta.Stats.InputSubmissionCount = &submissions

	ref := schema.SourceEntryRef(genID + "_user")
	entry := schema.SessionEntry{
		SessionID: sid, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, SourceEntryRef: ref, ContentPreview: &text,
	}
	generation := indexformat.Generation{
		ID:                   genID,
		Completeness:         indexformat.GenerationCompletenessComplete,
		Metadata:             meta,
		Main:                 indexformat.Partition{Entries: []schema.SessionEntry{entry}},
		Content:              []indexformat.ContentRecord{{Ref: ref}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
	}
	return indexformat.V2{Generation: generation}, map[schema.SourceEntryRef][]byte{ref: []byte(text)}
}

// lastGoodPushSeed seeds a complete full authority (A) plus one retained
// incomplete preview attempt (B) over it.
func lastGoodPushSeed(t *testing.T, db *store.Store, sid schema.SessionID, authorityText, previewText string) {
	t.Helper()
	authority, blobs := lastGoodPushGeneration(t, sid, "gen_push_a", authorityText)
	meta := authority.Generation.Metadata
	storetest.SeedGenerationPublication(t, db, &meta, authority, blobs)

	identity := strings.Repeat("d", 64)
	lastGoodPushExec(t, db, `UPDATE sessions SET artifact_hash = ? WHERE session_id = ?`, identity, string(sid))
	preview, previewBlobs := lastGoodPushGeneration(t, sid, "gen_push_b", previewText)
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
		t.Fatalf("retention disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
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
