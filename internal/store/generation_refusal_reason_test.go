package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// previewOverFullReason is the actionable refusal the guarded transaction
// reports for an accidental preview replacing full read authority.
const previewOverFullReason = "preview replacement refused over full read authority"

// assessedSourceOmittedPreview builds the REAL assessed capture the way
// production does: the facts are set FIRST (SourceOmitted and Unaccounted, the
// derivation prepareGenerationResult performs), then AssessCapture and
// ContentCapture derive the tuple. The recoveryCapture helper leaves those facts
// unset and therefore certifies FULL coverage; it is the construction idiom for
// the last-good authority, never a producer of the preview.
func assessedSourceOmittedPreview(t *testing.T, v2 indexformat.V2) ingest.SessionContentCaptureWrite {
	t.Helper()
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: ingest.Harness("claude-code"), Result: v2,
		Policy: ingest.CaptureFreshCandidate, Authoritative: true,
		SourceOmitted: true, Unaccounted: true,
	})
	if err != nil {
		t.Fatalf("assessment: %v", err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	if capture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly {
		t.Fatalf("assessed capture format = %s, want preview_only", capture.CaptureFormat)
	}
	return capture
}

// assessedPreviewActivation commits a full prior generation for a fresh session
// and returns a complete candidate carrying the REAL assessed
// unaccounted-SourceOmitted preview plus the capture it was built from. When
// identity is non-nil the stored pair identity is seeded, so the latest-attempt
// boundary has a verified pair to record.
func assessedPreviewActivation(t *testing.T, s *Store, sid schema.SessionID, identity *string) (GenerationActivation, ingest.SessionContentCaptureWrite) {
	t.Helper()
	seedFullAuthority(t, s, sid)
	if identity != nil {
		setStoredArtifactHash(t, s, sid, *identity)
	}
	preview, blobs := buildTestGeneration(t, sid, "gen_preview", "preview text", "preview input", "preview output")
	capture := assessedSourceOmittedPreview(t, preview)
	return GenerationActivation{
		Generation:     preview,
		Blobs:          blobs,
		IndexerVersion: 18,
		IndexedAtMs:    2,
		ContentCapture: capture,
	}, capture
}

// requireAssessedPreviewOutcome asserts the store follows the assessed capture
// tuple and reports whether the attempt was kept as the latest attempt. The assessed
// unaccounted-SourceOmitted preview is kept as the latest attempt; a preview carrying any other
// code keeps the existing guard refusal, so the latest attempt is never a bypass for an
// accidental preview.
func requireAssessedPreviewOutcome(t *testing.T, s *Store, sid schema.SessionID, capture ingest.SessionContentCaptureWrite, outcome ingest.ActivationOutcome, err error) bool {
	t.Helper()
	if capture.CaptureFormat != ingest.ContentCaptureFormatPreviewOnly {
		t.Fatalf("assessed capture format = %s, want preview_only", capture.CaptureFormat)
	}
	if capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		requirePreviewOverFullRefusal(t, s, sid, outcome, err)
		return false
	}
	if err != nil {
		t.Fatalf("latest-attempt write returned error %v, want a preview", err)
	}
	if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
		t.Fatalf("disposition = %v, want ActivationLatestAttemptPreview", outcome.Disposition)
	}
	if active, readErr := s.activeGenerationID(context.Background(), sid); readErr != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the untouched full prior", active, readErr)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.generationID != "gen_preview" {
		t.Fatalf("latest-attempt generation = %q, want gen_preview", pointer.generationID)
	}
	return true
}

// requirePreviewOverFullRefusal asserts the write was refused with the
// actionable guard reason and the full prior stayed authoritative.
func requirePreviewOverFullRefusal(t *testing.T, s *Store, sid schema.SessionID, outcome ingest.ActivationOutcome, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), previewOverFullReason) {
		t.Fatalf("refusal = %v, want the actionable reason %q", err, previewOverFullReason)
	}
	if outcome.Disposition != ingest.ActivationNotCommitted {
		t.Fatalf("disposition = %v, want NotCommitted", outcome.Disposition)
	}
	if active, readErr := s.activeGenerationID(context.Background(), sid); readErr != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the full prior", active, readErr)
	}
}

