package ingest

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/native_mounted_admission.yaml
var nativeMountedAdmissionYAML []byte

// The dependencies record real Codex parser handoffs, not synthetic parse results.
// Both entry points must admit bytes and report a nonzero, bounded peak.
type mountedNativeStore struct {
	*batchIndexStore
	recorder     *nativeBatchRecordingStore
	maxCandidate int64
}

var (
	_ NativeGenerationBatchStager    = (*mountedNativeStore)(nil)
	_ NativeGenerationBatchActivator = (*mountedNativeStore)(nil)
	_ NativeGenerationActivator      = (*mountedNativeStore)(nil)
)

func (s *mountedNativeStore) StageNativeGenerations(ctx context.Context, a []NativeGenerationActivation) ([]NativeGenerationStaged, []error) {
	for _, activation := range a {
		s.maxCandidate = max(s.maxCandidate, nativeCandidateWriteBytes(&NativeGenerationCandidate{Result: activation.Generation, Blobs: activation.Blobs}))
	}
	return s.recorder.StageNativeGenerations(ctx, a)
}

func (s *mountedNativeStore) ActivateStagedNativeGenerations(ctx context.Context, a []NativeGenerationActivation, h []NativeGenerationStaged) []NativeActivationResult {
	return s.recorder.ActivateStagedNativeGenerations(ctx, a, h)
}

func (s *mountedNativeStore) ActivateNativeGeneration(ctx context.Context, a NativeGenerationActivation) (ActivationOutcome, error) {
	return s.recorder.ActivateNativeGeneration(ctx, a)
}

func TestNativeMountedAdmission(t *testing.T) {
	var fixture struct {
		Transcript string `yaml:"transcript"`
	}
	if err := yaml.Unmarshal(nativeMountedAdmissionYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range loadNativeLaneFixtures(t, nativeMountedAdmissionYAML) {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			store := &mountedNativeStore{
				batchIndexStore: &batchIndexStore{serialIndexStore: serialIndexStore{entries: make(map[SessionID][]schema.SessionEntry)}},
				recorder:        &nativeBatchRecordingStore{sweepRecordingStore: &sweepRecordingStore{}},
			}
			profiler := &IndexProfiler{}
			p := &Pipeline{metricsStore: store, config: PipelineConfig{Parallelism: 3, IndexProfiler: profiler,
				Write: WriteConfig{StagedMemoryBytes: c.CapUnits, BatchSessions: 10, ActivationSessions: 10, BatchBytes: 1 << 20}}}
			var metas []indexedMeta
			for i := range c.Entries {
				sid, err := schema.NewSessionID(fmt.Sprintf("00000000-0000-4000-8000-%012d", i))
				if err != nil {
					t.Fatal(err)
				}
				metas = append(metas, indexedMeta{session: DiscoveredSession{SessionID: sid, Harness: HarnessCodex,
					SourcePath: ResolvedPath("/fixture/native.jsonl"), SourceFormat: SourceFormatJSONL},
					transcriptData: []byte(strings.ReplaceAll(fixture.Transcript, "SESSION_ID", string(sid)))})
			}
			prepareIndexParallelInputs(t, p, metas)
			p.indexers = map[Harness]TranscriptIndexer{HarnessCodex: NewCodexIndexer(p.fs)}
			p.harvesterVersions = NativeGenerationTargets(HarvesterVersionRegistry)
			done := make(chan struct{})
			var indexed []indexedMeta
			var logs []IndexLogEntry
			go func() {
				defer close(done)
				switch c.Mode {
				case "batch":
					indexed, logs, _ = p.indexBatch(ctx, metas, IndexOutcomeIndexed, "harvest")
				case "stream":
					ch := make(chan streamedIndexWork, len(metas))
					for _, meta := range metas {
						ch <- streamedIndexWork{meta: meta}
					}
					close(ch)
					indexed, logs, _ = p.indexLoop(ctx, ch, nil, nil, IndexOutcomeIndexed, "harvest", nil, nil)
				default:
					t.Errorf("unknown mounted admission mode %s", c.Mode)
				}
			}()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("production native handoff deadlocked under byte pressure")
			}
			for _, result := range indexed {
				if !result.indexed {
					t.Fatalf("native handoff failed: diagnostics=%v", p.diagnostics)
				}
			}
			if len(indexed) != len(metas) || store.maxCandidate == 0 {
				t.Fatalf("production path did not stage every native session: indexed=%d metas=%d max=%d logs=%+v profile=%+v", len(indexed), len(metas), store.maxCandidate, logs, profiler.Snapshot())
			}
			profile := profiler.Snapshot()
			if profile.StagedPeakBytes == 0 || profile.StagedPeakBytes > max(c.CapUnits, store.maxCandidate) {
				t.Fatalf("mounted peak=%d, cap=%d, largest candidate=%d", profile.StagedPeakBytes, c.CapUnits, store.maxCandidate)
			}
			if profile.ActivationSizes[1] != len(metas) || len(profile.ActivationSizes) != 1 {
				t.Fatalf("oversized candidates were not alone: %v", profile.ActivationSizes)
			}
		})
	}
}
