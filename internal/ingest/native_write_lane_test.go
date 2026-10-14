package ingest

import (
	"context"
	_ "embed"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/native_batch_activation.yaml
var nativeBatchActivationYAML []byte

//go:embed testdata/native_staged_bytes.yaml
var nativeStagedBytesYAML []byte

//go:embed testdata/index_drain_flush.yaml
var indexDrainFlushYAML []byte

type nativeLaneCase struct {
	Name          string `yaml:"name"`
	Entries       []int  `yaml:"entries"`
	ActivationCap int    `yaml:"activation_cap"`
	ByteCapUnits  int64  `yaml:"byte_cap_units"`
	CapUnits      int64  `yaml:"cap_units"`
	Refuse        *int   `yaml:"refuse"`
	Skip          *int   `yaml:"skip"`
	Cancel        bool   `yaml:"cancel"`
	Mode          string `yaml:"mode"`
	Batches       []int  `yaml:"batches"`
}

func loadNativeLaneFixtures(t *testing.T, data []byte) []nativeLaneCase {
	t.Helper()
	var f struct {
		RequiredNames []string         `yaml:"required_names"`
		Cases         []nativeLaneCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	present := make(map[string]bool)
	for _, c := range f.Cases {
		if present[c.Name] {
			t.Fatalf("duplicate fixture %s", c.Name)
		}
		present[c.Name] = true
	}
	requireFixtureNames(t, "native write lane", f.RequiredNames, present)
	return f.Cases
}

type nativeBatchRecordingStore struct {
	*sweepRecordingStore
	stageBatches, activationBatches []int
	refuse, skip                    *int
	gate                            *stagedBytesGate
	maxObserved                     int64
}

var (
	_ NativeGenerationBatchStager    = (*nativeBatchRecordingStore)(nil)
	_ NativeGenerationBatchActivator = (*nativeBatchRecordingStore)(nil)
)

func (s *nativeBatchRecordingStore) StageNativeGenerations(_ context.Context, a []NativeGenerationActivation) ([]NativeGenerationStaged, []error) {
	s.stageBatches = append(s.stageBatches, len(a))
	if s.gate != nil {
		s.gate.mu.Lock()
		s.maxObserved = max(s.maxObserved, s.gate.used)
		s.gate.mu.Unlock()
	}
	h := make([]NativeGenerationStaged, len(a))
	for i := range a {
		h[i] = benchStaged(a[i].Generation.Generation.ID)
	}
	return h, make([]error, len(a))
}

func (s *nativeBatchRecordingStore) ActivateStagedNativeGenerations(_ context.Context, a []NativeGenerationActivation, _ []NativeGenerationStaged) []NativeActivationResult {
	s.activationBatches = append(s.activationBatches, len(a))
	r := make([]NativeActivationResult, len(a))
	for i, a := range a {
		id := a.Generation.Generation.ID
		r[i].Outcome = ActivationOutcome{Disposition: ActivationCommittedNow, CandidateID: id}
		if s.refuse != nil && id == fmt.Sprintf("g_bench_%d", *s.refuse) {
			r[i].Outcome.Disposition = ActivationNotCommitted
			r[i].Err = fmt.Errorf("store: fixture refuses candidate %s; prior authority unchanged", id)
		}
		if s.skip != nil && id == fmt.Sprintf("g_bench_%d", *s.skip) {
			r[i].Outcome.Disposition = ActivationSkipped
		}
	}
	return r
}

func TestNativeGenerationBatchActivation(t *testing.T) {
	for _, c := range loadNativeLaneFixtures(t, nativeBatchActivationYAML) {
		t.Run(c.Name, func(t *testing.T) {
			store := &nativeBatchRecordingStore{sweepRecordingStore: &sweepRecordingStore{}, refuse: c.Refuse, skip: c.Skip}
			var results []indexParseResult
			for i, n := range c.Entries {
				results = append(results, benchNativeResult(t, i, n))
			}
			unit := nativeCandidateWriteBytes(results[0].nativeCandidate)
			p := &Pipeline{metricsStore: store, config: PipelineConfig{Parallelism: 3, Write: WriteConfig{ActivationSessions: c.ActivationCap, BatchBytes: unit * c.ByteCapUnits}}}
			flush := p.flushIndexParseResults(t.Context(), results, IndexOutcomeIndexed, "harvest", nil)
			if !reflect.DeepEqual(store.activationBatches, c.Batches) {
				t.Fatalf("activation batches=%v, want %v", store.activationBatches, c.Batches)
			}
			for i, im := range flush.indexed {
				want := c.Refuse == nil || i != *c.Refuse
				if im.indexed != want {
					t.Fatalf("session %d indexed=%v, want %v", i, im.indexed, want)
				}
				if c.Skip != nil && i == *c.Skip && flush.logEntries[i].Outcome != IndexOutcomeSkipped {
					t.Fatalf("skip outcome=%v", flush.logEntries[i])
				}
			}
		})
	}
}

func TestNativeStagedBytesCap(t *testing.T) {
	for _, c := range loadNativeLaneFixtures(t, nativeStagedBytesYAML) {
		t.Run(c.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var results []indexParseResult
			var maxWeight int64
			for i, n := range c.Entries {
				r := benchNativeResult(t, i, n)
				maxWeight = max(maxWeight, nativeCandidateWriteBytes(r.nativeCandidate))
				results = append(results, r)
			}
			cap := nativeCandidateWriteBytes(results[0].nativeCandidate) * c.CapUnits
			gate := newStagedBytesGate(cap)
			store := &nativeBatchRecordingStore{sweepRecordingStore: &sweepRecordingStore{}, refuse: c.Refuse, gate: gate}
			p := &Pipeline{metricsStore: store, config: PipelineConfig{Parallelism: 3, Write: WriteConfig{BatchBytes: cap, BatchSessions: 10, ActivationSessions: 10}}}
			ch := make(chan indexParseResult)
			var wg sync.WaitGroup
			for _, r := range results {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ch <- p.admitNativeResult(ctx, r, gate)
				}()
			}
			go func() {
				wg.Wait()
				close(ch)
			}()
			if c.Cancel {
				cancel()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				drainIndexParseResultsWithGate(ch, nil, p.writeConfig(), func(batch []indexParseResult) {
					p.flushIndexParseResults(ctx, batch, IndexOutcomeIndexed, "harvest", nil)
				}, gate.blocked)
			}()
			select {
			case <-done:
			case <-time.After(6 * time.Second):
				t.Fatal("byte admission deadlocked; release must wake blocked producers")
			}
			if got := gate.Peak(); got > max(cap, maxWeight) {
				t.Fatalf("peak=%d exceeds cap=%d and single oversized=%d", got, cap, maxWeight)
			}
			gate.mu.Lock()
			used := gate.used
			gate.mu.Unlock()
			if used != 0 {
				t.Fatalf("completed/refused/cancelled candidates leaked %d admitted bytes", used)
			}
			if !c.Cancel && store.maxObserved == 0 {
				t.Fatal("stager observed no admitted bytes")
			}
		})
	}
}

