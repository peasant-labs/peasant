package ingest

import (
	"context"
	"fmt"
	"slices"
)

// ValidationOutcome describes whether optional validation established equality.
type ValidationOutcome string

const (
	ValidationProven      ValidationOutcome = "proven"
	ValidationNotProven   ValidationOutcome = "not-proven"
	ValidationUnsupported ValidationOutcome = "unsupported"
)

// AllValidationOutcomes lists the supported validation outcomes.
var AllValidationOutcomes = []ValidationOutcome{
	ValidationProven, ValidationNotProven, ValidationUnsupported,
}

// NewValidationOutcome validates a raw outcome.
func NewValidationOutcome(raw string) (ValidationOutcome, error) {
	value := ValidationOutcome(raw)
	if slices.Contains(AllValidationOutcomes, value) {
		return value, nil
	}
	return "", fmt.Errorf("ingest: validate managed generation outcome %q: unrecognized outcome; no result was accepted; use a supported validation outcome", raw)
}

// ValidationBudget bounds validation. Zero values mean unset; the current
// implementation derives no defaults and does not run parallel validation.
type ValidationBudget struct {
	MaxConcurrent         int
	ChunkSessions         int
	MaxBytesPerValidation int64
}

// ManagedGenerationValidationRequest describes a captured generation.
// This shape is separate from ManagedGenerationUnchangedProof because it also
// carries the generation identity and budget required by a future validator.
type ManagedGenerationValidationRequest struct {
	SessionID     SessionID
	GenerationID  string
	Metadata      UnifiedMetadata
	InputDigest   string
	IndexerTarget int
	FormatTarget  int
	Expected      *SessionIndexState
	Budget        ValidationBudget
}

// ManagedGenerationValidationResult is a validated outcome and its detail.
type ManagedGenerationValidationResult struct {
	Outcome  ValidationOutcome
	Mismatch UnchangedProofMismatch // set only for not-proven
	Detail   string
}

// NewManagedGenerationValidationResult rejects inconsistent outcome/mismatch pairs.
func NewManagedGenerationValidationResult(outcome ValidationOutcome, mismatch UnchangedProofMismatch, detail string) (ManagedGenerationValidationResult, error) {
	if _, err := NewValidationOutcome(string(outcome)); err != nil {
		return ManagedGenerationValidationResult{}, err
	}
	if _, err := NewUnchangedProofMismatch(string(mismatch)); err != nil {
		return ManagedGenerationValidationResult{}, err
	}
	if (outcome == ValidationNotProven) != (mismatch != UnchangedMismatchNone) {
		return ManagedGenerationValidationResult{}, fmt.Errorf("ingest: validate generation result: outcome %q and mismatch %q disagree; result was not accepted; provide a mismatch only for not-proven", outcome, mismatch)
	}
	return ManagedGenerationValidationResult{Outcome: outcome, Mismatch: mismatch, Detail: detail}, nil
}

// ManagedGenerationValidator is the optional generation-validation seam.
type ManagedGenerationValidator interface {
	ValidateManagedGenerationUnchanged(context.Context, ManagedGenerationValidationRequest) (ManagedGenerationValidationResult, error)
}
