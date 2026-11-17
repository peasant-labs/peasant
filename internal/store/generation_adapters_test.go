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

// TestNativeDerivedDurationReproducesOutputHash proves that a native session's
// derived duration rereads as the computed value. 60034 ms is a duration whose
// minutes*60000 lands just below the integer, so a truncating writer stores
// 60033 ms and the stored output hash can never prove the metrics current.
func TestNativeDerivedDurationReproducesOutputHash(t *testing.T) {
	s, _ := openGenerationStore(t)
	const rawSession = "7b7b7b7b-7777-4777-8777-7b7b7b7b7b7b"
	seedGenerationSession(t, s, rawSession)
	sid, err := ingest.NewSessionID(rawSession)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	generation, blobs := buildTestGeneration(t, sid, "gen-duration-1", "text", "in", "out")
	if err := activateTestGeneration(t, s, generation, blobs); err != nil {
		t.Fatalf("activate generation: %v", err)
	}
	input, err := s.ReadMetricInput(ctx, sid, false)
	if err != nil {
		t.Fatalf("capture metric input: %v", err)
	}
	const durationMs = 60034
	minutes := float64(durationMs) / 60000
	computeVersion := 1
	computedAt := time.Now().UnixMilli()
	inputHash := input.DatabaseHash
	metrics := &ingest.SessionMetrics{
		SessionID: sid,
		InputHash: &inputHash,
		QualityMetrics: schema.QualityMetrics{
			ComputeVersion:  &computeVersion,
			ComputedAt:      &computedAt,
			DurationMinutes: &minutes,
		},
	}
	outputHash, err := ingest.MetricOutputHash(metrics)
	if err != nil {
		t.Fatal(err)
	}
	metrics.OutputHash = &outputHash
	if err := s.SaveMetricsForInput(ctx, input, metrics); err != nil {
		t.Fatalf("save metrics: %v", err)
	}
	stats, err := s.ReadCapturedStats(ctx, sid)
	if err != nil {
		t.Fatalf("read captured stats: %v", err)
	}
	if stats.DurationMs == nil || *stats.DurationMs != durationMs {
		t.Fatalf("captured duration = %v, want %d ms", stats.DurationMs, durationMs)
	}
	stored, err := s.GetMetrics(ctx, sid)
	if err != nil || stored == nil || stored.OutputHash == nil {
		t.Fatalf("reread metrics = %+v, %v", stored, err)
	}
	reread, err := ingest.MetricOutputHash(stored)
	if err != nil {
		t.Fatal(err)
	}
	if reread != *stored.OutputHash {
		t.Fatalf("reread output hash %s does not reproduce stored %s; duration %v, want %v", reread, *stored.OutputHash, stored.DurationMinutes, minutes)
	}
}
