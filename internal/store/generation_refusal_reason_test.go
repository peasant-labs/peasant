package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// previewOverFullReason is the actionable refusal the guarded transaction
// reports for an accidental preview replacing full read authority.
const previewOverFullReason = "preview replacement refused over full read authority"

// previewOverFullActivation commits a full prior generation for a fresh
// session and returns a preview-capture candidate that would replace it.
func previewOverFullActivation(t *testing.T, s *Store, sid schema.SessionID) GenerationActivation {
	t.Helper()
	seedGenerationSession(t, s, string(sid))
	prior, priorBlobs := buildTestGeneration(t, sid, "gen_full_prior", "full text", "full input", "full output")
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation: prior, Blobs: priorBlobs, IndexerVersion: 1, IndexedAtMs: 1,
		ContentCapture: recoveryCapture(t, prior),
	}); err != nil {
		t.Fatalf("full prior activation: %v", err)
	}
	// A structurally complete candidate whose capture is only a bounded
	// preview: the transaction's last-good guard refuses it.
	preview := testActivation(t, sid, "gen_preview")
	preview.IndexerVersion = 1
	preview.IndexedAtMs = 2
	return preview
}

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

func TestPreviewOverFullRefusalReasonInline(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("1a1a1a1a-1a1a-4a1a-8a1a-1a1a1a1a1a1a")
	activation := previewOverFullActivation(t, s, sid)
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	requirePreviewOverFullRefusal(t, s, sid, outcome, err)
}

func TestPreviewOverFullRefusalReasonPrepared(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("2b2b2b2b-2b2b-4b2b-8b2b-2b2b2b2b2b2b")
	activation := previewOverFullActivation(t, s, sid)
	prepared, err := s.StageGeneration(context.Background(), activation)
	if err != nil {
		t.Fatalf("StageGeneration: %v", err)
	}
	activation.Prepared = prepared
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	requirePreviewOverFullRefusal(t, s, sid, outcome, err)
}

// TestPreviewOverFullRefusalReasonReplayed proves a repeated attempt with
// the same preview candidate is refused again with the actionable reason:
// no recovery path exists that could bypass the guard, and the last-good
// generation stays authoritative across attempts.
func TestPreviewOverFullRefusalReasonReplayed(t *testing.T) {
	s, _ := openGenerationStore(t)
	sid := schema.SessionID("3c3c3c3c-3c3c-4c3c-8c3c-3c3c3c3c3c3c")
	activation := previewOverFullActivation(t, s, sid)
	first, err := s.ActivateGeneration(context.Background(), activation)
	requirePreviewOverFullRefusal(t, s, sid, first, err)
	outcome, err := s.ActivateGeneration(context.Background(), activation)
	requirePreviewOverFullRefusal(t, s, sid, outcome, err)
}
