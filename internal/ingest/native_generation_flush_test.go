package ingest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	gatedLargeBlobs  = 40
	gatedSmallBlobs  = 4
	gatedSmallCount  = 8
	gatedFlushWithin = 30 * time.Second
)

// gatedNativeStore is a dependency fake for the real batch flush: preparing
// the large candidate blocks until the gate opens (or the context ends), and
// every activation records its commit and how many activations overlapped.
type gatedNativeStore struct {
	MetricsStore
	gate      chan struct{}
	committed chan string

	mu        sync.Mutex
	commits   map[string]int
	inFlight  int
	maxActive int
}

func newGatedNativeStore() *gatedNativeStore {
	return &gatedNativeStore{
		gate:      make(chan struct{}),
		committed: make(chan string, gatedSmallCount+1),
		commits:   map[string]int{},
	}
}

func (s *gatedNativeStore) IndexSessionEntryBatch(_ context.Context, writes []SessionEntryWrite) []SessionEntryWriteResult {
	results := make([]SessionEntryWriteResult, len(writes))
	for i, write := range writes {
		results[i] = SessionEntryWriteResult{SessionID: write.SessionID, Written: true}
	}
	return results
}

func (s *gatedNativeStore) StageNativeGeneration(ctx context.Context, activation NativeGenerationActivation) (NativeGenerationStaged, error) {
	if len(activation.Blobs) == gatedLargeBlobs {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return benchStaged(activation.Generation.Generation.ID), nil
}

func (s *gatedNativeStore) activate(activation NativeGenerationActivation) (ActivationOutcome, error) {
	id := activation.Generation.Generation.ID
	s.mu.Lock()
	s.inFlight++
	s.maxActive = max(s.maxActive, s.inFlight)
	s.mu.Unlock()
	time.Sleep(time.Millisecond)
	s.mu.Lock()
	s.inFlight--
	s.commits[id]++
	s.mu.Unlock()
	s.committed <- id
	return ActivationOutcome{Disposition: ActivationCommittedNow, CandidateID: id}, nil
}

func (s *gatedNativeStore) ActivateStagedNativeGeneration(_ context.Context, activation NativeGenerationActivation, _ NativeGenerationStaged) (ActivationOutcome, error) {
	return s.activate(activation)
}

func (s *gatedNativeStore) ActivateNativeGeneration(_ context.Context, activation NativeGenerationActivation) (ActivationOutcome, error) {
	return s.activate(activation)
}

func gatedNativeResults(t *testing.T) []indexParseResult {
	t.Helper()
	results := []indexParseResult{benchNativeResult(t, 0, gatedLargeBlobs)}
	for i := 1; i <= gatedSmallCount; i++ {
		results = append(results, benchNativeResult(t, i, gatedSmallBlobs))
	}
	return results
}

// TestNativeFlushStreamsCommitsPastABlockedCandidate proves the streaming
// shape on the real flush: while the large candidate is still preparing,
// every small candidate commits and reports progress; once it is released,
// every candidate has exactly one commit and one progress advance, and no two
// activations ever overlapped.
func TestNativeFlushStreamsCommitsPastABlockedCandidate(t *testing.T) {
	store := newGatedNativeStore()
	results := gatedNativeResults(t)
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 4}, metricsStore: store}
	var emits atomic.Int64
	done := make(chan indexWriteFlush, 1)
	go func() {
		done <- pipeline.flushIndexParseResultsWithProgress(context.Background(), results, IndexOutcomeIndexed, "test", nil, func() { emits.Add(1) })
	}()

	deadline := time.After(gatedFlushWithin)
	for i := 0; i < gatedSmallCount; i++ {
		select {
		case <-store.committed:
		case <-deadline:
			t.Fatalf("only %d small candidates committed while the large one was preparing", i)
		}
	}
	// The large candidate is still blocked, so its commit cannot have run.
	select {
	case id := <-store.committed:
		t.Fatalf("candidate %s committed while the large one was still blocked", id)
	default:
	}
	waitFor(t, func() bool { return emits.Load() == gatedSmallCount }, "progress for every small commit")

	close(store.gate)
	var flush indexWriteFlush
	select {
	case flush = <-done:
	case <-time.After(gatedFlushWithin):
		t.Fatal("flush did not finish after the large candidate was released")
	}
	if got := emits.Load(); got != int64(len(results)) {
		t.Fatalf("progress advances = %d, want %d", got, len(results))
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range results {
		if !flush.indexed[i].indexed {
			t.Fatalf("result %d was not reported indexed", i)
		}
	}
	if len(store.commits) != len(results) {
		t.Fatalf("committed candidates = %v, want %d distinct", store.commits, len(results))
	}
	for id, count := range store.commits {
		if count != 1 {
			t.Fatalf("candidate %s committed %d times, want once", id, count)
		}
	}
	if store.maxActive != 1 {
		t.Fatalf("overlapping activations = %d, want at most one at a time", store.maxActive)
	}
}

// TestNativeFlushCancelledWhileBlockedReturns cancels the run while the large
// candidate is preparing: the flush must return, and every result still gets
// exactly one progress advance.
func TestNativeFlushCancelledWhileBlockedReturns(t *testing.T) {
	store := newGatedNativeStore()
	results := gatedNativeResults(t)
	pipeline := &Pipeline{config: PipelineConfig{Parallelism: 4}, metricsStore: store}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var emits atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeline.flushIndexParseResultsWithProgress(ctx, results, IndexOutcomeIndexed, "test", nil, func() { emits.Add(1) })
	}()
	waitFor(t, func() bool { return emits.Load() == gatedSmallCount }, "progress for every small commit")
	cancel()
	select {
	case <-done:
	case <-time.After(gatedFlushWithin):
		t.Fatal("flush deadlocked after cancellation")
	}
	if got := emits.Load(); got != int64(len(results)) {
		t.Fatalf("progress advances = %d, want %d", got, len(results))
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(gatedFlushWithin)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
