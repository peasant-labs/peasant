package ingest

import (
	"context"
	_ "embed"
	"sync/atomic"
	"testing"

	"gopkg.in/yaml.v3"
)

// advisoryNativeStore has no staging capability, so it exercises true inline
// activation. The real store's byte threshold is tested in the store package.
type advisoryNativeStore struct {
	*sweepRecordingStore
	emitActivation  bool
	activationCalls atomic.Int32
}

// advisoryStagingStore adds optional prestaging without a prepared activator.
// Emission at each boundary is independent so neither reporter can mask a
// missing reporter at the other boundary.
type advisoryStagingStore struct {
	*advisoryNativeStore
	emitStage  bool
	stageCalls atomic.Int32
}

var (
	_ NativeGenerationStager    = (*advisoryStagingStore)(nil)
	_ NativeGenerationActivator = (*advisoryNativeStore)(nil)
	_ NativeGenerationActivator = (*advisoryStagingStore)(nil)
)

//go:embed testdata/write_advisory.yaml
var writeAdvisoryYAML []byte

type writeAdvisoryCase struct {
	Name                string `yaml:"name"`
	Stager              bool   `yaml:"stager"`
	EmitStage           bool   `yaml:"emit_stage"`
	EmitActivation      bool   `yaml:"emit_activation"`
	WantStageCalls      int32  `yaml:"want_stage_calls"`
	WantActivationCalls int32  `yaml:"want_activation_calls"`
}

func nativeWriteTestAdvisory(activation NativeGenerationActivation) DiagnosticEntry {
	return DiagnosticEntry{ErrorType: "oversize_write_advisory", Location: string(activation.Generation.Generation.Metadata.SessionID), Message: "no other writer running is recommended"}
}

func (s *advisoryStagingStore) StageNativeGeneration(ctx context.Context, activation NativeGenerationActivation) (NativeGenerationStaged, error) {
	s.stageCalls.Add(1)
	if s.emitStage {
		ReportWriteAdvisory(ctx, nativeWriteTestAdvisory(activation))
	}
	return benchStaged(activation.Generation.Generation.ID), nil
}

func (s *advisoryNativeStore) ActivateNativeGeneration(ctx context.Context, activation NativeGenerationActivation) (ActivationOutcome, error) {
	s.activationCalls.Add(1)
	if s.emitActivation {
		ReportWriteAdvisory(ctx, nativeWriteTestAdvisory(activation))
	}
	return s.sweepRecordingStore.ActivateNativeGeneration(ctx, activation)
}

func TestNativeFlushRetainsWriteAdvisory(t *testing.T) {
	t.Parallel()
	var fixtures struct {
		RequiredNames []string            `yaml:"required_names"`
		Cases         []writeAdvisoryCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(writeAdvisoryYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if present[c.Name] {
			t.Fatalf("duplicate advisory fixture %q", c.Name)
		}
		present[c.Name] = true
	}
	requireFixtureNames(t, "write advisory", fixtures.RequiredNames, present)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			fake := &advisoryNativeStore{sweepRecordingStore: &sweepRecordingStore{}, emitActivation: c.EmitActivation}
			staging := &advisoryStagingStore{advisoryNativeStore: fake, emitStage: c.EmitStage}
			var dependency MetricsStore = fake
			if c.Stager {
				dependency = staging
			}
			if _, hasStager := dependency.(NativeGenerationStager); hasStager != c.Stager {
				t.Fatalf("staging capability=%v, want %v", hasStager, c.Stager)
			}
			pipeline := &Pipeline{config: PipelineConfig{Parallelism: 1}, metricsStore: dependency}
			result := benchNativeResult(t, 0, 2)
			flush := pipeline.flushIndexParseResults(t.Context(), []indexParseResult{result}, IndexOutcomeIndexed, "harvest", nil)
			if got := staging.stageCalls.Load(); got != c.WantStageCalls {
				t.Fatalf("stage invocations=%d, want %d", got, c.WantStageCalls)
			}
			if got := fake.activationCalls.Load(); got != c.WantActivationCalls {
				t.Fatalf("inline activation invocations=%d, want %d", got, c.WantActivationCalls)
			}
			if len(flush.indexed) != 1 || !flush.indexed[0].indexed {
				t.Fatalf("flush must commit its candidate: %+v", flush.indexed)
			}
			want := nativeWriteTestAdvisory(NativeGenerationActivation{Generation: result.nativeCandidate.Result})
			got := pipeline.snapshotDiagnostics()
			if len(got) != 1 || got[0] != want {
				t.Fatalf("harvest diagnostics=%+v, want exactly one retained advisory %+v", got, want)
			}
		})
	}
}