func TestIndexDrainFlushInterval(t *testing.T) {
	for _, c := range loadNativeLaneFixtures(t, indexDrainFlushYAML) {
		t.Run(c.Name, func(t *testing.T) {
			ch := make(chan indexParseResult, 3)
			ticks := make(chan time.Time, 1)
			armed := make(chan struct{}, 3)
			flushed := make(chan int, 3)
			done := make(chan struct{})
			profiler := &IndexProfiler{}
			cfg := WriteConfig{BatchSessions: 3, BatchBytes: 1 << 20, FlushIntervalMs: 50}.WithDefaults(1)
			newTimer := func(d time.Duration) indexDrainTimer {
				if d != cfg.FlushInterval() {
					t.Errorf("interval=%s, want %s", d, cfg.FlushInterval())
				}
				armed <- struct{}{}
				return indexDrainTimer{c: ticks, stop: func() {}}
			}
			if c.Mode == "filling" {
				for range 3 {
					ch <- indexParseResult{}
				}
				ticks <- time.Now()
				close(ch)
			} else {
				ch <- indexParseResult{}
			}
			go func() {
				defer close(done)
				drainIndexWithTimer(ch, nil, cfg, func(b []indexParseResult) {
					flushed <- len(b)
				}, nil, newTimer, profiler)
			}()
			select {
			case <-armed:
			case <-time.After(time.Second):
				t.Fatal("first admission did not arm flush timer")
			}
			var got []int
			switch c.Mode {
			case "above":
				ticks <- time.Now()
				select {
				case n := <-flushed:
					got = append(got, n)
				case <-time.After(time.Second):
					t.Fatal("interval did not flush partial batch")
				}
				ch <- indexParseResult{}
				close(ch)
			case "below":
				select {
				case n := <-flushed:
					t.Fatalf("partial batch flushed before interval: %d", n)
				default:
				}
				ch <- indexParseResult{}
				close(ch)
			case "filling":
			default:
				t.Fatalf("unknown drain mode %s", c.Mode)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("closed input did not drain")
			}
			close(flushed)
			for n := range flushed {
				got = append(got, n)
			}
			if !reflect.DeepEqual(got, c.Batches) {
				t.Fatalf("batches=%v, want %v", got, c.Batches)
			}
			profile := profiler.Snapshot()
			if c.Mode == "filling" && profile.FlushWaitCount != 0 {
				t.Fatal("ready input was counted as a partial-batch wait")
			}
			if c.Mode == "above" && profile.FlushWaitCount == 0 {
				t.Fatal("observed timer wait was not recorded")
			}
		})
	}
}
