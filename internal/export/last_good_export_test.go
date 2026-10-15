package export_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// This file proves the export half of the coexistence read contract: a session
// that keeps a newer incomplete attempt beside its last-good generation refuses
// a default export (never leaking the preserved legacy read), and the explicit
// last-good selection serves the last-good full authority in both lanes — the
// managed generation and the preserved certificate read.

// lastGoodPreviewCapture is the store write tuple for the unaccounted
// source-records omission preview the retention predicate reads.
func lastGoodPreviewCapture() ingest.SessionContentCaptureWrite {
	return ingest.SessionContentCaptureWrite{
		Status:           ingest.ContentCaptureIncomplete,
		SourceAuthority:  ingest.ContentSourceNewIngest,
		TranscriptOrigin: ingest.TranscriptOriginFile,
		CaptureFormat:    ingest.ContentCaptureFormatPreviewOnly,
		FailureCode:      ingest.ContentCaptureSourceRecordsOmitted,
		FailureMessage:   "oversized source records were omitted without a stand-in",
		CapturedAtMs:     2,
	}
}

// lastGoodOpenStore opens a generation-capable store over a fresh golden copy.
func lastGoodOpenStore(t *testing.T) *store.Store {
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

// lastGoodSeedAuthority seeds the session with a complete full generation (A)
// carrying the supplied authority text.
func lastGoodSeedAuthority(t *testing.T, db *store.Store, sid schema.SessionID, authorityText string) {
	t.Helper()
	generation, blobs := testutil.CompleteGeneration(t, sid, "gen_last_good_a", authorityText)
	meta := generation.Generation.Metadata
	storetest.SeedGenerationPublication(t, db, &meta, generation, blobs)
}

// lastGoodRetainPreview retains one incomplete preview attempt (B) over the
// seeded authority, carrying its own distinguishable preview text.
func lastGoodRetainPreview(t *testing.T, db *store.Store, sid schema.SessionID, previewText string) {
	t.Helper()
	generation, blobs := testutil.CompleteGeneration(t, sid, "gen_latest_attempt_b", previewText)
	identity := strings.Repeat("b", 64)
	lastGoodExec(t, db, `UPDATE sessions SET artifact_hash = ? WHERE session_id = ?`, identity, string(sid))
	outcome, err := db.ActivateGeneration(context.Background(), store.GenerationActivation{
		Generation:     generation,
		Blobs:          blobs,
		IndexerVersion: ingest.HarvesterVersionRegistry[ingest.HarnessClaudeCode].IndexerVersion,
		IndexedAtMs:    2,
		ContentCapture: lastGoodPreviewCapture(),
	})
	if err != nil {
		t.Fatalf("retain latest attempt: %v", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("latest-attempt disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}
}

// lastGoodExec runs one statement against the store's pool.
func lastGoodExec(t *testing.T, db *store.Store, statement string, args ...any) {
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

// firstUserTurnContent returns the content of the exported user turn.
func firstUserTurnContent(t *testing.T, payload *schema.SessionDetailPayload) string {
	t.Helper()
	for i := range payload.Turns {
		if payload.Turns[i].Role == schema.RoleUser {
			return payload.Turns[i].Content
		}
	}
	t.Fatalf("exported payload has no user turn: %+v", payload.Turns)
	return ""
}

// TestExportSessionRefusesOnLatestAttemptWithoutLeakingLegacyRead proves the
// default export refuses a session that keeps a newer incomplete attempt: the
// last-good generation is NOT exported through the preserved legacy read, and
// the refusal names the way out.
func TestExportSessionRefusesOnLatestAttemptWithoutLeakingLegacyRead(t *testing.T) {
	db := lastGoodOpenStore(t)
	sid := schema.SessionID("a1a1a1a1-1111-4111-8111-111111111111")
	const authorityText = "authority full transcript"
	lastGoodSeedAuthority(t, db, sid, authorityText)
	lastGoodRetainPreview(t, db, sid, "latest attempt preview transcript")

	payload, err := export.ExportSession(context.Background(), db, testutil.NewMemFS(), string(sid))
	if err == nil {
		t.Fatalf("default export served a session with a latest attempt: %+v", payload)
	}
	if !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("default export error = %v, want the latest-attempt sentinel", err)
	}
	if strings.Contains(err.Error(), authorityText) {
		t.Fatalf("the refusal leaked transcript content: %v", err)
	}
}

// TestExportSessionSelectedLastGoodServesManagedAuthority proves the explicit
// last-good export serves the managed last-good generation while the newer
// incomplete attempt is kept beside it, and that it never serves the attempt.
func TestExportSessionSelectedLastGoodServesManagedAuthority(t *testing.T) {
	db := lastGoodOpenStore(t)
	sid := schema.SessionID("a2a2a2a2-2222-4222-8222-222222222222")
	const authorityText = "authority full transcript"
	const previewText = "latest attempt preview transcript"
	lastGoodSeedAuthority(t, db, sid, authorityText)
	lastGoodRetainPreview(t, db, sid, previewText)

	payload, err := export.ExportSessionSelected(context.Background(), db, testutil.NewMemFS(), string(sid), ingest.SessionReadSelection{LastGood: true})
	if err != nil {
		t.Fatalf("explicit last-good export: %v", err)
	}
	if got := firstUserTurnContent(t, payload); got != authorityText {
		t.Fatalf("last-good export content = %q, want the authority's %q", got, authorityText)
	}
	if strings.Contains(firstUserTurnContent(t, payload), previewText) {
		t.Fatal("the last-good export served the newer incomplete attempt")
	}
}

// TestExportSessionSelectedLastGoodServesCertificateAuthority proves the
// explicit last-good export reaches the preserved legacy/certificate read when
// the session has no managed generation but keeps a newer incomplete attempt —
// the state a certificate-only or file-backed authority is in.
func TestExportSessionSelectedLastGoodServesCertificateAuthority(t *testing.T) {
	db := lastGoodOpenStore(t)
	sid := schema.SessionID("a3a3a3a3-3333-4333-8333-333333333333")
	const authorityText = "certificate authority transcript"
	// The certificate authority is a V1 full capture with no managed generation:
	// the session's own stored entries are the full read.
	lastGoodSeedCertificateAuthority(t, db, sid, authorityText)
	lastGoodRetainPreview(t, db, sid, "latest attempt preview transcript")

	// The default export refuses in this lane too: the preserved legacy read
	// must not be reachable while the newer attempt is unresolved.
	if _, err := export.ExportSession(context.Background(), db, testutil.NewMemFS(), string(sid)); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("default certificate-only export error = %v, want the latest-attempt sentinel", err)
	}

	payload, err := export.ExportSessionSelected(context.Background(), db, testutil.NewMemFS(), string(sid), ingest.SessionReadSelection{LastGood: true})
	if err != nil {
		t.Fatalf("explicit last-good certificate export: %v", err)
	}
	if got := firstUserTurnContent(t, payload); got != authorityText {
		t.Fatalf("certificate last-good export content = %q, want the preserved authority's %q", got, authorityText)
	}
}

// TestExportSessionSelectedLastGoodRefusesActivePreview proves the explicit
// last-good export refuses when the active authority is itself only a preview:
// nothing full remains to serve, so it must not fall back to the attempt.
func TestExportSessionSelectedLastGoodRefusesActivePreview(t *testing.T) {
	db := lastGoodOpenStore(t)
	sid := schema.SessionID("a4a4a4a4-4444-4444-8444-444444444444")
	lastGoodSeedAuthority(t, db, sid, "authority full transcript")
	lastGoodRetainPreview(t, db, sid, "latest attempt preview transcript")
	// Explicit rebuild: the pointer is cleared, the preview is the active
	// authority, and the certificate records the preview, so the last-good full
	// read has nothing full to serve.
	lastGoodExec(t, db, `UPDATE session_content_captures SET status='incomplete', capture_format='preview_only', failure_code='source_records_omitted' WHERE session_id = ?`, string(sid))
	lastGoodExec(t, db, `UPDATE sessions SET active_generation_id = 'gen_latest_attempt_b', latest_attempt_generation_id = NULL, latest_attempt_artifact_hash = NULL, latest_attempt_indexer_version = NULL, latest_attempt_failure_code = NULL, latest_attempt_failure_message = NULL, latest_attempt_captured_at_ms = NULL, latest_attempt_stats_json = NULL WHERE session_id = ?`, string(sid))

	payload, err := export.ExportSessionSelected(context.Background(), db, testutil.NewMemFS(), string(sid), ingest.SessionReadSelection{LastGood: true})
	if err == nil {
		t.Fatalf("last-good export served an active preview: %+v", payload)
	}
	if !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("last-good active-preview error = %v, want the latest-attempt sentinel", err)
	}
}

// lastGoodSeedCertificateAuthority seeds a V1 full capture with one user entry
// and no managed generation: the preserved certificate read is the authority.
func lastGoodSeedCertificateAuthority(t *testing.T, db *store.Store, sid schema.SessionID, text string) {
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
	entry := schema.SessionEntry{
		SessionID: sid, EntryIndex: 0, Harness: schema.HarnessClaudeCode,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &text,
	}
	testutil.SeedReadyPublication(t, db, &meta, []schema.SessionEntry{entry})
	// A verified pair identity is the latest attempt's pair proof for this row.
	lastGoodExec(t, db, `UPDATE sessions SET artifact_hash = ? WHERE session_id = ?`, strings.Repeat("c", 64), string(sid))
}
