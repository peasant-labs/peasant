package ingest

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestBatchFlushRefusalsEmitProgress covers the two batch refusals that store
// an error outcome without a store write: parsed output without a captured
// input, and an assessment that cannot become a store write. Each must still
// report exactly one progress advance.
func TestBatchFlushRefusalsEmitProgress(t *testing.T) {
	withoutInput := benchNativeResult(t, 0, gatedSmallBlobs)
	withoutInput.nativeCandidate = nil
	withoutInput.input = nil
	refusedAssessment := benchNativeResult(t, 1, gatedSmallBlobs)
	refusedAssessment.nativeCandidate = nil
	refusedAssessment.assessmentReady = true // the zero assessment has unknown coverage
	results := []indexParseResult{withoutInput, refusedAssessment}

	pipeline := &Pipeline{metricsStore: newGatedNativeStore()}
	var emits atomic.Int64
	flush := pipeline.flushIndexParseResultsWithProgress(context.Background(), results, IndexOutcomeIndexed, "test", nil, func() { emits.Add(1) })
	if got := emits.Load(); got != int64(len(results)) {
		t.Fatalf("progress advances = %d, want %d", got, len(results))
	}
	for i := range results {
		if flush.indexed[i].indexed || flush.logEntries[i].Outcome != IndexOutcomeError {
			t.Fatalf("result %d = (indexed %v, outcome %q), want a refused error outcome", i, flush.indexed[i].indexed, flush.logEntries[i].Outcome)
		}
	}
}
