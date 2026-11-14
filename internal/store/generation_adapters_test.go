package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

// TestGenerationBoundComputeRefusedAcrossActivation proves that a metrics
// computation bound to one managed generation cannot save its metrics and title
// after a replacement generation is activated, even when the captured entries
// and derived values are identical: the active generation is part of the
// captured input identity.
func TestGenerationBoundComputeRefusedAcrossActivation(t *testing.T) {
	s, _ := openGenerationStore(t)
	const rawSession = "7a7a7a7a-7777-4777-8777-7a7a7a7a7a7a"
	seedGenerationSession(t, s, rawSession)
	sid, err := ingest.NewSessionID(rawSession)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, firstBlobs := buildTestGeneration(t, sid, "gen-bound-1", "same text", "same in", "same out")
	if err := activateTestGeneration(t, s, first, firstBlobs); err != nil {
		t.Fatalf("activate first generation: %v", err)
	}
	input, err := s.ReadMetricInput(ctx, sid, false)
	if err != nil {
		t.Fatalf("capture metric input: %v", err)
	}
	if input.GenerationID != "gen-bound-1" {
		t.Fatalf("captured metric input generation = %q, want gen-bound-1", input.GenerationID)
	}

	computeVersion := 1
	computedAt := time.Now().UnixMilli()
	inputHash := input.DatabaseHash
	metrics := &ingest.SessionMetrics{
		SessionID: sid,
		InputHash: &inputHash,
		QualityMetrics: schema.QualityMetrics{
			ComputeVersion: &computeVersion,
			ComputedAt:     &computedAt,
		},
	}
	outputHash, err := ingest.MetricOutputHash(metrics)
	if err != nil {
		t.Fatal(err)
	}
	metrics.OutputHash = &outputHash

	// A replacement generation with different entries: the installed
	// generation identity changes, so compute bound to the first input
	// must refuse. (Byte-identical content under a fresh identifier would
	// skip instead of replacing, keeping the first generation active.)
	second, secondBlobs := buildTestGeneration(t, sid, "gen-bound-2", "changed text", "changed in", "changed out")
	if err := activateTestGeneration(t, s, second, secondBlobs); err != nil {
		t.Fatalf("activate replacement generation: %v", err)
	}
	if err := s.SaveMetricsForInput(ctx, input, metrics); err == nil {
		t.Fatal("a compute captured before the generation replacement saved over the newer generation")
	}
}

// TestManagedGenerationRefreshAdapterSurfacesRefusalAndStaleProof pins the
// boundary the pipeline classifies: the ingest-owned request translates to the
// store's immutable request, and the store's refusal and stale-capture errors
// reach the caller unchanged. A caller that cannot see them would read a
// refused refresh as a successful settle instead of falling back to the parse.
func TestManagedGenerationRefreshAdapterSurfacesRefusalAndStaleProof(t *testing.T) {
	s, _ := openGenerationStore(t)
	defer func() { _ = s.Close() }()
	ctx := t.Context()
	sourceMetadata, _, expected := seedManagedRefreshSource(t, s)
	refreshed := refreshedManagedMetadata(sourceMetadata)

	refused := refreshed
	refused.ContentHash = strings.Repeat("f", 64)
	_, err := s.RefreshManagedGenerationWithoutParse(ctx, ingest.ManagedGenerationRefreshRequest{
		SessionID:     managedRefreshTestSession,
		Metadata:      refused,
		ExpectedState: expected,
	})
	var refusal *ManagedGenerationRefreshError
	if !errors.As(err, &refusal) {
		t.Fatalf("refused refresh error = %v, want a *ManagedGenerationRefreshError", err)
	}
	if !errors.Is(err, ErrManagedGenerationRefreshRefused) {
		t.Fatalf("refused refresh error = %v, want errors.Is ErrManagedGenerationRefreshRefused", err)
	}
	if refusal.Reason != ManagedGenerationRefreshIdentityChanged {
		t.Fatalf("refusal reason = %q, want identity-changed", refusal.Reason)
	}

	stale := *expected
	staleAt := time.Now().UnixMilli()
	stale.IndexedAt = &staleAt
	_, err = s.RefreshManagedGenerationWithoutParse(ctx, ingest.ManagedGenerationRefreshRequest{
		SessionID:     managedRefreshTestSession,
		Metadata:      refreshed,
		ExpectedState: &stale,
	})
	var staleWork *ingest.StaleIndexWorkError
	if !errors.As(err, &staleWork) {
		t.Fatalf("stale refresh error = %v, want a *ingest.StaleIndexWorkError", err)
	}
}
