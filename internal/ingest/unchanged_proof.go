package ingest

import (
	"context"
	"fmt"
	"slices"
)

// ManagedGenerationUnchangedProof names the captured input and index target.
type ManagedGenerationUnchangedProof struct {
	SessionID     SessionID
	Metadata      UnifiedMetadata
	InputDigest   string
	IndexerTarget int
	FormatTarget  int
	Expected      *SessionIndexState
}

// ManagedGenerationUnchangedProver is the store boundary for a read-only
// proof and its separately revalidated settlement.
type ManagedGenerationUnchangedProver interface {
	ProveCapturedGenerationUnchanged(context.Context, ManagedGenerationUnchangedProof) (UnchangedProof, error)
	SettleCapturedGenerationUnchanged(context.Context, ManagedGenerationUnchangedProof) (ManagedGenerationRefreshOutcome, error)
}

// UnchangedProofMismatch identifies why an equality proof could not be made.
// It is not a claim that the generation changed.
type UnchangedProofMismatch string

const (
	UnchangedMismatchNone            UnchangedProofMismatch = ""
	UnchangedNotProvenActive         UnchangedProofMismatch = "generation-not-active"
	UnchangedNotProvenUnsupported    UnchangedProofMismatch = "generation-unsupported"
	UnchangedNotProvenLatestAttempt  UnchangedProofMismatch = "latest-attempt-held"
	UnchangedNotProvenContent        UnchangedProofMismatch = "content-hash"
	UnchangedNotProvenSourceFormat   UnchangedProofMismatch = "source-format"
	UnchangedNotProvenHarness        UnchangedProofMismatch = "harness"
	UnchangedNotProvenCompleteness   UnchangedProofMismatch = "completeness"
	UnchangedNotProvenSourceEvidence UnchangedProofMismatch = "source-evidence"
	UnchangedNotProvenMetadata       UnchangedProofMismatch = "metadata-context"
	UnchangedNotProvenRedaction      UnchangedProofMismatch = "redaction-context"
	UnchangedNotProvenIndexer        UnchangedProofMismatch = "index-not-current"
	UnchangedNotProvenInputProof     UnchangedProofMismatch = "input-proof-missing"
	UnchangedNotProvenSeal           UnchangedProofMismatch = "seal-missing"
)

// AllUnchangedProofMismatches returns a fresh list including the zero value.
func AllUnchangedProofMismatches() []UnchangedProofMismatch {
	return []UnchangedProofMismatch{
		UnchangedMismatchNone, UnchangedNotProvenActive, UnchangedNotProvenUnsupported,
		UnchangedNotProvenLatestAttempt, UnchangedNotProvenContent, UnchangedNotProvenSourceFormat,
		UnchangedNotProvenHarness, UnchangedNotProvenCompleteness, UnchangedNotProvenSourceEvidence,
		UnchangedNotProvenMetadata, UnchangedNotProvenRedaction, UnchangedNotProvenIndexer,
		UnchangedNotProvenInputProof, UnchangedNotProvenSeal,
	}
}

// NewUnchangedProofMismatch validates a mismatch at a boundary.
func NewUnchangedProofMismatch(raw string) (UnchangedProofMismatch, error) {
	value := UnchangedProofMismatch(raw)
	if slices.Contains(AllUnchangedProofMismatches(), value) {
		return value, nil
	}
	return UnchangedMismatchNone, fmt.Errorf("ingest: read unchanged-proof mismatch %q: this build does not recognize it; no proof result was accepted; use a supported mismatch reason", raw)
}

// UnchangedProof is one-sided: failure to prove equality requires a fallback.
type UnchangedProof struct {
	Proven   bool
	Mismatch UnchangedProofMismatch
	Detail   string
}
