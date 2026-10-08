package ingest

import (
	"context"
	"testing"
)

// advisoryNativeStore models a store emitting the same advisory at staging
// and at inline activation. The real store's byte threshold is tested in the
// store package; this test protects the harvest report's production wiring.
type advisoryNativeStore struct {
	*sweepRecordingStore
}

var (
	_ NativeGenerationStager    = (*advisoryNativeStore)(nil)
	_ NativeGenerationActivator = (*advisoryNativeStore)(nil)
)

func nativeWriteTestAdvisory(activation NativeGenerationActivation) DiagnosticEntry {
	return DiagnosticEntry{ErrorType: "oversize_write_advisory", Location: string(activation.Generation.Generation.Metadata.SessionID), Message: "no other writer running is recommended"}
}

func (s *advisoryNativeStore) StageNativeGeneration(ctx context.Context, activation NativeGenerationActivation) (NativeGenerationStaged, error) {
	ReportWriteAdvisory(ctx, nativeWriteTestAdvisory(activation))
	return benchStaged(activation.Generation.Generation.ID), nil
}

func (s *advisoryNativeStore) ActivateNativeGeneration(ctx context.Context, activation NativeGenerationActivation) (ActivationOutcome, error) {
	ReportWriteAdvisory(ctx, nativeWriteTestAdvisory(activation))
	return s.sweepRecordingStore.ActivateNativeGeneration(ctx, activation)
}

func TestNativeFlushRetainsWriteAdvisory(t *testing.T) {
	t.Parallel()
	fake := &advisoryNativeStore{sweepRecordingStore: &sweepRecordingStore{}}
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: fake}
	result := benchNativeResult(t, 0, 2)
	pipeline.flushIndexParseResults(t.Context(), []indexParseResult{result}, IndexOutcomeIndexed, "harvest", nil)
	want := nativeWriteTestAdvisory(NativeGenerationActivation{Generation: result.nativeCandidate.Result})
	got := pipeline.snapshotDiagnostics()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("harvest diagnostics=%+v, want exactly one retained advisory %+v", got, want)
	}
}
