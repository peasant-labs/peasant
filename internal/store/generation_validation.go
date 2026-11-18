package store

import (
	"context"

	"github.com/peasant-labs/peasant/internal/ingest"
)

var _ ingest.ManagedGenerationValidator = (*Store)(nil)

// ValidateManagedGenerationUnchanged declines validation without accessing the store.
func (s *Store) ValidateManagedGenerationUnchanged(context.Context, ingest.ManagedGenerationValidationRequest) (ingest.ManagedGenerationValidationResult, error) {
	return ingest.ManagedGenerationValidationResult{
		Outcome: ingest.ValidationUnsupported,
		Detail:  "this build has no parallel validator; the existing refresh fallback runs",
	}, nil
}