// TestPreviewOverFullAssessedInline proves the store follows the assessed
// capture on the inline route: the assessed unaccounted-SourceOmitted preview is
// kept as the latest attempt over a publishable full authority, and the latest-attempt pointer records
// the verified pair.
func TestPreviewOverFullAssessedInline(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a")
	identity := hexIdentity('7')
	activation, capture := assessedPreviewActivation(t, s, sid, &identity)
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	if !requireAssessedPreviewOutcome(t, s, sid, capture, outcome, err) {
		return
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.artifactHash != identity {
		t.Fatalf("latest-attempt pair identity = %q, want the verified identity", pointer.artifactHash)
	}
}

// TestPreviewOverFullAssessedPrepared proves the decision holds for a candidate
// whose objects were staged first.
func TestPreviewOverFullAssessedPrepared(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("2b2b2b2b-2b2b-4b2b-8b2b-2b2b2b2b2b2b")
	identity := hexIdentity('8')
	activation, capture := assessedPreviewActivation(t, s, sid, &identity)
	prepared, err := s.StageGeneration(context.Background(), activation)
	if err != nil {
		t.Fatalf("StageGeneration: %v", err)
	}
	activation.Prepared = prepared
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	requireAssessedPreviewOutcome(t, s, sid, capture, outcome, err)
}

// TestPreviewOverFullAssessedReplayed proves a repeated attempt with the same
// assessed candidate is an idempotent AlreadyCommitted that leaves the authority
// and the latest-attempt pointer unchanged.
func TestPreviewOverFullAssessedReplayed(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("3c3c3c3c-3c3c-4c3c-8c3c-3c3c3c3c3c3c")
	identity := hexIdentity('9')
	activation, capture := assessedPreviewActivation(t, s, sid, &identity)
	first, err := s.ActivateGeneration(context.Background(), activation)
	if !requireAssessedPreviewOutcome(t, s, sid, capture, first, err) {
		return
	}
	pointerBefore := latestAttemptPointerFor(t, s, sid)
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if outcome.Disposition != ingest.ActivationAlreadyCommitted {
		t.Fatalf("replay disposition = %v, want ActivationAlreadyCommitted", outcome.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer != pointerBefore {
		t.Fatalf("latest-attempt pointer changed on replay: %+v -> %+v", pointerBefore, pointer)
	}
}

// TestPreviewOverFullPairProofRefusal proves the store boundary refuses an
// assessed preview when neither the stored artifact hash nor the write supplies
// a verified pair identity, leaving the authority intact.
func TestPreviewOverFullPairProofRefusal(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("4d4d4d4d-4d4d-4d4d-8d4d-4d4d4d4d4d4d")
	activation, capture := assessedPreviewActivation(t, s, sid, nil)
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	if capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		// The assessed tuple is not the latest-attempt tuple, so the guard refusal
		// stands before the pair-proof clause is reached.
		requirePreviewOverFullRefusal(t, s, sid, outcome, err)
		return
	}
	if err == nil || !strings.Contains(err.Error(), "pair-proof-unavailable") {
		t.Fatalf("refusal = %v, want the actionable pair-proof-unavailable error", err)
	}
	if outcome.Disposition != ingest.ActivationNotCommitted {
		t.Fatalf("disposition = %v, want NotCommitted", outcome.Disposition)
	}
	if active, readErr := s.activeGenerationID(context.Background(), sid); readErr != nil || active != "gen_full_prior" {
		t.Fatalf("active = %q (err=%v), want the full prior", active, readErr)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want none", pointer)
	}
}

// TestPreviewOverFullGuardRefusalNotAssessed proves a preview-over-full write
// whose capture is NOT the assessed unaccounted-SourceOmitted tuple keeps the
// existing guard refusal: the latest attempt is never a bypass for an accidental preview.
func TestPreviewOverFullGuardRefusalNotAssessed(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("5e5e5e5e-5e5e-4e5e-8e5e-5e5e5e5e5e5e")
	identity := hexIdentity('a')
	seedFullAuthority(t, s, sid)
	setStoredArtifactHash(t, s, sid, identity)

	// testActivation carries a preview-only capture with no failure code: not
	// the assessed tuple, so the guard refusal stands.
	activation := testActivation(t, sid, "gen_preview")
	activation.IndexerVersion = 18
	activation.IndexedAtMs = 2
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	requirePreviewOverFullRefusal(t, s, sid, outcome, err)
	if pointer := latestAttemptPointerFor(t, s, sid); pointer.found() {
		t.Fatalf("latest-attempt pointer = %+v, want none after a guard refusal", pointer)
	}
}

// TestPreviewOverFullReplayLeavesAuthorityAndPointer proves the named replay
// expectation: re-submitting the already-installed active authority's candidate
// identifier reports AlreadyCommitted and leaves the authority and the latest-attempt
// pointer (pointing at the newer attempt) unchanged.
func TestPreviewOverFullReplayLeavesAuthorityAndPointer(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("6f6f6f6f-6f6f-4f6f-8f6f-6f6f6f6f6f6f")
	identity := hexIdentity('b')
	activation, capture := assessedPreviewActivation(t, s, sid, &identity)
	first, err := s.ActivateGeneration(context.Background(), activation)
	if !requireAssessedPreviewOutcome(t, s, sid, capture, first, err) {
		return
	}
	pointerBefore := latestAttemptPointerFor(t, s, sid)

	prior, priorBlobs := buildTestGeneration(t, sid, "gen_full_prior", "full text", "full input", "full output")
	duplicate, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: prior, Blobs: priorBlobs, IndexerVersion: 1, IndexedAtMs: 1,
		ContentCapture: recoveryCapture(t, prior),
	})
	if err != nil {
		t.Fatalf("old-authority duplicate: %v", err)
	}
	if duplicate.Disposition != ingest.ActivationAlreadyCommitted {
		t.Fatalf("duplicate disposition = %v, want ActivationAlreadyCommitted", duplicate.Disposition)
	}
	if pointer := latestAttemptPointerFor(t, s, sid); pointer != pointerBefore {
		t.Fatalf("latest-attempt pointer changed on an old-authority duplicate: %+v -> %+v", pointerBefore, pointer)
	}
}
