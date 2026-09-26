package store

import (
	"context"
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

	// A replacement generation with byte-identical entries: only the installed
	// generation identity changes.
	second, secondBlobs := buildTestGeneration(t, sid, "gen-bound-2", "same text", "same in", "same out")
	if err := activateTestGeneration(t, s, second, secondBlobs); err != nil {
		t.Fatalf("activate replacement generation: %v", err)
	}
	if err := s.SaveMetricsForInput(ctx, input, metrics); err == nil {
		t.Fatal("a compute captured before the generation replacement saved over the newer generation")
	}
}
