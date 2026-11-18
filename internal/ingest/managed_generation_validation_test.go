package ingest_test

import (
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
)

func TestValidationOutcomeBoundary(t *testing.T) {
	for _, want := range ingest.AllValidationOutcomes {
		got, err := ingest.NewValidationOutcome(string(want))
		if err != nil || got != want {
			t.Fatalf("outcome %q: got %q, %v", want, got, err)
		}
	}
	const unknown = "unexpected-validation-value"
	if _, err := ingest.NewValidationOutcome(unknown); err == nil || !strings.Contains(err.Error(), unknown) {
		t.Fatalf("unknown outcome error = %v; want it to name %q", err, unknown)
	}
}

func TestManagedGenerationValidatorStub(t *testing.T) {
	// A nil store proves the stub cannot take a connection or writer lane.
	var s *store.Store
	result, err := s.ValidateManagedGenerationUnchanged(t.Context(), ingest.ManagedGenerationValidationRequest{})
	if err != nil || result.Outcome != ingest.ValidationUnsupported || result.Mismatch != ingest.UnchangedMismatchNone {
		t.Fatalf("zero-budget validation = %+v, %v", result, err)
	}
}
