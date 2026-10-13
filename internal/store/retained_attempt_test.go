package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// sourceOmittedPreviewWrite is the store write's capture tuple for the
// unaccounted source-records omission preview: preview_only with the
// source_records_omitted code. It is the exact tuple the pipeline's
// AssessCapture + ContentCapture derivation produces for the P15 row-4 facts
// (SourceOmitted && Unaccounted && !hasOmissions); the eligibility predicate
// reads these literal fields, so a store test constructs them directly.
func sourceOmittedPreviewWrite() ingest.SessionContentCaptureWrite {
	return ingest.SessionContentCaptureWrite{
		Status:           ingest.ContentCaptureIncomplete,
		SourceAuthority:  ingest.ContentSourceNewIngest,
		TranscriptOrigin: ingest.TranscriptOriginFile,
		CaptureFormat:    ingest.ContentCaptureFormatPreviewOnly,
		FailureCode:      ingest.ContentCaptureSourceRecordsOmitted,
		FailureMessage:   "oversized source records were omitted with positional placeholders",
		CapturedAtMs:     1,
	}
}

// retainedPointerFor reads the stored retained pointer for one session.
func retainedPointerFor(t *testing.T, s *Store, sid schema.SessionID) retainedAttemptPointer {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	pointer, err := readRetainedAttemptPointerOnConn(conn, sid)
	if err != nil {
		t.Fatalf("read retained pointer: %v", err)
	}
	return pointer
}

// storedArtifactHash reads the session's artifact hash, distinguishing NULL.
func storedArtifactHash(t *testing.T, s *Store, sid schema.SessionID) (string, bool) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	value, found, err := readStoredArtifactHashOnConn(conn, ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("read artifact hash: %v", err)
	}
	return value, found
}

