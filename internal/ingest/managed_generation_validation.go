package ingest

import (
	"context"
	"fmt"
	"slices"
)

type ValidationOutcome string

const (
	ValidationProven      ValidationOutcome = "proven"
	ValidationNotProven   ValidationOutcome = "not-proven"
	ValidationUnsupported ValidationOutcome = "unsupported"
)

var AllValidationOutcomes = []ValidationOutcome{
	ValidationProven, ValidationNotProven, ValidationUnsupported,
}

func NewValidationOutcome(raw string) (ValidationOutcome, error) {
	value := ValidationOutcome(raw)
	if slices.Contains(AllValidationOutcomes, value) {
		return value, nil
	}
	return "", fmt.Errorf("ingest: validate managed generation outcome %q: unrecognized outcome; no result was accepted; use a supported validation outcome", raw)
}

type ValidationBudget struct {
	MaxConcurrent         int
	ChunkSessions         int
	MaxBytesPerValidation int64
}

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

type ManagedGenerationValidationResult struct {
	Outcome  ValidationOutcome
	Mismatch UnchangedProofMismatch // set only for not-proven
	Detail   string
}

type ManagedGenerationValidator interface {
	ValidateManagedGenerationUnchanged(context.Context, ManagedGenerationValidationRequest) (ManagedGenerationValidationResult, error)
}
