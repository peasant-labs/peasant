package ingest

import (
	"context"
	"fmt"

	"github.com/peasant-labs/schema"
)

// ReconcileDecision is what a retained session needs from the reconciliation
// walk, decided from small evidence only: its committed metadata file and the
// identity the database already holds for it.
//
// The zero value is deliberately not a decision. A caller must obtain one from
// NewReconcileDecision or DecideReconcile, so a forgotten assignment cannot
// read as "unchanged" and silently retire the work a session needs.
type ReconcileDecision uint8

const (
	// ReconcileUnchanged means the database already mirrors exactly the pair
	// on disk. The walk must not lock the session, read its transcript, or
	// stage an intent to establish that.
	ReconcileUnchanged ReconcileDecision = iota + 1
	// ReconcileBootstrap means the database holds no current capture for a
	// session whose retained files exist: a retained-logs-only session that is
	// mirrored from its own files.
	ReconcileBootstrap
	// ReconcileRepair means the database holds a capture that disagrees with
	// the retained files, so the committed pair is read and re-mirrored.
	ReconcileRepair
)

func (d ReconcileDecision) String() string {
	switch d {
	case ReconcileUnchanged:
		return "unchanged"
	case ReconcileBootstrap:
		return "bootstrap"
	case ReconcileRepair:
		return "repair"
	}
	return "invalid"
}

// Valid reports whether the value is one of the three decisions.
func (d ReconcileDecision) Valid() bool {
	return d == ReconcileUnchanged || d == ReconcileBootstrap || d == ReconcileRepair
}

// NewReconcileDecision converts a recorded name back into a decision. It
// refuses anything outside the closed set instead of defaulting to one.
func NewReconcileDecision(name string) (ReconcileDecision, error) {
	for _, decision := range []ReconcileDecision{ReconcileUnchanged, ReconcileBootstrap, ReconcileRepair} {
		if decision.String() == name {
			return decision, nil
		}
	}
	return 0, fmt.Errorf("decide retained reconciliation work: %q is not a reconciliation decision; no retained file or database row was changed; use %q, %q or %q",
		name, ReconcileUnchanged, ReconcileBootstrap, ReconcileRepair)
}

// PublicationCaptureDigest is the identity of the capture the database
// currently holds for one session. It is the small half of the same evidence
// the retained metadata file carries, so the two can be compared without
// opening a transcript.
//
// It is capture evidence only: it says which pair the database mirrors, never
// that an index run succeeded.
type PublicationCaptureDigest struct {
	SessionID    SessionID
	Harness      Harness
	MetadataHash string
	ContentHash  string
}

// PublicationCaptureDigestReader reports the mirrored capture identity for one
// session, or nil when the database holds no current capture for it.
type PublicationCaptureDigestReader interface {
	ReadPublicationCaptureDigest(context.Context, SessionID) (*PublicationCaptureDigest, error)
}

// DecideReconcile decides a retained session's reconciliation work from its
// committed metadata bytes and the identity the database already holds.
//
// A session is unchanged only when every small fact agrees: the same session
// and harness, the same metadata and transcript digests as the mirrored
// capture, and the same artifact identity as the index state. The artifact
// identity is recomputed here from the metadata alone, using the transcript
// digest the metadata itself declares, so a metadata change that moves the
// artifact identity is still seen without reading the transcript.
//
// A transcript changed out of band under unchanged metadata is deliberately
// not decided here. That is a checksum failure, and it is reported where it is
// reported today: by the readers that verify the committed pair before using
// it, which hold the session and warn instead of rewriting it.
func DecideReconcile(meta *UnifiedMetadata, metadataJSON []byte, digest *PublicationCaptureDigest, state *SessionIndexState) (ReconcileDecision, error) {
	if meta == nil {
		return 0, fmt.Errorf("decide retained reconciliation work: no committed metadata was supplied; no retained file or database row was changed; capture the session's committed metadata before deciding")
	}
	if digest == nil {
		return ReconcileBootstrap, nil
	}
	if state == nil || digest.SessionID != meta.SessionID || digest.Harness != meta.ModelHarness ||
		meta.MetadataHash == "" || meta.ContentHash == "" ||
		digest.MetadataHash != meta.MetadataHash || digest.ContentHash != meta.ContentHash ||
		state.SessionID != meta.SessionID || state.Harness != meta.ModelHarness || state.ArtifactHash == nil {
		return ReconcileRepair, nil
	}
	// The declared digest is only usable as evidence while it still describes
	// the metadata it sits in; an edited file with a stale digest is a repair.
	if meta.MetadataHash != schema.ComputeMetadataHash(meta) {
		return ReconcileRepair, nil
	}
	semantic, err := artifactSemanticJSON(metadataJSON, meta.ContentHash)
	if err != nil {
		return 0, fmt.Errorf("decide retained reconciliation work for session %s: %w; no retained file or database row was changed; restore valid committed metadata and retry harvest", meta.SessionID, err)
	}
	if schema.ComputeTranscriptHash(semantic) != *state.ArtifactHash {
		return ReconcileRepair, nil
	}
	return ReconcileUnchanged, nil
}