// setStoredArtifactHash seeds the session's stored pair identity directly, the
// state a legacy row carries once the pipeline has established it.
func setStoredArtifactHash(t *testing.T, s *Store, sid schema.SessionID, hash string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.Execute(conn, `UPDATE sessions SET artifact_hash = ? WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{hash, string(sid)},
	}); err != nil {
		t.Fatalf("set stored artifact hash: %v", err)
	}
}

// readIndexStateFor captures the session's current SQL index state, the
// snapshot a caller supplies as ExpectedState.
func readIndexStateFor(t *testing.T, s *Store, sid schema.SessionID) *ingest.SessionIndexState {
	t.Helper()
	state, err := s.ReadIndexState(context.Background(), ingest.SessionID(sid))
	if err != nil {
		t.Fatalf("ReadIndexState: %v", err)
	}
	if state == nil {
		t.Fatalf("ReadIndexState returned nil for a seeded session")
	}
	return state
}

// generationRowCount counts the session's harmonized generation rows.
func generationRowCount(t *testing.T, s *Store, sid schema.SessionID) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	var count int64
	if err := sqlitex.Execute(conn, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count generation rows: %v", err)
	}
	return count
}

// seedFullAuthority seeds the session and commits a complete full generation as
// the read authority.
func seedFullAuthority(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	seedGenerationSession(t, s, string(sid))
	prior, priorBlobs := buildTestGeneration(t, sid, "gen_full_prior", "full text", "full input", "full output")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: prior, Blobs: priorBlobs, IndexerVersion: 1, IndexedAtMs: 1,
		ContentCapture: recoveryCapture(t, prior),
	}); err != nil {
		t.Fatalf("full prior activation: %v", err)
	}
}

// previewCandidate builds a complete generation carrying the assessed
// unaccounted-SourceOmitted preview capture.
func previewCandidate(t *testing.T, sid schema.SessionID, genID string) GenerationActivation {
	t.Helper()
	preview, blobs := buildTestGeneration(t, sid, genID, "preview text", "preview input", "preview output")
	return GenerationActivation{
		Generation:     preview,
		Blobs:          blobs,
		IndexerVersion: 18,
		IndexedAtMs:    2,
		ContentCapture: sourceOmittedPreviewWrite(),
	}
}

func hexIdentity(fill byte) string {
	return strings.Repeat(string([]byte{fill}), 64)
}

// TestRetentionRetainsLatestPreviewOverFullAuthority proves the core
// coexistence behavior on the single activation route: the preview is retained,
// the full authority stays active and byte-identical, and the pointer records
// the attempt.
func TestRetentionRetainsLatestPreviewOverFullAuthority(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("4d4d4d4d-4d4d-4d4d-8d4d-4d4d4d4d4d4d")
	identity := hexIdentity('a')
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, identity)

	outcome, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b"))
	if err != nil {
		t.Fatalf("retention activation: %v", err)
	}
	if outcome.Disposition != ingest.ActivationRetainedPreview {
		t.Fatalf("disposition = %v, want ActivationRetainedPreview", outcome.Disposition)
	}
	if active, readErr := s.activeGenerationID(context.Background(), sid); readErr != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the full authority", active, readErr)
	}
	pointer := retainedPointerFor(t, s, sid)
	if pointer.generationID != "gen_preview_b" {
		t.Fatalf("retained generation = %q, want gen_preview_b", pointer.generationID)
	}
	if pointer.artifactHash != identity {
		t.Fatalf("retained pair identity = %q, want the verified identity", pointer.artifactHash)
	}
	if pointer.indexerVersion != 18 {
		t.Fatalf("retained producer revision = %d, want 18", pointer.indexerVersion)
	}
	if pointer.failureCode != string(ingest.ContentCaptureSourceRecordsOmitted) {
		t.Fatalf("retained failure code = %q, want source_records_omitted", pointer.failureCode)
	}
	if stored, found := storedArtifactHash(t, s, sid); !found || stored != identity {
		t.Fatalf("stored artifact hash = %q (found=%v), want the untouched identity", stored, found)
	}
	// The active authority's capture is still the full certificate.
	capture, found, err := s.GetSessionContentCapture(context.Background(), ingest.SessionID(sid))
	if err != nil || !found {
		t.Fatalf("read capture: %v (found=%v)", err, found)
	}
	if capture.CaptureFormat != ingest.ContentCaptureFormatFull || capture.Status != ingest.ContentCaptureComplete {
		t.Fatalf("active capture = %s/%s, want the untouched full certificate", capture.CaptureFormat, capture.Status)
	}
}

// TestRetentionFreshIDUnchangedIsTrueNoOp proves the TRUE no-op: a fresh
// identifier carrying the same binding and facets writes nothing and leaves the
// pointer on the original attempt.
func TestRetentionFreshIDUnchangedIsTrueNoOp(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("5e5e5e5e-5e5e-4e5e-8e5e-5e5e5e5e5e5e")
	identity := hexIdentity('b')
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, identity)

	if _, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b")); err != nil {
		t.Fatalf("first retention: %v", err)
	}
	before := generationRowCount(t, s, sid)
	// A fresh identifier with the same projection and facets: the binding
	// excludes the generation identifier, so this is the true no-op.
	repeat, repeatBlobs := buildTestGeneration(t, sid, "gen_preview_b2", "preview text", "preview input", "preview output")
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: repeat, Blobs: repeatBlobs, IndexerVersion: 18, IndexedAtMs: 3,
		ContentCapture: sourceOmittedPreviewWrite(),
	})
	if err != nil {
		t.Fatalf("repeat retention: %v", err)
	}
	if outcome.Disposition != ingest.ActivationRetainedPreview {
		t.Fatalf("disposition = %v, want ActivationRetainedPreview", outcome.Disposition)
	}
	if after := generationRowCount(t, s, sid); after != before {
		t.Fatalf("generation rows = %d, want %d (the true no-op writes nothing)", after, before)
	}
	if pointer := retainedPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" {
		t.Fatalf("retained generation = %q, want the unchanged gen_preview_b", pointer.generationID)
	}
}

// TestRetentionChangedFacetReplacesAttempt proves the REPLACE branch: a changed
// producer facet replaces the retained attempt with a fresh immutable row and
// advances the pointer.
func TestRetentionChangedFacetReplacesAttempt(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("6f6f6f6f-6f6f-4f6f-8f6f-6f6f6f6f6f6f")
	identity := hexIdentity('c')
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, identity)

	if _, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b")); err != nil {
		t.Fatalf("first retention: %v", err)
	}
	replacement, blobs := buildTestGeneration(t, sid, "gen_preview_b2", "preview text", "preview input", "preview output")
	outcome, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: replacement, Blobs: blobs, IndexerVersion: 19, IndexedAtMs: 4,
		ContentCapture: sourceOmittedPreviewWrite(),
	})
	if err != nil {
		t.Fatalf("replacement retention: %v", err)
	}
	if outcome.Disposition != ingest.ActivationRetainedPreview {
		t.Fatalf("disposition = %v, want ActivationRetainedPreview", outcome.Disposition)
	}
	pointer := retainedPointerFor(t, s, sid)
	if pointer.generationID != "gen_preview_b2" {
		t.Fatalf("retained generation = %q, want the replaced gen_preview_b2", pointer.generationID)
	}
	if pointer.indexerVersion != 19 {
		t.Fatalf("retained producer revision = %d, want 19", pointer.indexerVersion)
	}
	if rows := generationRowCount(t, s, sid); rows != 2 {
		t.Fatalf("generation rows = %d, want 2 (the full authority and one retained attempt)", rows)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the full authority", active, err)
	}
}

// TestRetentionPairProofRefusalLeavesAuthority proves clause 6: a legacy row
// with no stored identity and no supplied identity is refused at the store
// boundary with the actionable pair-proof-unavailable error, and the authority
// is intact.
func TestRetentionPairProofRefusalLeavesAuthority(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("7a7a7a7a-7a7a-4a7a-8a7a-7a7a7a7a7a7a")
	seedFullAuthority(t, s, sid)

	outcome, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b"))
	if err == nil || !strings.Contains(err.Error(), "pair-proof-unavailable") {
		t.Fatalf("refusal = %v, want the actionable pair-proof-unavailable error", err)
	}
	if outcome.Disposition != ingest.ActivationNotCommitted {
		t.Fatalf("disposition = %v, want NotCommitted", outcome.Disposition)
	}
	if active, readErr := s.activeGenerationID(context.Background(), sid); readErr != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the full authority", active, readErr)
	}
	if pointer := retainedPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("retained pointer = %+v, want none", pointer)
	}
}

// TestRetentionLegacyBootstrapRecordsIdentity proves the legacy NULL bootstrap:
// a stored hash of NULL with a supplied verified identity establishes the
// session identity and records it non-null on the pointer.
func TestRetentionLegacyBootstrapRecordsIdentity(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("8b8b8b8b-8b8b-4b8b-8b8b-8b8b8b8b8b8b")
	seedFullAuthority(t, s, sid)
	if _, found := storedArtifactHash(t, s, sid); found {
		t.Fatal("stored artifact hash must start NULL for the bootstrap case")
	}
	identity := hexIdentity('d')
	candidate := previewCandidate(t, sid, "gen_preview_b")
	candidate.ExpectedState = readIndexStateFor(t, s, sid)
	candidate.ArtifactIdentity = &identity
	outcome, err := s.ActivateGeneration(context.Background(), candidate)
	if err != nil {
		t.Fatalf("bootstrap retention: %v", err)
	}
	if outcome.Disposition != ingest.ActivationRetainedPreview {
		t.Fatalf("disposition = %v, want ActivationRetainedPreview", outcome.Disposition)
	}
	if stored, found := storedArtifactHash(t, s, sid); !found || stored != identity {
		t.Fatalf("stored artifact hash = %q (found=%v), want the bootstrapped identity", stored, found)
	}
	if pointer := retainedPointerFor(t, s, sid); pointer.artifactHash != identity {
		t.Fatalf("retained pair identity = %q, want the bootstrapped identity", pointer.artifactHash)
	}
}

// TestRetentionReplayLeavesAuthorityAndPointer proves the named replay
// expectation: re-submitting the already-installed active authority's candidate
// identifier reports AlreadyCommitted and leaves both the authority and the
// retained pointer untouched; a same-identifier retry of the retained candidate
// reports AlreadyCommitted too.
func TestRetentionReplayLeavesAuthorityAndPointer(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("9c9c9c9c-9c9c-4c9c-8c9c-9c9c9c9c9c9c")
	identity := hexIdentity('e')
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, identity)

	if _, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b")); err != nil {
		t.Fatalf("retention: %v", err)
	}
	pointerBefore := retainedPointerFor(t, s, sid)
	rowsBefore := generationRowCount(t, s, sid)

	prior, priorBlobs := buildTestGeneration(t, sid, "gen_full_prior", "full text", "full input", "full output")
	duplicate, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: prior, Blobs: priorBlobs, IndexerVersion: 1, IndexedAtMs: 1,
		ContentCapture: recoveryCapture(t, prior),
	})
	if err != nil {
		t.Fatalf("old-authority duplicate: %v", err)
	}
	if duplicate.Disposition != ingest.ActivationAlreadyCommitted {
		t.Fatalf("old-authority duplicate disposition = %v, want AlreadyCommitted", duplicate.Disposition)
	}
	if pointer := retainedPointerFor(t, s, sid); pointer != pointerBefore {
		t.Fatalf("retained pointer changed on an old-authority duplicate: %+v -> %+v", pointerBefore, pointer)
	}
	if rows := generationRowCount(t, s, sid); rows != rowsBefore {
		t.Fatalf("generation rows changed on an old-authority duplicate: %d -> %d", rowsBefore, rows)
	}

	retried, err := s.ActivateGeneration(context.Background(), previewCandidate(t, sid, "gen_preview_b"))
	if err != nil {
		t.Fatalf("retained-candidate retry: %v", err)
	}
	if retried.Disposition != ingest.ActivationAlreadyCommitted {
		t.Fatalf("retained-candidate retry disposition = %v, want AlreadyCommitted", retried.Disposition)
	}
	if pointer := retainedPointerFor(t, s, sid); pointer != pointerBefore {
		t.Fatalf("retained pointer changed on a same-identifier retry: %+v -> %+v", pointerBefore, pointer)
	}
}

// TestRetentionBatchRouteMapsDisposition proves the batch route maps a retained
// commit to ActivationRetainedPreview and keeps the authority active.
func TestRetentionBatchRouteMapsDisposition(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("adadadad-adad-4dad-8dad-adadadadadad")
	identity := hexIdentity('f')
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, identity)

	results := s.ActivateGenerationBatch(context.Background(), []GenerationActivation{previewCandidate(t, sid, "gen_preview_b")})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("batch retention error: %v", results[0].Err)
	}
	if results[0].Outcome.Disposition != ingest.ActivationRetainedPreview {
		t.Fatalf("batch disposition = %v, want ActivationRetainedPreview", results[0].Outcome.Disposition)
	}
	if active, err := s.activeGenerationID(context.Background(), sid); err != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the full authority", active, err)
	}
	if pointer := retainedPointerFor(t, s, sid); pointer.generationID != "gen_preview_b" {
		t.Fatalf("retained generation = %q, want gen_preview_b", pointer.generationID)
	}
}
